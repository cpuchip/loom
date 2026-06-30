package loom

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	// --verbose is REQUIRED with --print + --output-format stream-json.
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if s.opts.Model != "" {
		args = append(args, "--model", s.opts.Model)
	}
	cmd := exec.CommandContext(ctx, s.bin, args...)
	if s.opts.Workdir != "" {
		cmd.Dir = s.opts.Workdir
	}
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
	// read events until THIS turn's result event
	for s.sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(s.sc.Bytes(), &ev) != nil {
			continue // skip any non-JSON noise
		}
		if ev["type"] != "result" {
			continue
		}
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
		return r, nil
	}
	if err := s.sc.Err(); err != nil {
		return Reply{Backend: "claude", Err: err.Error()}, err
	}
	return Reply{Backend: "claude", Err: "stream ended before result"},
		fmt.Errorf("claude: stream ended before a result event (process exited?)")
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
