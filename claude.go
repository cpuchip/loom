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

	mu        sync.Mutex
	started   bool
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	sc        *bufio.Scanner
	sessionID string
	lastCost  float64 // cumulative cost seen so far (for per-turn deltas)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureStarted(ctx); err != nil {
		return Reply{Backend: "claude", Err: err.Error()}, err
	}
	// one user turn, as a single NDJSON line
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}}
	line, _ := json.Marshal(msg)
	if _, err := fmt.Fprintf(s.stdin, "%s\n", line); err != nil {
		return Reply{Backend: "claude", Err: err.Error()}, err
	}
	// read events until THIS turn's result event, emitting along the way
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
			if cum, ok := ev["total_cost_usd"].(float64); ok {
				r.CostUSD = cum - s.lastCost // total_cost_usd is cumulative across turns
				s.lastCost = cum
			}
			if isErr, _ := ev["is_error"].(bool); isErr && r.Err == "" {
				r.Err = r.Text
			}
			s.sessionID = r.SessionID
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
	return args
}

// claudeCmd builds the transport chain for a claude invocation — the trust axis:
//
//	direct   → claude …                            (cwd = Workdir; full host access)
//	isolate  → docker run -i … loom-claude claude … (sandboxed: only /work + creds)
//	remote   → ssh -T <host> [cd <dir> &&] claude … (the REMOTE box's own claude + auth)
//
// stream-json flows over each transport's stdin/stdout unchanged. (remote+isolate —
// docker on the remote with remote paths/creds — is a v2; remote wins here.)
func claudeCmd(ctx context.Context, bin string, opts SessionOpts, claudeArgs []string) *exec.Cmd {
	if opts.Remote != "" {
		inner := strings.Join(append([]string{bin}, claudeArgs...), " ")
		if opts.Workdir != "" {
			inner = "cd " + opts.Workdir + " && " + inner
		}
		// run inside a LOGIN shell so the remote's full PATH loads — a plain
		// `ssh host cmd` uses a non-interactive shell that misses nvm / npm-global
		// installs, so `claude` reads as "command not found".
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

// dockerRunArgs builds `docker run -i … loom-claude claude <args>` so the agent is
// sandboxed: it sees only the workdir (bind-mounted /work) and the credentials file
// (read-only) — never the host. claude writes its own state to an ephemeral
// in-container ~/.claude (gone on --rm).
func dockerRunArgs(opts SessionOpts, claudeArgs []string) []string {
	wd := opts.Workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	home, _ := os.UserHomeDir()
	image := opts.Image
	if image == "" {
		image = "loom-claude"
	}
	a := []string{
		"docker", "run", "-i", "--rm",
		"-v", dockerVol(wd) + ":/work", "-w", "/work",
		"-v", dockerVol(filepath.Join(home, ".claude", ".credentials.json")) + ":/root/.claude/.credentials.json:ro",
		image, "claude",
	}
	return append(a, claudeArgs...)
}

// dockerVol normalizes a host path for a Docker bind mount (C:\path → C:/path,
// which Docker Desktop on Windows accepts).
func dockerVol(host string) string { return filepath.ToSlash(host) }

// shellQuote single-quotes s for a POSIX shell, so `bash -lc <script>` on the far
// side of ssh receives the whole script (spaces, &&, flags) as one argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *claudeSession) SessionID() string { return s.sessionID }

func (s *claudeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin != nil {
		_ = s.stdin.Close() // EOF → claude finishes pending work and exits
	}
	if s.cmd != nil {
		return s.cmd.Wait()
	}
	return nil
}
