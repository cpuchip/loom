// Command loom drives coding-agent CLIs (Claude Code, agy/Gemini) as workers.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cpuchip/loom"
)

// emitReply writes one turn's result: as a single JSON line to stdout when jsonOut
// (for programmatic/subprocess callers — the "pull" channel), else the answer on
// stdout with cost/error meta on stderr. Session-resume hints are left to the caller.
func emitReply(r loom.Reply, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	fmt.Println(r.Text)
	if r.Err != "" {
		fmt.Fprintf(os.Stderr, "[%s: %s]\n", r.Backend, r.Err)
	}
	if r.CostUSD > 0 {
		fmt.Fprintf(os.Stderr, "[%s $%.4f]\n", r.Backend, r.CostUSD)
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "chat":
		err = cmdChat(os.Args[2:])
	case "panel":
		err = cmdPanel(os.Args[2:])
	case "review":
		err = cmdReview(os.Args[2:])
	case "duo":
		err = cmdDuo(os.Args[2:])
	case "race":
		err = cmdRace(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "await":
		err = cmdAwait(os.Args[2:])
	case "sessions":
		err = cmdSessions(os.Args[2:])
	case "runs":
		err = cmdRuns(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "pair":
		err = cmdPair(os.Args[2:])
	case "enroll":
		err = cmdEnroll(os.Args[2:])
	case "agents":
		for name := range loom.Backends() {
			fmt.Println(name)
		}
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loom: "+err.Error())
		os.Exit(1)
	}
}

func pickBackend(name string) (loom.Backend, error) {
	b, ok := loom.Backends()[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (try: loom agents)", name)
	}
	return b, nil
}

// sessFlags is the shared flag set for the session commands (run, chat) — the
// backend selector plus the whole claude-configuration surface.
type sessFlags struct {
	agent, model, dir, clone, remote, resume *string
	mcpConfig, allowedTools, permMode        *string
	sysPromptFile, claudeHome, skills        *string
	connect, token, session, peer            *string
	events, isolate, skipPerms, json         *bool
	consult                                  *bool
}

func addSessionFlags(fs *flag.FlagSet) *sessFlags {
	return &sessFlags{
		agent:         fs.String("agent", "claude", "backend (loom agents)"),
		model:         fs.String("model", "", "model override"),
		dir:           fs.String("dir", "", "working dir (the agent's cwd / the corpus it works)"),
		events:        fs.Bool("events", false, "stream tool calls + thinking to stderr"),
		isolate:       fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)"),
		remote:        fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)"),
		resume:        fs.String("resume", "", "resume a prior claude session by id"),
		mcpConfig:     fs.String("mcp-config", "", "claude --mcp-config: wire in MCP server(s) from a JSON file (the hinge into pg-ai-stewards)"),
		allowedTools:  fs.String("allowed-tools", "", "claude --allowed-tools: scope which tools (incl. MCP) the agent may call"),
		permMode:      fs.String("permission-mode", "", "claude --permission-mode (e.g. acceptEdits, plan)"),
		skipPerms:     fs.Bool("skip-permissions", false, "claude --dangerously-skip-permissions (headless; safe only INSIDE --isolate)"),
		consult:       fs.Bool("consult", false, "read-only consult: inject an answer-don't-act directive so a QUESTION drive doesn't sprawl into edits/commits/journaling"),
		sysPromptFile: fs.String("system-prompt-file", "", "claude --append-system-prompt-file: inject instructions"),
		claudeHome:    fs.String("claude-home", "", "(--isolate) host dir mounted as the container's ~/.claude: skills/instructions/settings/MCP + PERSISTED sessions (enables resume+isolate)"),
		skills:        fs.String("skills", "", "source dir of authored skills (each a <name>/SKILL.md folder) — mirrored into the skill dir the TARGET backend reads (claude→.claude/skills, codex→.agents/skills, copilot/opencode→.claude/skills) within the workdir; local only"),
		connect:       fs.String("connect", "", "drive a remote `loom serve` over websocket (ws://host:port, or wss://host:port for pinned mTLS) — the --agent/opts are opened THERE"),
		token:         fs.String("token", "", "auth token for --connect"),
		peer:          fs.String("peer", "", "(--connect wss://) the pinned peer name you paired with (loom pair)"),
		session:       fs.String("session", "", "(--connect) reattach a warm resident by this stable NAME — a second use reuses the live process, no respawn/cold-read"),
		json:          fs.Bool("json", false, "emit the Reply as JSON to stdout (for programmatic/subprocess callers)"),
	}
}

func addCloneFlag(fs *flag.FlagSet, sf *sessFlags) {
	sf.clone = fs.String("clone", "", "git URL to clone before starting (uses --dir, or a fresh temp dir)")
}

// chooseBackend routes to the ws transport when --connect is set (the remote server
// opens --agent with these opts), else to a local backend by name.
func chooseBackend(sf *sessFlags) (loom.Backend, error) {
	if *sf.connect != "" {
		return makeConnectBackend(*sf.connect, *sf.token, *sf.agent, *sf.session, *sf.peer, false)
	}
	return pickBackend(*sf.agent)
}

// makeConnectBackend builds the ws transport, loading this node's identity + pin store
// when the URL is wss:// (pinned mTLS). A wss:// URL requires --peer — the name of the
// pinned server you paired with — so we dial into a KNOWN cert, never trust-on-first-use.
// A ws:// URL ignores all of that (plaintext on the encrypted mesh).
func makeConnectBackend(url, token, agent, session, peer string, attachOnly bool) (loom.ConnectBackend, error) {
	cb := loom.ConnectBackend{URL: url, Token: token, Agent: agent, SessionName: session, AttachOnly: attachOnly, Peer: peer}
	if strings.HasPrefix(url, "wss://") {
		if peer == "" {
			return cb, fmt.Errorf("--connect wss:// requires --peer <name> (the pinned peer you paired with via `loom pair`)")
		}
		id, err := loom.LoadOrCreateIdentity("")
		if err != nil {
			return cb, fmt.Errorf("load identity: %w", err)
		}
		pins, err := loom.LoadPinStore("")
		if err != nil {
			return cb, fmt.Errorf("load pins: %w", err)
		}
		cb.Identity = id
		cb.Pins = pins
	}
	return cb, nil
}

