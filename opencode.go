package loom

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// OpencodeBackend drives the opencode CLI (`opencode run`) as a PER-TURN exec
// session: each Send spawns `opencode run --format json` (turn 1) or adds
// `-s <sessionID>` (later turns). opencode persists sessions to disk itself, so
// multi-turn context and resume-after-restart ride its own session store — the
// same durable-session shape as codex, prompt passed as the positional message.
//
// Schema verified live (opencode-ai 1.17.15, 2026-07-08) — every event carries
// the sessionID at the top level:
//
//	{"type":"step_start","sessionID":"ses_…","part":{"type":"step-start",…}}
//	{"type":"text","sessionID":"…","part":{"type":"text","text":"PONG",…}}
//	{"type":"tool_use","part":{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"…"},"output":"…","title":"…"}}}
//	{"type":"step_finish","part":{"type":"step-finish","tokens":{…},"cost":0.00103208}}
//
// step_finish carries real USD cost (summed into Reply.CostUSD — opencode is the
// only backend that reports it) and Turns counts model steps, not user turns.
// Resume verified live: `run -s <id>` recalled a prior turn's content; the id is
// stable across turns.
//
// Trust mapping: SkipPermissions → `--auto` (auto-approve everything —
// dangerous, for externally-sandboxed environments); default = opencode's own
// permission config, where unapproved tools fail closed in headless run mode;
// Consult rides the directive. opencode has NO native filesystem sandbox, so
// Isolate is not enforceable here — wall it externally (docker/remote) when it
// matters. Claude-specific opts (MCPConfig, AllowedTools, PermissionMode,
// ClaudeHome, Image) are ignored; configure those in opencode.json instead.
// The prompt travels as one argv element: fine for worker dispatches, but
// Windows caps a command line at ~32K chars — don't feed whole flattened
// transcripts through this backend.
type OpencodeBackend struct {
	Bin string // default "opencode"
}

func (b OpencodeBackend) Name() string { return "opencode" }

func (b OpencodeBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	bin := b.Bin
	if bin == "" {
		bin = "opencode"
	}
	return &opencodeSession{bin: bin, opts: opts, sessionID: opts.Resume}, nil
}

type opencodeSession struct {
	bin  string
	opts SessionOpts

	turnMu sync.Mutex // one turn at a time; held across a SendStream turn
	mu     sync.Mutex // guards the fields below (NOT held during the read)

	sessionID   string
	firstSent   bool
	cur         *exec.Cmd
	interrupted bool
}

func (s *opencodeSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *opencodeSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.mu.Lock()
	resume := s.sessionID
	if !s.firstSent && s.opts.SystemPromptFile != "" {
		if data, err := os.ReadFile(s.opts.SystemPromptFile); err == nil && len(data) > 0 {
			prompt = "<instructions>\n" + strings.TrimSpace(string(data)) + "\n</instructions>\n\n" + prompt
		}
	}
	s.firstSent = true
	if s.opts.Consult {
		prompt = consultDirective + "\n\n" + prompt
	}
	s.interrupted = false
	s.mu.Unlock()

	cmd := onehotCmd(ctx, resolveOpencodeBin(s.bin), s.opts, opencodeArgs(s.opts, resume, prompt))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Reply{Backend: "opencode", Err: err.Error()}, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return Reply{Backend: "opencode", Err: err.Error()}, err
	}
	s.mu.Lock()
	s.cur = cmd
	s.mu.Unlock()

	r := Reply{Backend: "opencode"}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		handleOpencodeLine(sc.Bytes(), &r, onEvent)
	}
	if err := sc.Err(); err != nil && r.Err == "" {
		r.Err = err.Error()
	}
	waitErr := cmd.Wait()
	s.mu.Lock()
	s.cur = nil
	if r.SessionID != "" {
		s.sessionID = r.SessionID
	}
	wasInterrupted := s.interrupted
	s.mu.Unlock()

	if wasInterrupted && r.Err == "" {
		r.Err = "interrupted"
		emit(onEvent, Event{Kind: EvResult, Backend: "opencode", Text: r.Text})
		return r, nil
	}
	if r.Err == "" && r.Text == "" && waitErr != nil {
		r.Err = waitErr.Error()
		return r, waitErr
	}
	emit(onEvent, Event{Kind: EvResult, Backend: "opencode", Text: r.Text})
	if r.Err != "" {
		return r, fmt.Errorf("opencode: %s", r.Err)
	}
	return r, nil
}

