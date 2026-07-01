package loom

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ClaudeBackend drives Claude Code (`claude`) as a PERSISTENT stream-json
// session: one process, many turns fed over stdin, holding context across turns.
//
// Verified 2026-06-29 (Claude Code v2.1.196): turn 1 "remember 42" → turn 2
// recalled "42", same session_id, one process. Each turn emits its own `result`
// event; cost is cumulative so we report the per-turn delta. The cold session
// pays ~27K cache-creation tokens once; subsequent turns cache-READ the prior
// context — that amortization is the whole point of keeping the session alive.
type ClaudeBackend struct {
	Bin string // default "claude"
}

func (b ClaudeBackend) Name() string { return "claude" }

func (b ClaudeBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	bin := b.Bin
	if bin == "" {
		bin = "claude"
	}
	return &claudeSession{bin: bin, opts: opts}, nil
}

type claudeSession struct {
	bin  string
	opts SessionOpts

	turnMu sync.Mutex // one turn at a time; held across a SendStream turn
	ioMu   sync.Mutex // guards stdin writes + the fields below (NOT held during the read)

	started   bool
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	sc        *bufio.Scanner
	sessionID string
	lastCost  float64 // cumulative cost seen so far (for per-turn deltas)
	reqID     int     // control_request id counter (interrupts)
}

func (s *claudeSession) ensureStarted(ctx context.Context) error {
	if s.started {
		return nil
	}
	cmd := claudeCmd(ctx, s.bin, s.opts, claudeArgs(s.opts))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // result lines (full transcript) can be large
	s.cmd, s.stdin, s.sc, s.started = cmd, stdin, sc, true
	return nil
}

func (s *claudeSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *claudeSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.turnMu.Lock() // one turn at a time; NOT held during Interrupt (that uses ioMu)
	defer s.turnMu.Unlock()

	s.ioMu.Lock()
	err := s.ensureStarted(ctx)
	s.ioMu.Unlock()
	if err != nil {
		return Reply{Backend: "claude", Err: err.Error()}, err
	}
	if err := s.writeUser(prompt); err != nil {
		return Reply{Backend: "claude", Err: err.Error()}, err
	}
	// Read events until THIS turn's result, holding NO lock — so Interrupt (which
	// writes a control_request under ioMu) can fire while we're mid-read. The scanner
	// is single-reader by construction (turnMu serializes turns).
	for s.sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(s.sc.Bytes(), &ev) != nil {
			continue // skip any non-JSON noise
		}
		switch ev["type"] {
		case "assistant":
			emitClaudeContent(onEvent, ev)
		case "user": // tool_result blocks come back as user messages
			emitClaudeContent(onEvent, ev)
		case "result":
			r := Reply{Backend: "claude"}
			r.Text, _ = ev["result"].(string)
			r.SessionID, _ = ev["session_id"].(string)
			if n, ok := ev["num_turns"].(float64); ok {
				r.Turns = int(n)
			}
			// An interrupt ends the turn with subtype "error_during_execution" and
			// is_error=true but no result text — surface it as a clear error so the
			// caller knows the turn was cut short (the session stays alive).
			if isErr, _ := ev["is_error"].(bool); isErr && r.Err == "" {
				if r.Text != "" {
					r.Err = r.Text
				} else if sub, _ := ev["subtype"].(string); sub != "" {
					r.Err = sub
				} else {
					r.Err = "error"
				}
			}
			s.ioMu.Lock()
			if cum, ok := ev["total_cost_usd"].(float64); ok {
				r.CostUSD = cum - s.lastCost // total_cost_usd is cumulative across turns
				s.lastCost = cum
			}
			s.sessionID = r.SessionID
			s.ioMu.Unlock()
			emit(onEvent, Event{Kind: EvResult, Backend: "claude", Text: r.Text})
			return r, nil
		}
	}
	if err := s.sc.Err(); err != nil {
		return Reply{Backend: "claude", Err: err.Error()}, err
	}
	return Reply{Backend: "claude", Err: "stream ended before result"},
		fmt.Errorf("claude: stream ended before a result event (process exited?)")
}