func (sf *sessFlags) opts() loom.SessionOpts {
	return loom.SessionOpts{
		Workdir: *sf.dir, Model: *sf.model, Isolate: *sf.isolate, Remote: *sf.remote, Resume: *sf.resume,
		MCPConfig: *sf.mcpConfig, AllowedTools: *sf.allowedTools, PermissionMode: *sf.permMode,
		SkipPermissions: *sf.skipPerms, SystemPromptFile: *sf.sysPromptFile, ClaudeHome: *sf.claudeHome,
		Consult: *sf.consult, SkillsDir: *sf.skills,
	}
}

// resolveClone prepares the local workdir before the backend is opened. A clone cannot
// be paired with --remote: git would run here while the agent works somewhere else.
func resolveClone(cloneURL, dir, remote string, stderr io.Writer) (string, error) {
	if cloneURL == "" {
		return dir, nil
	}
	if remote != "" {
		return "", fmt.Errorf("--clone cannot be used with --remote")
	}

	tempDir := dir == ""
	if tempDir {
		var err error
		dir, err = os.MkdirTemp("", "loom-clone-")
		if err != nil {
			return "", fmt.Errorf("make clone temp dir: %w", err)
		}
	} else if err := requireEmptyDir(dir); err != nil {
		return "", err
	}

	output, err := exec.Command("git", "clone", cloneURL, dir).CombinedOutput()
	if err != nil {
		if tempDir {
			_ = os.RemoveAll(dir)
		}
		return "", fmt.Errorf("git clone %q %q: %w: %s", cloneURL, dir, err, strings.TrimSpace(string(output)))
	}
	if tempDir {
		fmt.Fprintf(stderr, "[cloned into %s]\n", dir)
	}
	return dir, nil
}

// requireEmptyDir refuses to hand git a destination containing user work. Git itself
// creates a missing destination, while an existing empty one is safe to populate.
func requireEmptyDir(dir string) error {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect clone destination %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("clone destination %q exists and is not an empty directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect clone destination %q: %w", dir, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("clone destination %q exists and is not empty", dir)
	}
	return nil
}

func (sf *sessFlags) resolveClone(stderr io.Writer) error {
	dir, err := resolveClone(*sf.clone, *sf.dir, *sf.remote, stderr)
	if err != nil {
		return err
	}
	*sf.dir = dir
	return nil
}

// run: one-shot prompt → single reply.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	sf := addSessionFlags(fs)
	addCloneFlag(fs, sf)
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("run: need a prompt")
	}
	if err := sf.resolveClone(os.Stderr); err != nil {
		return err
	}
	// Lifecycle durability (the 2026-07-18 incident fix): every run gets a streamed log,
	// a crash-legible manifest, and a completion sentinel under LOOM_HOME/runs/<id>/. A
	// recorder failure is non-fatal — the worker still runs, just without durability.
	rec, recErr := newRunRecorder(os.Args, runCwd(*sf.dir), *sf.agent, *sf.model)
	if recErr != nil {
		fmt.Fprintf(os.Stderr, "[loom run: lifecycle recorder unavailable: %v]\n", recErr)
	} else {
		fmt.Fprintln(os.Stderr, rec.startedLine())
	}
	b, err := chooseBackend(sf)
	if err != nil {
		return err
	}
	// Record child PIDs as the backend spawns them, so a supervisor can confirm a killed
	// wrapper took its child with it. Cleared on return (a `loom run` drives one run).
	if rec != nil {
		loom.SetChildSpawnHook(rec.addChildPID)
		defer loom.SetChildSpawnHook(nil)
	}
	sess, err := b.Open(context.Background(), sf.opts())
	if err != nil {
		return err
	}
	defer sess.Close()
	var reply loom.Reply
	var runErr error
	if rec != nil {
		// Runs from a defer → covers graceful returns AND panics. A hard TerminateProcess
		// skips it on purpose: the un-finished manifest + stale heartbeat is the evidence.
		defer func() { rec.finish(runErr, reply) }()
	}
	var recEvent func(loom.Event)
	if rec != nil {
		recEvent = rec.logEvent
	}
	reply, runErr = sendTurnTee(sess, prompt, *sf.events, recEvent)
	if runErr != nil {
		return runErr
	}
	if err := emitReply(reply, *sf.json); err != nil {
		return err
	}
	if !*sf.json && reply.SessionID != "" {
		fmt.Fprintf(os.Stderr, "[session %s — resume: loom run --resume %s ...]\n", reply.SessionID, reply.SessionID)
	}
	return nil
}

// runCwd resolves the recorded working directory: the explicit --dir, else the wrapper's
// own cwd (what the agent actually inherits).
func runCwd(dir string) string {
	if dir != "" {
		return dir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// chat: persistent multi-turn session — one message per stdin line.
func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	sf := addSessionFlags(fs)
	addCloneFlag(fs, sf)
	_ = fs.Parse(args)
	if err := sf.resolveClone(os.Stderr); err != nil {
		return err
	}
	b, err := chooseBackend(sf)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), sf.opts())
	if err != nil {
		return err
	}
	defer sess.Close()
	fmt.Fprintf(os.Stderr, "loom chat — %s — one message per line, Ctrl-D to end\n", *sf.agent)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		r, err := sendTurn(sess, line, *sf.events)
		if err != nil {
			return err
		}
		if err := emitReply(r, *sf.json); err != nil {
			return err
		}
	}
	if id := sess.SessionID(); !*sf.json && id != "" {
		fmt.Fprintf(os.Stderr, "[session %s — resume: loom chat --resume %s]\n", id, id)
	}
	return in.Err()
}

