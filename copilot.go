package loom

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CopilotBackend drives GitHub Copilot CLI (`copilot`) as a PER-TURN exec
// session: each Send spawns `copilot -p <prompt> --output-format json` (turn 1)
// or adds `--resume <sessionId>` (later turns). Copilot persists sessions
// itself; the id is STABLE across turns (verified live: a resumed turn recalled
// the prior turn's content and the final result event returned the same id).
//
// Schema verified live (copilot 1.0.69, 2026-07-08) — JSONL on stdout:
//
//	{"type":"assistant.message","data":{"content":"PONG","model":"…",…}}          (LAST one = the turn's answer)
//	{"type":"assistant.reasoning","data":{…}} / assistant.*_delta                  (deltas are ephemeral noise here)
//	{"type":"tool.execution_start","data":{"toolName":"powershell","arguments":{"command":"…"},…}}
//	{"type":"tool.execution_complete","data":{"toolCallId":"…","success":true,"result":{"content":"…"}}}
//	{"type":"assistant.turn_end","data":{…}}
//	{"type":"result","sessionId":"…","exitCode":0,"usage":{"premiumRequests":1,…}}  (the FINAL line)
//
// Trust ladder — copilot's native permission system is the wall:
//
//	SkipPermissions → --allow-all         (tools+paths+urls auto-approved; no wall)
//	Isolate         → --allow-all-tools   (tools auto-run, but file access stays
//	                                       walled to the workdir + temp — copilot's
//	                                       path verification is the workspace wall)
//	Consult         → directive only      (no allow flags: in -p mode unapproved
//	                                       tools fail closed, so a consult stays advice;
//	                                       verified: text-only turns need no allow flag)
//
// Real mappings claude-only backends lack: MCPConfig → --additional-mcp-config
// @<path> (the substrate hinge), AllowedTools → repeated --allow-tool=<t>.
// Always passed: --log-level none --no-color --no-auto-update (a worker turn
// must never self-update mid-dispatch). PermissionMode/ClaudeHome/Image are
// ignored. The prompt rides argv (Windows ~32K command-line cap applies).
//
// NOTE on installs: VS Code's copilot-chat extension ships its own copilot on
// PATH (globalStorage/…/copilotCli) which can SHADOW an npm-global install and
// lag it (1.0.34 vs 1.0.69 when this was written). Pin with LOOM_COPILOT_BIN
// when the versions matter.
type CopilotBackend struct {
	Bin string // default "copilot"
}

func (b CopilotBackend) Name() string { return "copilot" }

func (b CopilotBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	bin := b.Bin
	if bin == "" {
		bin = "copilot"
	}
	return &copilotSession{bin: bin, opts: opts, sessionID: opts.Resume}, nil
}

type copilotSession struct {
	bin  string
	opts SessionOpts

	turnMu sync.Mutex // one turn at a time; held across a SendStream turn
	mu     sync.Mutex // guards the fields below (NOT held during the read)

	sessionID   string
	firstSent   bool
	cur         *exec.Cmd
	interrupted bool
}

func (s *copilotSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *copilotSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
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

	cmd := onehotCmd(ctx, resolveCopilotBin(s.bin), s.opts, copilotArgs(s.opts, resume, prompt))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Reply{Backend: "copilot", Err: err.Error()}, err
	}
	cmd.Stderr = os.Stderr
	// StartChild (not a bare cmd.Start) so a dying loom wrapper reaps this child — see reap.go.
	if err := StartChild(cmd); err != nil {
		return Reply{Backend: "copilot", Err: err.Error()}, err
	}
	s.mu.Lock()
	s.cur = cmd
	s.mu.Unlock()

	r := Reply{Backend: "copilot"}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		handleCopilotLine(sc.Bytes(), &r, onEvent)
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
		emit(onEvent, Event{Kind: EvResult, Backend: "copilot", Text: r.Text})
		return r, nil
	}
	if r.Err == "" && r.Text == "" && waitErr != nil {
		r.Err = waitErr.Error()
		return r, waitErr
	}
	emit(onEvent, Event{Kind: EvResult, Backend: "copilot", Text: r.Text})
	if r.Err != "" {
		return r, fmt.Errorf("copilot: %s", r.Err)
	}
	return r, nil
}