// writeUser sends one user-message turn as a single NDJSON line (under ioMu, so it
// can't interleave with an Interrupt write).
func (s *claudeSession) writeUser(content string) error {
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	line, _ := json.Marshal(msg)
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("claude: session not started")
	}
	_, err := fmt.Fprintf(s.stdin, "%s\n", line)
	return err
}

// Interrupt stops the in-flight turn: it writes a stream-json control_request with
// subtype "interrupt" (verified 2026-06-30 — claude acks with a control_response
// success, then ends the turn with a result subtype "error_during_execution").
// The subprocess stays alive; steer by calling Send with a new instruction.
// Safe to call concurrently with SendStream (writes under ioMu; the read holds no lock).
func (s *claudeSession) Interrupt() error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("claude: no session to interrupt")
	}
	s.reqID++
	msg := map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("int-%d", s.reqID),
		"request":    map[string]any{"subtype": "interrupt"},
	}
	line, _ := json.Marshal(msg)
	_, err := fmt.Fprintf(s.stdin, "%s\n", line)
	return err
}

// emitClaudeContent walks a claude message event's content blocks → typed events.
func emitClaudeContent(onEvent func(Event), ev map[string]any) {
	if onEvent == nil {
		return
	}
	m, _ := ev["message"].(map[string]any)
	if m == nil {
		return
	}
	blocks, _ := m["content"].([]any) // a string content (our own echo) yields nil → skip
	for _, b := range blocks {
		blk, _ := b.(map[string]any)
		if blk == nil {
			continue
		}
		switch blk["type"] {
		case "text":
			if t, _ := blk["text"].(string); t != "" {
				emit(onEvent, Event{Kind: EvAssistant, Backend: "claude", Text: t})
			}
		case "thinking":
			if t, _ := blk["thinking"].(string); t != "" {
				emit(onEvent, Event{Kind: EvThinking, Backend: "claude", Text: t})
			}
		case "tool_use":
			name, _ := blk["name"].(string)
			emit(onEvent, Event{Kind: EvToolCall, Backend: "claude", Tool: name})
		case "tool_result":
			emit(onEvent, Event{Kind: EvToolResult, Backend: "claude"})
		}
	}
}

// claudeArgs builds the persistent-session flags. --verbose is REQUIRED with
// --print + --output-format stream-json. --resume <id> reattaches to a prior
// session by id (context restored from the session store on whichever box runs
// claude) — the piece that lets a session survive a process restart or a dropped
// remote pipe: run once, keep the Reply.SessionID, reopen with Resume set.
func claudeArgs(opts SessionOpts) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Resume != "" {
		args = append(args, "--resume", opts.Resume)
	}
	// substrate-integration passthroughs (config-file paths are target-relative)
	if opts.MCPConfig != "" {
		args = append(args, "--mcp-config", opts.MCPConfig)
	}
	if opts.AllowedTools != "" {
		args = append(args, "--allowed-tools", opts.AllowedTools)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}
	if opts.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if opts.SystemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", opts.SystemPromptFile)
	}
	return args
}