// send: reattach (or first-open) a warm resident by name over --connect and send one
// turn. --detach starts the turn and returns a turn-id immediately (fetch the reply
// later with `loom await`) — so a minutes-long turn need not pin this process. Without
// --detach it streams + prints the reply like `run`, but leaves the resident warm.
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	sf := addSessionFlags(fs)
	detach := fs.Bool("detach", false, "start the turn detached — return a turn-id at once, fetch the reply with `loom await`")
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("send: need a message")
	}
	if *sf.connect == "" {
		return fmt.Errorf("send: --connect ws://host:port is required (send drives a `loom serve`)")
	}
	if *sf.session == "" {
		return fmt.Errorf("send: --session <name> is required (the warm resident to reattach)")
	}
	b, err := chooseBackend(sf)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), sf.opts())
	if err != nil {
		return err
	}
	defer sess.Close() // named → leaves the resident warm for the next drive
	if *detach {
		ds, ok := sess.(loom.DetachSession)
		if !ok {
			return fmt.Errorf("send: --detach needs --connect (a serve session)")
		}
		id, err := ds.SendDetached(context.Background(), prompt)
		if err != nil {
			return err
		}
		if *sf.json {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"turn_id": id, "status": "running"})
		}
		fmt.Println(id)
		fmt.Fprintf(os.Stderr, "[detached turn %d — fetch: loom await --connect %s --token … --session %s --turn %d]\n", id, *sf.connect, *sf.session, id)
		return nil
	}
	r, err := sendTurn(sess, prompt, *sf.events)
	if err != nil {
		return err
	}
	return emitReply(r, *sf.json)
}

// await: fetch a detached (or dropped) turn's reply from a warm resident's reply ring.
// It reattaches by name WITHOUT spawning (attach-only), so polling never creates a
// session. --last-reply fetches the most recent turn when the turn-id isn't known.
func cmdAwait(args []string) error {
	fs := flag.NewFlagSet("await", flag.ExitOnError)
	connect := fs.String("connect", "", "the `loom serve` endpoint (ws://host:port, or wss:// for pinned mTLS)")
	token := fs.String("token", "", "auth token for --connect")
	peer := fs.String("peer", "", "(wss://) the pinned peer name you paired with")
	agent := fs.String("agent", "claude", "backend the session runs on (matches the original open)")
	session := fs.String("session", "", "the warm resident's name")
	turn := fs.Int64("turn", 0, "the turn-id returned by `loom send --detach`")
	lastReply := fs.Bool("last-reply", false, "fetch the most recent turn (when the turn-id isn't known)")
	timeout := fs.Int("timeout", 30, "seconds to block waiting for the turn to finish before reporting it's still running")
	jsonOut := fs.Bool("json", false, "emit the Reply as JSON to stdout")
	_ = fs.Parse(args)
	if *connect == "" || *session == "" {
		return fmt.Errorf("await: --connect and --session are required")
	}
	if *turn == 0 && !*lastReply {
		return fmt.Errorf("await: need --turn <id> or --last-reply")
	}
	b, err := makeConnectBackend(*connect, *token, *agent, *session, *peer, true)
	if err != nil {
		return err
	}
	sess, err := b.Open(context.Background(), loom.SessionOpts{})
	if err != nil {
		return err
	}
	defer sess.Close() // named → leaves the resident warm
	ds, ok := sess.(loom.DetachSession)
	if !ok {
		return fmt.Errorf("await: session does not support await (needs --connect)")
	}
	// The server bounds a single await block (awaitMax); a --timeout beyond that is
	// honored HERE by looping bounded server awaits until the deadline — so
	// `--timeout 1800` is one command that blocks up to 30 minutes, not a poll dance
	// the caller has to script (the field lesson from driving a 17-turn work order).
	deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		r, running, err := ds.Await(context.Background(), *turn, *lastReply, remain)
		if err != nil {
			return err
		}
		if !running {
			return emitReply(r, *jsonOut)
		}
		// server clamped its block and the turn is still going — loop until OUR deadline
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"turn_id": *turn, "status": "running"})
	}
	fmt.Fprintf(os.Stderr, "[turn still running — poll again: loom await … --turn %d]\n", *turn)
	return nil
}

// sessions: list the warm residents a `loom serve` holds — name, backend, frozen opts,
// idle time, last turn-id — so you know what to reattach.
func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	connect := fs.String("connect", "", "the `loom serve` endpoint (ws://host:port, or wss:// for pinned mTLS)")
	token := fs.String("token", "", "auth token for --connect")
	peer := fs.String("peer", "", "(wss://) the pinned peer name you paired with")
	jsonOut := fs.Bool("json", false, "emit the list as JSON to stdout")
	_ = fs.Parse(args)
	if *connect == "" {
		return fmt.Errorf("sessions: --connect ws://host:port is required")
	}
	b, err := makeConnectBackend(*connect, *token, "", "", *peer, false)
	if err != nil {
		return err
	}
	infos, err := b.Sessions(context.Background())
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(infos)
	}
	if len(infos) == 0 {
		fmt.Println("(no resident sessions)")
		return nil
	}
	for _, in := range infos {
		name := in.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Printf("%s\t%s\tidle=%ds\tlast_turn=%d", name, in.Backend, in.IdleSeconds, in.LastTurnID)
		if in.Model != "" {
			fmt.Printf("\tmodel=%s", in.Model)
		}
		if in.PermissionMode != "" {
			fmt.Printf("\tperm=%s", in.PermissionMode)
		}
		if in.Dir != "" {
			fmt.Printf("\tdir=%s", in.Dir)
		}
		if in.KeepAlive {
			fmt.Printf("\tkeep-alive")
		}
		fmt.Println()
	}
	return nil
}

// panel: fan one prompt across several agents (the council pattern).
func cmdPanel(args []string) error {
	fs := flag.NewFlagSet("panel", flag.ExitOnError)
	agents := fs.String("agents", "claude", "comma-separated backends")
	dir := fs.String("dir", "", "working dir")
	model := fs.String("model", "", "model override")
	isolate := fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)")
	remote := fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)")
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("panel: need a prompt")
	}
	var backends []loom.Backend
	for _, name := range strings.Split(*agents, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		b, err := pickBackend(name)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}
	replies := loom.Panel(context.Background(), backends, loom.SessionOpts{Workdir: *dir, Model: *model, Isolate: *isolate, Remote: *remote}, prompt)
	for _, r := range replies {
		fmt.Printf("\n=== %s ===\n", r.Backend)
		if r.Err != "" {
			fmt.Printf("ERROR: %s\n", r.Err)
			continue
		}
		fmt.Println(r.Text)
		if r.CostUSD > 0 {
			fmt.Printf("[$%.4f]\n", r.CostUSD)
		}
	}
	return nil
}

