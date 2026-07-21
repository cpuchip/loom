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

// CodexBackend drives OpenAI Codex CLI (`codex`) as a PER-TURN exec session:
// each Send spawns `codex exec --json` (turn 1) or `codex exec resume <id> --json`
// (later turns), with the prompt fed over stdin. Codex persists every session to
// disk as it runs, so multi-turn context, resume-after-restart, and steer-after-
// interrupt all ride its own session store — the same durable-session shape as
// claude --resume, via a one-shot process per turn instead of a persistent one.
//
// Verified 2026-07-02 (codex-cli 0.141.0): turn 1 "reply PONG" → `exec resume
// <thread_id>` turn 2 recalled PONG and ran a shell command; JSONL events
// (thread.started / item.started / item.completed / turn.completed) parsed as
// implemented here. Two CLI quirks the flags below encode: `exec resume` accepts
// a NARROWER flag set than `exec` (no -s/-C — sandbox rides `-c sandbox_mode=`),
// and codex reads stdin when piped, so the prompt is passed as the `-` sentinel.
//
// The trust ladder maps to codex's NATIVE kernel-level sandbox (its own wall —
// no docker image needed):
//
//	SkipPermissions → --dangerously-bypass-approvals-and-sandbox  (no wall; for externally-sandboxed environments)
//	Consult         → sandbox read-only + the consult directive   (instruction AND enforcement)
//	Isolate         → sandbox workspace-write                     (edits walled to the workdir)
//	(none)          → codex's own config default
//
// MCPConfig IS honored (local sessions): the claude-format JSON is translated at
// Open into per-invocation `-c mcp_servers.<name>.<key>=…` config overrides (see
// mcpbridge.go) — codex's native MCP surface, without touching the user's
// ~/.codex/config.toml. A REMOTE session still ignores it: the config file is a
// local path, and the remote transport joins argv through `bash -lc` unquoted,
// which would mangle the TOML values — wire the remote box's own config.toml
// instead. Claude-specific opts with no codex analog (AllowedTools,
// PermissionMode, ClaudeHome, Image) remain ignored. SystemPromptFile is
// approximated by prepending the file's contents to the first prompt (codex has
// no append-system-prompt flag).
type CodexBackend struct {
	Bin string // default "codex"
}

func (b CodexBackend) Name() string { return "codex" }

func (b CodexBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	bin := b.Bin
	if bin == "" {
		bin = "codex"
	}
	if err := mirrorSkills(opts, "codex"); err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	var mcpArgs []string
	if opts.MCPConfig != "" && opts.Remote == "" {
		var err error
		if mcpArgs, err = codexMCPArgs(opts.MCPConfig); err != nil {
			// Fail at Open, like claude's own startup exit on a bad --mcp-config —
			// a misconfigured hinge should be loud, not a silently toolless worker.
			return nil, fmt.Errorf("codex: mcp-config: %w", err)
		}
	}
	return &codexSession{bin: bin, opts: opts, threadID: opts.Resume, mcpArgs: mcpArgs}, nil
}

type codexSession struct {
	bin     string
	opts    SessionOpts
	mcpArgs []string // `-c mcp_servers.…` overrides translated from opts.MCPConfig at Open (immutable)

	turnMu sync.Mutex // one turn at a time; held across a SendStream turn
	mu     sync.Mutex // guards the fields below (NOT held during the read)

	threadID    string
	firstSent   bool // system-prompt-file prepend happens once
	cur         *exec.Cmd
	interrupted bool
}

func (s *codexSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *codexSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.mu.Lock()
	resume := s.threadID
	prompt = s.framePrompt(prompt)
	s.interrupted = false
	s.mu.Unlock()

	cmd := codexCmd(ctx, s.bin, s.opts, codexArgs(s.opts, resume, s.mcpArgs))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Reply{Backend: "codex", Err: err.Error()}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Reply{Backend: "codex", Err: err.Error()}, err
	}
	cmd.Stderr = os.Stderr
	// StartChild (not a bare cmd.Start) so a dying loom wrapper reaps this child — see reap.go.
	if err := StartChild(cmd); err != nil {
		return Reply{Backend: "codex", Err: err.Error()}, err
	}
	s.mu.Lock()
	s.cur = cmd
	s.mu.Unlock()

	// The `-` prompt sentinel means "read the prompt from stdin"; write it and
	// close so the one-shot turn starts (codex otherwise waits on stdin EOF).
	_, _ = io.WriteString(stdin, prompt)
	_ = stdin.Close()

	r := Reply{Backend: "codex"}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // aggregated tool output can be large
	for sc.Scan() {
		handleCodexLine(sc.Bytes(), &r, onEvent)
	}
	if err := sc.Err(); err != nil && r.Err == "" {
		r.Err = err.Error()
	}
	waitErr := cmd.Wait()
	s.mu.Lock()
	s.cur = nil
	if r.SessionID != "" {
		s.threadID = r.SessionID
	}
	wasInterrupted := s.interrupted
	s.mu.Unlock()

	if wasInterrupted && r.Err == "" {
		// The turn was cut short on purpose; the on-disk session survived. Steer by
		// calling Send again — the next turn resumes with context intact.
		r.Err = "interrupted"
		emit(onEvent, Event{Kind: EvResult, Backend: "codex", Text: r.Text})
		return r, nil
	}
	if r.Err == "" && r.Text == "" && waitErr != nil {
		r.Err = waitErr.Error()
		return r, waitErr
	}
	emit(onEvent, Event{Kind: EvResult, Backend: "codex", Text: r.Text})
	if r.Err != "" {
		return r, fmt.Errorf("codex: %s", r.Err)
	}
	return r, nil
}