// claudeCmd builds the transport chain for a claude invocation — the trust axis:
//
//	direct          → claude …                                  (cwd = Workdir; full host access)
//	isolate         → docker run -i … loom-claude claude …      (sandboxed: only /work + creds)
//	remote          → ssh -T <host> bash -lc 'cd <dir> && claude …'          (the REMOTE box's own claude)
//	remote+isolate  → ssh -T <host> bash -lc 'docker run -i … loom-claude claude …'  (sandboxed ON the remote)
//
// stream-json flows over each transport's stdin/stdout unchanged — even chained
// through ssh then docker. remote+isolate resolves docker paths on the remote ($HOME).
func claudeCmd(ctx context.Context, bin string, opts SessionOpts, claudeArgs []string) *exec.Cmd {
	if opts.Remote != "" {
		var inner string
		if opts.Isolate {
			// remote + isolate: a sandboxed claude ON the remote box. The docker
			// command runs over there, so its volume paths must resolve on the REMOTE
			// ($HOME expanded by the remote login shell; --dir is a remote path).
			inner = strings.Join(remoteDockerArgs(opts, claudeArgs), " ")
		} else {
			inner = strings.Join(append([]string{bin}, claudeArgs...), " ")
			if opts.Workdir != "" {
				inner = "cd " + opts.Workdir + " && " + inner
			}
		}
		// run inside a LOGIN shell so the remote's full PATH loads — a plain
		// `ssh host cmd` uses a non-interactive shell that misses nvm / npm-global
		// installs, so `claude`/`docker` read as "command not found".
		return exec.CommandContext(ctx, "ssh", "-T", opts.Remote, "bash", "-lc", shellQuote(inner))
	}
	if opts.Isolate {
		a := dockerRunArgs(opts, claudeArgs)
		return exec.CommandContext(ctx, a[0], a[1:]...)
	}
	cmd := exec.CommandContext(ctx, bin, claudeArgs...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	return cmd
}

// dockerArgs builds `docker run -i … <image> claude <args>` so the agent is
// sandboxed: it sees only the workdir (bind-mounted /work) and the credentials file
// (read-only) — never the host. claude writes its own state to an ephemeral
// in-container ~/.claude (gone on --rm). wd/creds are already in the target
// platform's path form (local: forward-slashed host paths; remote: $HOME-relative).
// dockerArgs builds the `docker run …` invocation. claudeHome (if set) is mounted
// as the container's WRITABLE ~/.claude — skills, instructions, settings, MCP, AND
// persisted session state (so a later --resume container reattaches). The creds are
// still layered read-only on top, so auth works without copying secrets into the home.
func dockerArgs(wd, creds, claudeHome, image string, claudeArgs []string) []string {
	if image == "" {
		image = "loom-claude"
	}
	// the loom-claude image runs as the non-root `node` user, so ~/.claude is here
	const claudeDir = "/home/node/.claude"
	a := []string{"docker", "run", "-i", "--rm"}
	if claudeHome != "" {
		a = append(a, "-v", claudeHome+":"+claudeDir)
	}
	a = append(a, "-v", wd+":/work", "-w", "/work")
	if creds != "" {
		a = append(a, "-v", creds+":"+claudeDir+"/.credentials.json:ro")
	}
	a = append(a, image, "claude")
	return append(a, claudeArgs...)
}

// dockerRunArgs sandboxes claude on the LOCAL machine — host paths, forward-slashed
// for Docker Desktop on Windows.
func dockerRunArgs(opts SessionOpts, claudeArgs []string) []string {
	wd := opts.Workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	home, _ := os.UserHomeDir()
	creds := filepath.Join(home, ".claude", ".credentials.json")
	claudeHome := ""
	if opts.ClaudeHome != "" {
		claudeHome = dockerVol(opts.ClaudeHome)
	}
	return dockerArgs(dockerVol(wd), dockerVol(creds), claudeHome, opts.Image, claudeArgs)
}

// remoteDockerArgs sandboxes claude on the REMOTE box (for --remote --isolate). The
// docker command runs over ssh inside a login shell, so paths resolve THERE: $HOME
// is expanded by the remote bash, and --dir/--claude-home are remote paths. The
// remote must have docker, the loom-claude image, and an authed
// ~/.claude/.credentials.json. Pass --dir to scope the sandbox; without it, it falls
// back to the remote $HOME.
func remoteDockerArgs(opts SessionOpts, claudeArgs []string) []string {
	wd := opts.Workdir
	if wd == "" {
		wd = "$HOME"
	}
	return dockerArgs(wd, "$HOME/.claude/.credentials.json", opts.ClaudeHome, opts.Image, claudeArgs)
}

// dockerVol normalizes a host path for a Docker bind mount (C:\path → C:/path,
// which Docker Desktop on Windows accepts).
func dockerVol(host string) string { return filepath.ToSlash(host) }

// shellQuote single-quotes s for a POSIX shell, so `bash -lc <script>` on the far
// side of ssh receives the whole script (spaces, &&, flags) as one argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *claudeSession) SessionID() string {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	return s.sessionID
}

func (s *claudeSession) Close() error {
	s.ioMu.Lock()
	stdin, cmd := s.stdin, s.cmd
	s.ioMu.Unlock()
	if stdin != nil {
		_ = stdin.Close() // EOF → claude finishes pending work and exits
	}
	if cmd != nil {
		return cmd.Wait()
	}
	return nil
}