// race: give independent local worktrees to several agents and keep the first tree whose
// deterministic oracle passes. It deliberately uses the run flag surface so trust controls
// (notably --skip-permissions and --allowed-tools) reach every contender unchanged.
func cmdRace(args []string) error {
	fs := flag.NewFlagSet("race", flag.ExitOnError)
	sf := addSessionFlags(fs)
	contenderList := fs.String("contenders", "", "comma-separated agent[:model] contenders")
	oracle := fs.String("oracle", "", "shell command that judges each contender; required because a race needs a deterministic judge")
	timeout := fs.Int("timeout", 600, "whole-race timeout in seconds")
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("race: need a prompt")
	}
	if *oracle == "" {
		return fmt.Errorf("race: -oracle is required (a race needs a deterministic judge)")
	}
	if *sf.connect != "" {
		return fmt.Errorf("race: --connect is not supported (race runs local contenders in isolated dirs)")
	}
	if *sf.remote != "" {
		return fmt.Errorf("race: --remote is not supported (race runs local contenders in isolated dirs)")
	}
	if *sf.resume != "" {
		return fmt.Errorf("race: --resume is not supported (each contender needs a fresh independent session)")
	}
	if *timeout <= 0 {
		return fmt.Errorf("race: --timeout must be greater than zero")
	}
	contenders, err := loom.ParseRaceContenders(*contenderList)
	if err != nil {
		return err
	}
	for i := range contenders {
		contenders[i].Backend, err = pickBackend(contenders[i].Agent)
		if err != nil {
			return err
		}
	}

	res, err := loom.Race(context.Background(), loom.RaceConfig{
		Contenders: contenders, Opts: sf.opts(), Prompt: prompt, Oracle: *oracle, Dir: *sf.dir,
		Timeout:  time.Duration(*timeout) * time.Second,
		Observer: raceObserverCLI(*sf.json),
	})
	if err != nil {
		return err
	}
	return emitRace(res, *sf.json)
}

func raceObserverCLI(jsonOut bool) loom.RaceObserver {
	if jsonOut {
		return loom.RaceObserver{}
	}
	name := func(c loom.RaceContender) string {
		if c.Model == "" {
			return c.Agent
		}
		return c.Agent + ":" + c.Model
	}
	return loom.RaceObserver{
		Start: func(c loom.RaceContender, dir string) {
			fmt.Fprintf(os.Stderr, "[race] %s started in %s\n", name(c), dir)
		},
		Finish: func(c loom.RaceContender, result loom.RaceContenderResult) {
			fmt.Fprintf(os.Stderr, "[race] %s finished: %s\n", name(c), result.Status)
		},
		Verdict: func(c loom.RaceContender, rc int, passed bool) {
			verdict := "failed"
			if passed {
				verdict = "passed"
			}
			fmt.Fprintf(os.Stderr, "[race] %s oracle %s (rc=%d)\n", name(c), verdict, rc)
		},
	}
}

// emitRace keeps stdout useful to both humans and scripts. In human mode the winner's
// directory is prominent because it, not the agents' prose, is the race deliverable.
func emitRace(res loom.RaceResult, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	if res.Winner == nil {
		fmt.Printf("Race finished: %s (no winning deliverable)\n", res.Status)
		return nil
	}
	model := res.Winner.Agent
	if res.Winner.Model != "" {
		model += ":" + res.Winner.Model
	}
	fmt.Printf("Winner: %s (%.2fs)\n", model, res.Winner.DurationS)
	fmt.Printf("Deliverable directory: %s\n", res.Winner.Dir)
	if res.CostUSD > 0 {
		fmt.Fprintf(os.Stderr, "[race $%.4f]\n", res.CostUSD)
	}
	return nil
}

// review: load a diff (or files) and fan a code-review across one or more agents.
func cmdReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	agents := fs.String("agents", "claude", "comma-separated backends")
	dir := fs.String("dir", "", "repo dir for --diff (default: cwd)")
	diff := fs.String("diff", "", "review `git diff <args>` (e.g. HEAD, main...HEAD, \"HEAD~1 HEAD\")")
	model := fs.String("model", "", "model override")
	maxChars := fs.Int("max", 40000, "cap on review content chars")
	events := fs.Bool("events", false, "stream tool calls/thinking to stderr")
	isolate := fs.Bool("isolate", false, "run claude in a docker sandbox (host walled off)")
	remote := fs.String("remote", "", "run claude on a remote box over ssh (e.g. cpuchip@host)")
	_ = fs.Parse(args)

	content, label, err := gatherReview(*dir, *diff, fs.Args(), *maxChars)
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("review: nothing to review (no files given, and the diff is empty)")
	}
	prompt := reviewPrompt(label, content)
	opts := loom.SessionOpts{Workdir: *dir, Model: *model, Isolate: *isolate, Remote: *remote}

	backends, err := backendsFromList(*agents)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "reviewing %s (%d chars) with: %s\n", label, len(content), *agents)

	if len(backends) == 1 {
		sess, err := backends[0].Open(context.Background(), opts)
		if err != nil {
			return err
		}
		defer sess.Close()
		r, err := sendTurn(sess, prompt, *events)
		if err != nil {
			return err
		}
		fmt.Println(r.Text)
		if r.CostUSD > 0 {
			fmt.Fprintf(os.Stderr, "[%s $%.4f]\n", r.Backend, r.CostUSD)
		}
		return nil
	}
	for _, rep := range loom.Panel(context.Background(), backends, opts, prompt) {
		fmt.Printf("\n=== %s ===\n", rep.Backend)
		if rep.Err != "" {
			fmt.Printf("ERROR: %s\n", rep.Err)
			continue
		}
		fmt.Println(rep.Text)
		if rep.CostUSD > 0 {
			fmt.Printf("[$%.4f]\n", rep.CostUSD)
		}
	}
	return nil
}