// framePrompt applies the instruction-level opts to the outgoing prompt: the
// system-prompt file (first turn only — after that it lives in the session
// context) and the consult directive (every turn — each spawn is a new process).
func (s *codexSession) framePrompt(prompt string) string {
	if !s.firstSent && s.opts.SystemPromptFile != "" {
		if data, err := os.ReadFile(s.opts.SystemPromptFile); err == nil && len(data) > 0 {
			prompt = "<instructions>\n" + strings.TrimSpace(string(data)) + "\n</instructions>\n\n" + prompt
		}
	}
	s.firstSent = true
	if s.opts.Consult {
		prompt = consultDirective + "\n\n" + prompt
	}
	return prompt
}

// handleCodexLine parses one JSONL event from `codex exec --json` into the Reply
// and typed loom events. Schema verified live (0.141.0):
//
//	{"type":"thread.started","thread_id":"…"}
//	{"type":"item.started","item":{"type":"command_execution","command":"…",…}}
//	{"type":"item.completed","item":{"type":"agent_message","text":"…"}}
//	{"type":"turn.completed","usage":{…tokens…}}   (tokens only — no USD; CostUSD stays 0)
//	{"type":"turn.failed","error":{"message":"…"}} / {"type":"error","message":"…"}
func handleCodexLine(line []byte, r *Reply, onEvent func(Event)) {
	var ev struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Usage    struct {
			InputTokens           int `json:"input_tokens"`
			CachedInputTokens     int `json:"cached_input_tokens"`
			OutputTokens          int `json:"output_tokens"`
			ReasoningOutputTokens int `json:"reasoning_output_tokens"`
		} `json:"usage"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			Type             string `json:"type"`
			Text             string `json:"text"`
			Command          string `json:"command"`
			AggregatedOutput string `json:"aggregated_output"`
			Server           string `json:"server"`
			Tool             string `json:"tool"`
			Query            string `json:"query"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return // skip non-JSON noise
	}
	switch ev.Type {
	case "thread.started":
		r.SessionID = ev.ThreadID
	case "item.started":
		switch ev.Item.Type {
		case "command_execution":
			emit(onEvent, Event{Kind: EvToolCall, Backend: "codex", Tool: "shell", Text: ev.Item.Command})
		case "mcp_tool_call":
			emit(onEvent, Event{Kind: EvToolCall, Backend: "codex", Tool: ev.Item.Server + "." + ev.Item.Tool})
		case "web_search":
			emit(onEvent, Event{Kind: EvToolCall, Backend: "codex", Tool: "web_search", Text: ev.Item.Query})
		}
	case "item.completed":
		switch ev.Item.Type {
		case "agent_message":
			if ev.Item.Text != "" {
				r.Text = ev.Item.Text // the LAST agent message is the turn's answer
				emit(onEvent, Event{Kind: EvAssistant, Backend: "codex", Text: ev.Item.Text})
			}
		case "reasoning":
			if ev.Item.Text != "" {
				emit(onEvent, Event{Kind: EvThinking, Backend: "codex", Text: ev.Item.Text})
			}
		case "command_execution":
			emit(onEvent, Event{Kind: EvToolResult, Backend: "codex", Tool: "shell", Text: ev.Item.AggregatedOutput})
		case "mcp_tool_call":
			emit(onEvent, Event{Kind: EvToolResult, Backend: "codex", Tool: ev.Item.Server + "." + ev.Item.Tool})
		case "file_change":
			emit(onEvent, Event{Kind: EvToolResult, Backend: "codex", Tool: "file_change"})
		}
	case "turn.completed":
		r.Turns++
		// Live shape 2026-07-20 (codex-cli 0.14x): usage{input_tokens,
		// cached_input_tokens, output_tokens, reasoning_output_tokens}. codex's
		// input_tokens INCLUDES the cached portion — subtract it so InputTokens
		// means "fresh input" on every backend. Tokens only; codex reports no
		// USD. Accumulated (+=) because one Send can complete multiple turns.
		u := ev.Usage
		if u.InputTokens+u.CachedInputTokens+u.OutputTokens > 0 {
			r.Usage = addUsage(r.Usage, &Usage{
				InputTokens:     max(u.InputTokens-u.CachedInputTokens, 0),
				OutputTokens:    u.OutputTokens,
				CacheReadTokens: u.CachedInputTokens,
				CostSource:      CostNone,
			})
		}
	case "turn.failed":
		if ev.Error.Message != "" {
			r.Err = ev.Error.Message
		} else {
			r.Err = "turn failed"
		}
	case "error":
		if ev.Message != "" {
			r.Err = ev.Message
		}
	}
}