// handleCopilotLine parses one JSONL event from `copilot --output-format json`.
func handleCopilotLine(line []byte, r *Reply, onEvent func(Event)) {
	var ev struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		ExitCode  *int   `json:"exitCode"`
		Data      struct {
			Content   string `json:"content"`
			Message   string `json:"message"`
			ToolName  string `json:"toolName"`
			Success   *bool  `json:"success"`
			Arguments struct {
				Command string `json:"command"`
			} `json:"arguments"`
			Result struct {
				Content string `json:"content"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return // skip non-JSON noise
	}
	switch ev.Type {
	case "assistant.message":
		if ev.Data.Content != "" {
			r.Text = ev.Data.Content // the LAST assistant message is the turn's answer
			emit(onEvent, Event{Kind: EvAssistant, Backend: "copilot", Text: ev.Data.Content})
		}
	case "assistant.reasoning":
		if ev.Data.Content != "" {
			emit(onEvent, Event{Kind: EvThinking, Backend: "copilot", Text: ev.Data.Content})
		}
	case "tool.execution_start":
		text := ev.Data.Arguments.Command
		emit(onEvent, Event{Kind: EvToolCall, Backend: "copilot", Tool: ev.Data.ToolName, Text: text})
	case "tool.execution_complete":
		emit(onEvent, Event{Kind: EvToolResult, Backend: "copilot", Text: ev.Data.Result.Content})
	case "assistant.turn_end":
		r.Turns++
	case "result":
		r.SessionID = ev.SessionID
		if ev.ExitCode != nil && *ev.ExitCode != 0 && r.Err == "" {
			r.Err = fmt.Sprintf("copilot exited %d", *ev.ExitCode)
		}
	case "error":
		if ev.Data.Message != "" {
			r.Err = ev.Data.Message
		} else {
			r.Err = strings.TrimSpace(string(line))
		}
	}
}

// copilotArgs builds the argv for one turn; the prompt rides -p.
func copilotArgs(opts SessionOpts, resume, prompt string) []string {
	args := []string{"-p", prompt, "--output-format", "json", "--log-level", "none", "--no-color", "--no-auto-update"}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	switch {
	case opts.SkipPermissions:
		args = append(args, "--allow-all")
	case opts.Isolate:
		args = append(args, "--allow-all-tools")
	}
	if opts.MCPConfig != "" {
		args = append(args, "--additional-mcp-config", "@"+opts.MCPConfig)
	}
	for _, t := range strings.FieldsFunc(opts.AllowedTools, func(r rune) bool { return r == ',' || r == ' ' }) {
		args = append(args, "--allow-tool="+t)
	}
	return args
}

// resolveCopilotBin mirrors resolveCodexBin for the copilot executable.
func resolveCopilotBin(bin string) string {
	if env := os.Getenv("LOOM_COPILOT_BIN"); env != "" {
		return env
	}
	if bin == "" {
		bin = "copilot"
	}
	return resolveNpmishBin(bin)
}

// Interrupt stops the in-flight turn by signalling the process; copilot's
// on-disk session survives, so steer by calling Send again.
func (s *copilotSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.cur.Process == nil {
		return fmt.Errorf("copilot: no in-flight turn to interrupt")
	}
	s.interrupted = true
	if err := s.cur.Process.Signal(os.Interrupt); err != nil {
		return s.cur.Process.Kill()
	}
	return nil
}

func (s *copilotSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *copilotSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.Process != nil {
		_ = s.cur.Process.Signal(os.Interrupt)
	}
	return nil
}