// duo: two agents bound to one workdir — a WORKER that builds and a CRITIC (trajectory
// eval) that judges at every build point, with loom running the loop between them. The
// worker opens with the caller's trust flags (exactly as `run`); the critic ALWAYS opens
// read-only (--consult) on the SAME --dir, so it inspects reality but can never write. The
// critic backend/model default to the worker's when --critic-* are omitted.
func cmdDuo(args []string) error {
	fs := flag.NewFlagSet("duo", flag.ExitOnError)
	sf := addSessionFlags(fs) // the worker's whole run-trust surface (dir, model, isolate, remote, mcp-config, …)
	criticAgent := fs.String("critic-agent", "", "critic backend for the trajectory eval (defaults to --agent)")
	criticModel := fs.String("critic-model", "", "critic model (defaults to --model)")
	rounds := fs.Int("rounds", loom.DuoDefaultRounds, "max build-point rounds before finishing with rounds_exhausted")
	_ = fs.Parse(args)
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		return fmt.Errorf("duo: need a build task")
	}
	// duo runs two LOCAL sessions bound to one --dir; the warm-resident / ws transport
	// (--connect) drives a single remote session and isn't wired for the pair.
	if *sf.connect != "" {
		return fmt.Errorf("duo: --connect is not supported (duo runs two local sessions bound to one --dir)")
	}

	worker, err := pickBackend(*sf.agent)
	if err != nil {
		return err
	}
	critic := worker
	if *criticAgent != "" {
		if critic, err = pickBackend(*criticAgent); err != nil {
			return err
		}
	}

	// The critic shares the worker's environment (same --dir, isolate/remote) so it sees
	// the SAME tree, but with its own model (default: the worker's), a FRESH session (never
	// the worker's --resume id — that's a different conversation), and read-only intent. Duo
	// also forces Consult; setting it here keeps the CLI's intent visible.
	workerOpts := sf.opts()
	criticOpts := sf.opts()
	if *criticModel != "" {
		criticOpts.Model = *criticModel
	}
	criticOpts.Resume = ""
	criticOpts.Consult = true

	res, err := loom.Duo(context.Background(), loom.DuoConfig{
		Worker: worker, Critic: critic,
		WorkerOpts: workerOpts, CriticOpts: criticOpts,
		Task: task, Rounds: *rounds,
		Observer: duoObserverCLI(*sf.events, *sf.json),
	})
	if err != nil {
		return err
	}
	return emitDuo(res, *sf.json)
}

// duoObserverCLI narrates a duo run to stderr — round banners + verdicts in the human mode,
// each seat's tool events (tagged [worker]/[critic]) under --events. In --json mode the
// human narration is suppressed (stdout carries the one JSON object), but loop WARNINGS and
// --events still reach stderr: a garbled verdict must never be silent.
func duoObserverCLI(events, jsonOut bool) loom.DuoObserver {
	obs := loom.DuoObserver{
		Warn: func(msg string) { fmt.Fprintf(os.Stderr, "  [duo] %s\n", msg) },
	}
	if !jsonOut {
		obs.Round = func(n int) { fmt.Fprintf(os.Stderr, "\n=== round %d ===\n", n) }
		obs.WorkerReply = func(_ int, text string) {
			if loom.WorkerReportedComplete(text) {
				fmt.Fprintln(os.Stderr, "  [worker] BUILD COMPLETE — the critic still judges the finished work")
			}
		}
		obs.Verdict = func(_ int, verdict, feedback string) {
			fmt.Fprintf(os.Stderr, "  [critic] VERDICT: %s\n", verdict)
			if verdict == "REVISE" && feedback != "" {
				fmt.Fprintf(os.Stderr, "  [critic] %s\n", oneLine(feedback, 200))
			}
		}
	}
	if events {
		obs.WorkerEvent = func(ev loom.Event) { duoEvent("worker", ev) }
		obs.CriticEvent = func(ev loom.Event) { duoEvent("critic", ev) }
	}
	return obs
}

// duoEvent prints one streamed tool event, tagged with the seat it came from.
func duoEvent(who string, ev loom.Event) {
	switch ev.Kind {
	case loom.EvToolCall:
		fmt.Fprintf(os.Stderr, "  [%s] → %s\n", who, ev.Tool)
	case loom.EvToolResult:
		fmt.Fprintf(os.Stderr, "  [%s] ← (tool result)\n", who)
	case loom.EvThinking:
		fmt.Fprintf(os.Stderr, "  [%s] · %s\n", who, oneLine(ev.Text, 100))
	}
}

// emitDuo writes the duo outcome: one JSON object on stdout for programmatic callers, else
// the worker's final report on stdout with the status, cost, and both resumable session ids
// on stderr (matching `run`'s feel).
func emitDuo(res loom.DuoResult, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	fmt.Println(res.Text)
	fmt.Fprintf(os.Stderr, "[duo %s after %d round(s)]\n", res.Status, len(res.Rounds))
	if res.CostUSD > 0 {
		fmt.Fprintf(os.Stderr, "[duo $%.4f]\n", res.CostUSD)
	}
	if res.WorkerSession != "" {
		fmt.Fprintf(os.Stderr, "[worker session %s — resume: loom run --resume %s ...]\n", res.WorkerSession, res.WorkerSession)
	}
	if res.CriticSession != "" {
		fmt.Fprintf(os.Stderr, "[critic session %s — inspect: loom run --consult --resume %s ...]\n", res.CriticSession, res.CriticSession)
	}
	return nil
}

