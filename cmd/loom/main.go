// Command loom drives coding-agent CLIs (Claude Code, agy/Gemini) as workers.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	case "send":
		err = cmdSend(os.Args[2:])
	case "await":
		err = cmdAwait(os.Args[2:])
	case "sessions":
		err = cmdSessions(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "pair":
		err = cmdPair(os.Args[2:])
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
	agent, model, dir, remote, resume *string
	mcpConfig, allowedTools, permMode *string
	sysPromptFile, claudeHome         *string
	connect, token, session, peer     *string
	events, isolate, skipPerms, json  *bool
	consult                           *bool
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
		connect:       fs.String("connect", "", "drive a remote `loom serve` over websocket (ws://host:port, or wss://host:port for pinned mTLS) — the --agent/opts are opened THERE"),
		token:         fs.String("token", "", "auth token for --connect"),
		peer:          fs.String("peer", "", "(--connect wss://) the pinned peer name you paired with (loom pair)"),
		session:       fs.String("session", "", "(--connect) reattach a warm resident by this stable NAME — a second use reuses the live process, no respawn/cold-read"),
		json:          fs.Bool("json", false, "emit the Reply as JSON to stdout (for programmatic/subprocess callers)"),
	}
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
		Consult: *sf.consult,
	}
}

// run: one-shot prompt → single reply.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	sf := addSessionFlags(fs)
	_ = fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return fmt.Errorf("run: need a prompt")
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
	r, err := sendTurn(sess, prompt, *sf.events)
	if err != nil {
		return err
	}
	if err := emitReply(r, *sf.json); err != nil {
		return err
	}
	if !*sf.json && r.SessionID != "" {
		fmt.Fprintf(os.Stderr, "[session %s — resume: loom run --resume %s ...]\n", r.SessionID, r.SessionID)
	}
	return nil
}

// chat: persistent multi-turn session — one message per stdin line.
func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	sf := addSessionFlags(fs)
	_ = fs.Parse(args)
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

// serve: run loom as a websocket service — a client (another loom, a browser) drives
// sessions over a socket with a token, instead of spawning subprocesses / ssh.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:7777", "bind address (a mesh IP:port — do NOT use 0.0.0.0 without TLS)")
	tokenFile := fs.String("token-file", "", "newline-delimited token file that gates clients (required unless --tls)")
	addToken := fs.Bool("add-token", false, "mint a token, append it to --token-file, print it, and exit")
	tlsFlag := fs.Bool("tls", false, "serve over pinned mTLS — peers enrolled with `loom pair` connect over wss://; --token-file becomes optional (the pin is the wall)")
	idleTTL := fs.Duration("idle-ttl", 4*time.Hour, "downgrade a named resident idle longer than this to cold-resumable (closed, lineage remembered); 0 = never")
	openaiHome := fs.String("openai-claude-home", "", "~/.claude the OpenAI-shim's isolated sessions mount (skills/settings/MCP); empty = loom default. The /v1/chat/completions endpoint shares the --listen port.")
	_ = fs.Parse(args)
	loom.SetOpenAIClaudeHome(*openaiHome)
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
	if showEvents {
		return sess.SendStream(context.Background(), prompt, eventPrinter())
	}
	return sess.Send(context.Background(), prompt)
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
  loom review --agents claude,local [--dir R] [--diff HEAD] [files...]   review a diff or files
  loom serve --listen 127.0.0.1:7777 --token-file ~/.loom/tokens [--add-token] [--idle-ttl 4h]   run loom as a ws service
  loom serve --listen 100.x.y.z:7777 --tls                     run loom over pinned mTLS (peers enrolled via loom pair)
  loom pair  --listen 100.x.y.z:7778                           wait for a pairing (one box)
  loom pair  --connect 100.x.y.z:7778 --name box-b             pair with the waiting box, confirm the PIN, pin its cert
  loom agents                                                  list backends

  warm-resident (over --connect): keep a claude process resident + warm and reattach by NAME —
  the first open cold-reads once, every later drive is a cache-warm reattach.
  loom send    --connect ws://host:port --token T --session NAME [--detach] "msg"   reattach-or-open, send
  loom await   --connect ws://host:port --token T --session NAME --turn ID [--last-reply] [--timeout S]  fetch a detached reply
  loom sessions --connect ws://host:port --token T             list resident sessions

  --connect ws://host:port --token T   (on run/chat/send) drive a remote loom serve over websocket —
                                       the --agent/--model/--dir/--resume/opts are opened THERE
  --connect wss://host:port --peer N   (on run/chat/send) drive a remote loom serve over PINNED mTLS —
                                       N is the pinned peer name from loom pair (no token needed)
  --session NAME                       (on run/chat/send) reattach a warm resident by name (no respawn)`)
}