// codexArgs builds the argv for one turn. Turn 1 is `exec` (full flag set); later
// turns are `exec resume <id>` (narrower set: no -s/-C — sandbox rides -c, and the
// workdir is the session's). mcpArgs (the `-c mcp_servers.…` overrides) ride EVERY
// turn — each turn is a fresh codex process that must re-wire its MCP servers; -c
// is accepted by `exec resume` too (same mechanism the resumed sandbox uses). The
// `-` sentinel reads the prompt from stdin (no ARG_MAX, no shell-quoting through ssh).
func codexArgs(opts SessionOpts, resume string, mcpArgs []string) []string {
	initial := resume == ""
	args := []string{"exec"}
	if !initial {
		args = append(args, "resume", resume)
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if initial && opts.Workdir != "" {
		args = append(args, "-C", opts.Workdir)
	}
	// the trust ladder (see the type comment): bypass > consult(read-only) > isolate(workspace-write)
	switch {
	case opts.SkipPermissions:
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case opts.Consult:
		args = append(args, sandboxArgs(initial, "read-only")...)
	case opts.Isolate:
		args = append(args, sandboxArgs(initial, "workspace-write")...)
	}
	args = append(args, mcpArgs...)
	return append(args, "-") // prompt from stdin
}

// sandboxArgs selects the sandbox flag form: `-s` exists only on `exec`; a resumed
// turn sets the same policy through the config override.
func sandboxArgs(initial bool, mode string) []string {
	if initial {
		return []string{"-s", mode}
	}
	return []string{"-c", `sandbox_mode="` + mode + `"`}
}

// codexCmd builds the transport chain — the same trust-axis shape as claudeCmd,
// minus docker (codex's sandbox is native, so isolate needs no container):
//
//	direct → codex exec …                                    (cwd = Workdir)
//	remote → ssh -T <host> bash -lc 'cd <dir> && codex exec …'  (the REMOTE box's codex)
//
// The prompt flows over stdin through either transport unchanged.
func codexCmd(ctx context.Context, bin string, opts SessionOpts, args []string) *exec.Cmd {
	if opts.Remote != "" {
		inner := strings.Join(append([]string{bin}, args...), " ")
		if opts.Workdir != "" {
			inner = "cd " + opts.Workdir + " && " + inner
		}
		// login shell for the remote's full PATH (nvm / npm-global / volta installs).
		return exec.CommandContext(ctx, "ssh", "-T", opts.Remote, "bash", "-lc", shellQuote(inner))
	}
	cmd := exec.CommandContext(ctx, resolveCodexBin(bin), args...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	return cmd
}

// resolveCodexBin mirrors resolveClaudeBin for the codex executable: an explicit
// LOOM_CODEX_BIN override, a path-y bin as given, PATH, then the install
// locations a detached daemon's PATH tends to miss (volta, npm-global, ~/.local).
func resolveCodexBin(bin string) string {
	if env := os.Getenv("LOOM_CODEX_BIN"); env != "" {
		return env
	}
	if bin == "" {
		bin = "codex"
	}
	if strings.ContainsAny(bin, `/\`) {
		return bin
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".volta", "bin", bin),
		filepath.Join(home, ".local", "bin", bin),
		filepath.Join(home, "bin", bin),
		"/usr/local/bin/" + bin,
		"/opt/homebrew/bin/" + bin,
	} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return bin
}

// Interrupt stops the in-flight turn by signalling the codex process. Unlike
// claude (an in-band control_request on a persistent process), codex is one
// process per turn and checkpoints the session to disk continuously — so killing
// the turn loses nothing durable. The session stays resumable: steer by calling
// Send with a new instruction.
func (s *codexSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.cur.Process == nil {
		return fmt.Errorf("codex: no in-flight turn to interrupt")
	}
	s.interrupted = true
	if err := s.cur.Process.Signal(os.Interrupt); err != nil {
		return s.cur.Process.Kill() // fallback: SIGINT not deliverable (already exited, or platform)
	}
	return nil
}

func (s *codexSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

func (s *codexSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.Process != nil {
		_ = s.cur.Process.Signal(os.Interrupt)
	}
	return nil
}