// serve: run loom as a websocket service — a client (another loom, a browser) drives
// sessions over a socket with a token, instead of spawning subprocesses / ssh.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7777", "bind address (a mesh IP:port — do NOT use 0.0.0.0 without TLS)")
	tokenFile := fs.String("token-file", "", "newline-delimited token file that gates clients (required unless --tls)")
	addToken := fs.Bool("add-token", false, "mint a token, append it to --token-file, print it, and exit")
	tlsFlag := fs.Bool("tls", false, "serve over pinned mTLS — peers enrolled with `loom pair` connect over wss://; --token-file becomes optional (the pin is the wall)")
	tlsListen := fs.String("tls-listen", "", "COEXISTENCE: also serve pinned mTLS on this second addr while --listen stays plain — clients migrate one at a time (a token file is required and gates BOTH listeners; the tls one adds the pin on top)")
	idleTTL := fs.Duration("idle-ttl", 4*time.Hour, "downgrade a named resident idle longer than this to cold-resumable (closed, lineage remembered); 0 = never")
	openaiHome := fs.String("openai-claude-home", "", "default ~/.claude the OpenAI-shim's isolated sessions mount (skills/settings/MCP); empty = loom default. The /v1/chat/completions endpoint shares the --listen port.")
	openaiHomeRoot := fs.String("openai-home-root", "", "dir holding role-specific claude-homes (<root>/<role>-claude-home); a model named \"<model>#<role>\" (e.g. sonnet#critic) mounts that role's home. Lets one serve host purpose-built environments (critic, review, ...).")
	openaiMCP := fs.String("openai-mcp-config", "", "--mcp-config JSON handed to every OpenAI-shim session: the hinge back into pg-ai-stewards (doc_*, doc_search, …). Claude sessions run in a Linux container, so the config's server must be reachable from there (container-baked binary or http via host.docker.internal); codex/copilot/opencode-routed sessions run on the HOST and resolve the config there (a /home/node/.claude/-anchored path maps to the role home's host dir). The shim routes by MODEL NAME: gpt*/codex*/sol/terra/luna → codex, copilot-* → copilot, '<backend>:<model>' pins, everything else → claude.")
	openaiTimeout := fs.Duration("openai-timeout", 30*time.Minute, "wall-clock cap for ONE shim completion (session spawn → reply). Keep it ABOVE the caller's own client timeout — the session dies with the client connection, so the smaller of the two governs and a too-small pair silently restarts long sessions from zero on the caller's retry.")
	openaiWarm := fs.Bool("openai-warm", false, "keep a WARM sticky seat: a `user:\"sticky:<name>\"` conversation's claude process/container stays alive between turns, so the next turn skips the ~2.5-3s cold spawn+--resume floor (voice companion). DEFAULT OFF — bare models and wi-- dispatches are never warm. Idle seats downgrade to cold-resumable on --idle-ttl; cap concurrent warm seats with LOOM_OPENAI_WARM_MAX (default 8).")
	_ = fs.Parse(args)
	loom.SetOpenAIClaudeHome(*openaiHome)
	loom.SetOpenAIHomeRoot(*openaiHomeRoot)
	loom.SetOpenAIMCPConfig(*openaiMCP)
	loom.SetOpenAITimeout(*openaiTimeout)
	loom.SetOpenAIWarm(*openaiWarm)
	if *addToken {
		if *tokenFile == "" {
			return fmt.Errorf("serve --add-token: --token-file is required (that is where the token is appended)")
		}
		tok, err := loom.AddToken(*tokenFile)
		if err != nil {
			return err
		}
		fmt.Println(tok)
		fmt.Fprintf(os.Stderr, "[token appended to %s — a client drives this box with:\n  loom run --connect ws://<this-box>:<port> --token %s ...]\n", *tokenFile, tok)
		return nil
	}
	if *tlsFlag && *tlsListen != "" {
		return fmt.Errorf("serve: use either --tls (pure pinned mTLS on --listen) or --tls-listen (coexistence: plain + mTLS), not both")
	}
	if *tlsListen != "" {
		id, err := loom.LoadOrCreateIdentity("")
		if err != nil {
			return err
		}
		pins, err := loom.LoadPinStore("")
		if err != nil {
			return err
		}
		return loom.ServeBoth(*listen, *tlsListen, *tokenFile, loom.Backends(), *idleTTL, id, pins)
	}
	if *tlsFlag {
		id, err := loom.LoadOrCreateIdentity("")
		if err != nil {
			return err
		}
		pins, err := loom.LoadPinStore("")
		if err != nil {
			return err
		}
		return loom.ServeTLS(*listen, *tokenFile, loom.Backends(), *idleTTL, id, pins)
	}
	if *tokenFile == "" {
		return fmt.Errorf("serve: --token-file is required (or use --tls for pinned mTLS)")
	}
	return loom.Serve(*listen, *tokenFile, loom.Backends(), *idleTTL)
}

// pair: the SAS device-pairing ceremony — two nodes exchange keys, both humans confirm
// the same six-digit PIN on both screens, and each pins the other's cert. No CA, no
// pre-shared token. One side listens, the other connects; --name pins the peer.
func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	listen := fs.String("listen", "", "wait for one pairing connection on this addr (e.g. 100.x.y.z:7778)")
	connect := fs.String("connect", "", "dial a peer's `loom pair --listen` (host:port)")
	name := fs.String("name", "", "name to pin the peer under (prompted if omitted)")
	_ = fs.Parse(args)
	if (*listen == "") == (*connect == "") {
		return fmt.Errorf("pair: exactly one of --listen or --connect is required")
	}

	id, err := loom.LoadOrCreateIdentity("")
	if err != nil {
		return err
	}
	pins, err := loom.LoadPinStore("")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "this node: %s\n", id.Fingerprint())

	var outcome *loom.PairOutcome
	if *connect != "" {
		fmt.Fprintf(os.Stderr, "pairing with %s …\n", *connect)
		outcome, err = loom.PairConnect(*connect, id)
	} else {
		fmt.Fprintf(os.Stderr, "waiting for a pairing connection on %s …\n", *listen)
		outcome, err = loom.PairListen(*listen, id)
	}
	if err != nil {
		return err
	}

	stdin := bufio.NewReader(os.Stdin)
	ok, err := confirmSAS(stdin, outcome.SAS)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pairing aborted — the PINs did not match (possible man-in-the-middle); nothing pinned")
	}

	peerName := *name
	if peerName == "" {
		peerName, err = promptLine(stdin, "name this peer: ")
		if err != nil {
			return err
		}
		if peerName == "" {
			return fmt.Errorf("pair: a peer name is required to pin")
		}
	}
	if err := pins.Add(peerName, outcome.PeerFingerprint); err != nil {
		return err
	}
	fmt.Printf("pinned %q = %s\n", peerName, outcome.PeerFingerprint)
	fmt.Fprintf(os.Stderr, "trusted. drive it with:\n  loom run --connect wss://<peer-host>:<port> --peer %s ...\n", peerName)
	return nil
}