// handleOpencodeLine parses one JSON event from `opencode run --format json`.
func handleOpencodeLine(line []byte, r *Reply, onEvent func(Event)) {
	var ev struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
		Message   string `json:"message"`
		Part      struct {
			Type  string  `json:"type"`
			Text  string  `json:"text"`
			Tool  string  `json:"tool"`
			Cost  float64 `json:"cost"`
			State struct {
				Status string `json:"status"`
				Title  string `json:"title"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"part"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return // skip non-JSON noise
	}
	if r.SessionID == "" && ev.SessionID != "" {
		r.SessionID = ev.SessionID
	}
	switch ev.Type {
	case "text":
		if ev.Part.Text != "" {
			r.Text = ev.Part.Text // the LAST text part is the turn's answer
			emit(onEvent, Event{Kind: EvAssistant, Backend: "opencode", Text: ev.Part.Text})
		}
	case "reasoning":
		if ev.Part.Text != "" {
			emit(onEvent, Event{Kind: EvThinking, Backend: "opencode", Text: ev.Part.Text})
		}
	case "tool_use":
		emit(onEvent, Event{Kind: EvToolCall, Backend: "opencode", Tool: ev.Part.Tool, Text: ev.Part.State.Title})
		if ev.Part.State.Status == "completed" {
			emit(onEvent, Event{Kind: EvToolResult, Backend: "opencode", Tool: ev.Part.Tool, Text: ev.Part.State.Output})
		}
	case "step_finish":
		r.Turns++ // a model step, not a user turn — closest available count
		r.CostUSD += ev.Part.Cost
	case "error":
		if ev.Message != "" {
			r.Err = ev.Message
		} else {
			r.Err = strings.TrimSpace(string(line))
		}
	}
}

// opencodeArgs builds the argv for one turn; the prompt is the final positional.
func opencodeArgs(opts SessionOpts, resume, prompt string) []string {
	args := []string{"run", "--format", "json"}
	if resume != "" {
		args = append(args, "-s", resume)
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.SkipPermissions {
		args = append(args, "--auto")
	}
	return append(args, prompt)
}

// resolveOpencodeBin mirrors resolveCodexBin for the opencode executable.
func resolveOpencodeBin(bin string) string {
	if env := os.Getenv("LOOM_OPENCODE_BIN"); env != "" {
		return env
	}
	if bin == "" {
		bin = "opencode"
	}
	return resolveNpmishBin(bin)
}

// resolveNpmishBin resolves a CLI that is typically npm/volta-installed: an
// explicit path is used as given; PATH next; then the install locations a
// detached daemon's PATH tends to miss (npm-global on Windows, volta, ~/.local).
func resolveNpmishBin(bin string) string {
	if strings.ContainsAny(bin, `/\`) {
		return bin
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "AppData", "Roaming", "npm", bin+".cmd"), // Windows npm-global shim
		filepath.Join(home, ".volta", "bin", bin),
		filepath.Join(home, ".local", "bin", bin),
		filepath.Join(home, "bin", bin),
		"/usr/local/bin/" + bin,
		"/opt/homebrew/bin/" + bin,
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return bin
}

// onehotCmd builds the transport chain for a one-shot argv-prompt backend
// (opencode, copilot) — the codexCmd shape, but since the prompt rides argv,
// the remote form quotes EVERY element (prompts contain spaces and newlines):
//
//	direct → <bin> …args                                        (cwd = Workdir)
//	remote → ssh -T <host> bash -lc 'cd <dir> && <bin> …args'   (the REMOTE box's CLI)
func onehotCmd(ctx context.Context, bin string, opts SessionOpts, args []string) *exec.Cmd {
	if opts.Remote != "" {
		parts := make([]string, 0, len(args)+1)
		parts = append(parts, bin)
		for _, a := range args {
			parts = append(parts, shellQuote(a))
		}
		inner := strings.Join(parts, " ")
		if opts.Workdir != "" {
			inner = "cd " + shellQuote(opts.Workdir) + " && " + inner
		}
		return exec.CommandContext(ctx, "ssh", "-T", opts.Remote, "bash", "-lc", shellQuote(inner))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if opts.Workdir != "" {
		cmd.Dir = opts.Workdir
	}
	return cmd
}

// Interrupt stops the in-flight turn by signalling the process; opencode's
// on-disk session survives, so steer by calling Send again.
func (s *opencodeSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.cur.Process == nil {
		return fmt.Errorf("opencode: no in-flight turn to interrupt")
	}
	s.interrupted = true
	if err := s.cur.Process.Signal(os.Interrupt); err != nil {
		return s.cur.Process.Kill()
	}
	return nil
}

func (s *opencodeSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *opencodeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.Process != nil {
		_ = s.cur.Process.Signal(os.Interrupt)
	}
	return nil
}