// confirmSAS shows the six-digit PIN in the two-screen confirm box (the BLE-pairing UX)
// and reads the human's y/N. Only a "y"/"yes" is a match; anything else aborts.
func confirmSAS(stdin *bufio.Reader, sas string) (bool, error) {
	g := loom.GroupSAS(sas)
	fmt.Fprintln(os.Stderr, "  ┌───────────────────────────────┐")
	fmt.Fprintln(os.Stderr, "  │  Confirm this PIN matches the  │")
	fmt.Fprintln(os.Stderr, "  │        other screen:          │")
	fmt.Fprintf(os.Stderr, "  │            %s            │\n", g)
	fmt.Fprintln(os.Stderr, "  └───────────────────────────────┘")
	line, err := promptLine(stdin, "match on both screens? [y/N] ")
	if err != nil {
		return false, err
	}
	l := strings.ToLower(strings.TrimSpace(line))
	return l == "y" || l == "yes", nil
}

// promptLine writes a prompt to stderr and reads one line from the shared stdin reader.
// A single reader is shared across prompts so a read-ahead never swallows the next line.
func promptLine(stdin *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// enroll: code-driven enrollment — the phone / agent / unattended alternative to
// `loom pair` (which needs two humans comparing a PIN on two screens). The box operator
// mints a code on the SERVER side and reads/speaks it; the CLIENT side submits it. One
// code, single use, short-lived.
//
//	on the box (server side), open a one-shot enrollment window:
//	  loom enroll --serve --listen 100.x.y.z:7779 --name phone
//	it prints a code, e.g. K7Q2-M9XA — read/speak it to whoever is enrolling.
//
//	on the enrolling machine/agent (client side):
//	  loom enroll --connect 100.x.y.z:7779 --code K7Q2-M9XA --name mybox
//	then drive it: loom run --connect wss://100.x.y.z:7777 --peer mybox ...
//
// The Android app does the CLIENT side of this exact exchange over HTTP — see
// docs/enrollment.md for the wire protocol.
func cmdEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	serve := fs.Bool("serve", false, "server side: open a one-shot enrollment window and print a code")
	connect := fs.String("connect", "", "client side: dial a box's `loom enroll --serve` (host:port)")
	listen := fs.String("listen", "", "(--serve) bind the enrollment window here (e.g. 100.x.y.z:7779)")
	code := fs.String("code", "", "(--connect) the code the box operator read out")
	name := fs.String("name", "", "(--serve) name to pin the enrolling client under; (--connect) name to pin the server under (the --peer name you will dial)")
	label := fs.String("label", "", "(--connect) optional self-label the server may use as its pin name for you")
	timeout := fs.Duration("timeout", 5*time.Minute, "(--serve) how long the enrollment window stays open")
	_ = fs.Parse(args)

	if *serve == (*connect != "") {
		return fmt.Errorf("enroll: exactly one of --serve or --connect is required")
	}

	id, err := loom.LoadOrCreateIdentity("")
	if err != nil {
		return err
	}
	pins, err := loom.LoadPinStore("")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "this node: %s\n", id.Fingerprint())

	if *serve {
		if *listen == "" {
			return fmt.Errorf("enroll --serve: --listen host:port is required")
		}
		c, err := loom.NewEnrollCode()
		if err != nil {
			return err
		}
		es := &loom.EnrollServer{Identity: id, Pins: pins, Code: c, PinName: *name}
		fmt.Fprintf(os.Stderr, "\n  Enrollment code (read it to the device being enrolled):\n\n      %s\n\n", loom.GroupEnrollCode(c))
		fmt.Fprintf(os.Stderr, "waiting for one enrollment on %s …\n", *listen)
		pinnedName, fp, err := es.ListenAndEnroll(*listen, *timeout)
		if err != nil {
			return err
		}
		fmt.Printf("enrolled %q = %s\n", pinnedName, fp)
		fmt.Fprintln(os.Stderr, "trusted. that client can now drive this box over mTLS (wss://).")
		return nil
	}

	// client side
	if *code == "" {
		return fmt.Errorf("enroll --connect: --code is required (ask the box operator for it)")
	}
	if *name == "" {
		return fmt.Errorf("enroll --connect: --name is required (the peer name you will dial with --peer)")
	}
	res, err := loom.EnrollConnect(*connect, *code, *name, *label, id, pins)
	if err != nil {
		return err
	}
	fmt.Printf("pinned %q = %s\n", res.ServerName, res.ServerFingerprint)
	fmt.Fprintf(os.Stderr, "trusted. drive it with:\n  loom run --connect wss://%s --peer %s ...\n", *connect, res.ServerName)
	return nil
}

func backendsFromList(list string) ([]loom.Backend, error) {
	var out []loom.Backend
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		b, err := pickBackend(name)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no agents given")
	}
	return out, nil
}

// gatherReview collects what to review: the named files, else a git diff.
func gatherReview(dir, diff string, files []string, maxChars int) (content, label string, err error) {
	if len(files) > 0 {
		var sb strings.Builder
		for _, f := range files {
			b, e := os.ReadFile(f)
			if e != nil {
				return "", "", fmt.Errorf("read %s: %w", f, e)
			}
			fmt.Fprintf(&sb, "===== %s =====\n%s\n\n", f, b)
		}
		return clip(sb.String(), maxChars), fmt.Sprintf("%d file(s)", len(files)), nil
	}
	base := []string{}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	if diff != "" {
		out, e := runGit(append(append([]string{}, base...), append([]string{"diff"}, strings.Fields(diff)...)...))
		if e != nil {
			return "", "", e
		}
		return clip(out, maxChars), "git diff " + diff, nil
	}
	// default: uncommitted changes vs HEAD; if none, the latest commit
	if out, _ := runGit(append(append([]string{}, base...), "diff", "HEAD")); strings.TrimSpace(out) != "" {
		return clip(out, maxChars), "the working-tree diff (vs HEAD)", nil
	}
	out, e := runGit(append(append([]string{}, base...), "show", "HEAD"))
	if e != nil {
		return "", "", e
	}
	return clip(out, maxChars), "the latest commit (HEAD)", nil
}

func runGit(args []string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n // back up to a UTF-8 rune boundary so we don't split a multibyte char
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…(truncated)…\n"
}

func reviewPrompt(label, content string) string {
	return "You are a senior code reviewer. Review the following " + label +
		" for CORRECTNESS bugs, security risks, and real defects — not style or formatting. " +
		"List the most important findings (most severe first), each with the location if visible " +
		"and a one-line why. If it looks correct, say so plainly. Be specific and concise.\n\n" +
		content
}

// sendTurn runs one turn, optionally streaming the agent's tool calls + thinking
// to stderr (the work) while the final answer is returned in the Reply.
func sendTurn(sess loom.Session, prompt string, showEvents bool) (loom.Reply, error) {
	return sendTurnTee(sess, prompt, showEvents, nil)
}

// sendTurnTee is sendTurn plus an optional extra event sink (the run recorder's log). It
// preserves the exact old fast path — sess.Send with no streaming — only when there is
// neither a stderr printer nor an extra sink; otherwise it streams so every event reaches
// the durable log even when --events is off.
func sendTurnTee(sess loom.Session, prompt string, showEvents bool, extra func(loom.Event)) (loom.Reply, error) {
	// While this turn runs, the first Ctrl-C interrupts the AGENT's work (not loom) —
	// the session stays alive, so you can steer with your next message. Default
	// Ctrl-C (exit loom) is restored the moment the turn returns.
	if it, ok := sess.(loom.Interruptible); ok {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		done := make(chan struct{})
		go func() {
			select {
			case <-sigCh:
				fmt.Fprintln(os.Stderr, "  ⎋ interrupting…")
				_ = it.Interrupt()
			case <-done:
			}
		}()
		defer func() { signal.Stop(sigCh); close(done) }()
	}
	if !showEvents && extra == nil {
		return sess.Send(context.Background(), prompt)
	}
	var printer func(loom.Event)
	if showEvents {
		printer = eventPrinter()
	}
	cb := func(ev loom.Event) {
		if extra != nil {
			extra(ev)
		}
		if printer != nil {
			printer(ev)
		}
	}
	return sess.SendStream(context.Background(), prompt, cb)
}

func eventPrinter() func(loom.Event) {
	return func(ev loom.Event) {
		switch ev.Kind {
		case loom.EvToolCall:
			fmt.Fprintf(os.Stderr, "  → %s\n", ev.Tool)
		case loom.EvToolResult:
			fmt.Fprintln(os.Stderr, "  ← (tool result)")
		case loom.EvThinking:
			fmt.Fprintf(os.Stderr, "  · %s\n", oneLine(ev.Text, 100))
			// EvAssistant + EvResult are carried by the returned Reply
		}
	}
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func usage() {
	fmt.Fprintln(os.Stderr, `loom — a harness around the harnesses (drive coding-agent CLIs as workers)

usage:
  loom run   --agent claude [--model M] [--dir D] "prompt"     one-shot
  loom chat  --agent claude [--model M] [--dir D]              multi-turn (one msg per stdin line)
  loom panel  --agents claude,agy [--dir D] "prompt"           fan one prompt across agents (council)
  loom race   --contenders codex:gpt-5.6-terra,claude:sonnet --oracle "<shell cmd>" [--dir D] [--timeout 600] "prompt"
  loom review --agents claude,local [--dir R] [--diff HEAD] [files...]   review a diff or files
  loom duo    --agent claude --critic-agent codex [--dir D] [--rounds 6] "task"   worker builds, critic judges each build point
  loom serve --listen 127.0.0.1:7777 --token-file ~/.loom/tokens [--add-token] [--idle-ttl 4h]   run loom as a ws service
  loom serve --listen 100.x.y.z:7777 --tls                     run loom over pinned mTLS (peers enrolled via loom pair/enroll)
  loom serve --listen 100.x.y.z:7777 --tls-listen 100.x.y.z:7778 --token-file T   COEXISTENCE: plain + mTLS at once (migrate clients one at a time)
  loom pair  --listen 100.x.y.z:7778                           wait for a pairing (one box, two humans compare a PIN)
  loom pair  --connect 100.x.y.z:7778 --name box-b             pair with the waiting box, confirm the PIN, pin its cert
  loom enroll --serve --listen 100.x.y.z:7779 --name phone     one-shot code enrollment window (for a phone/agent that can't do the PIN compare)
  loom enroll --connect 100.x.y.z:7779 --code K7Q2-M9XA --name mybox   submit the code, pin the box's cert
  loom agents                                                  list backends

  warm-resident (over --connect): keep a claude process resident + warm and reattach by NAME —
  the first open cold-reads once, every later drive is a cache-warm reattach.
  loom send    --connect ws://host:port --token T --session NAME [--detach] "msg"   reattach-or-open, send
  loom await   --connect ws://host:port --token T --session NAME --turn ID [--last-reply] [--timeout S]  fetch a detached reply
  loom sessions --connect ws://host:port --token T             list resident sessions
  loom runs                                                    list recent loom-run lifecycle records (running/heartbeat-stale/done/failed)
  loom runs tail <run-id>                                      print a run's streamed output.log (survives a dead wrapper)

  --connect ws://host:port --token T   (on run/chat/send) drive a remote loom serve over websocket —
                                       the --agent/--model/--dir/--resume/opts are opened THERE
  --connect wss://host:port --peer N   (on run/chat/send) drive a remote loom serve over PINNED mTLS —
                                       N is the pinned peer name from loom pair (no token needed)
  --session NAME                       (on run/chat/send) reattach a warm resident by name (no respawn)`)
}
