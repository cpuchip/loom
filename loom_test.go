package loom

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestResultParsing is a pure unit test: the claude `result` event shape we parse.
// No process, no network, no money.
func TestResultParsing(t *testing.T) {
	const ev = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"42","session_id":"abc-123","total_cost_usd":0.036}`
	var m map[string]any
	if err := json.Unmarshal([]byte(ev), &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "result" {
		t.Fatalf("type = %v", m["type"])
	}
	if got, _ := m["result"].(string); got != "42" {
		t.Errorf("result = %q, want 42", got)
	}
	if got, _ := m["session_id"].(string); got != "abc-123" {
		t.Errorf("session_id = %q", got)
	}
	if got, _ := m["total_cost_usd"].(float64); got != 0.036 {
		t.Errorf("cost = %v", got)
	}
}

func TestConvIDFromPath(t *testing.T) {
	brain := `C:\Users\x\.gemini\antigravity-cli\brain`
	p := brain + `\conv-9f8\.system_generated\logs\transcript.jsonl`
	if got := convIDFromPath(p, brain); got != "conv-9f8" {
		t.Errorf("convID = %q, want conv-9f8", got)
	}
}

// TestStripThink locks the CoT-stripping (incl. the orphan </think> case that a
// loom self-review caught — qwen3.x/vLLM seed the opening tag in the prompt).
func TestStripThink(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<think>reason</think>answer", "answer"},          // matched pair
		{"reason</think>answer", "answer"},                 // orphan closing tag
		{"<think>a</think>x<think>b</think>y", "xy"},        // multiple pairs
		{"<think>only reasoning</think>", ""},              // reasoning only
		{"no tags at all", "no tags at all"},               // untagged → unchanged
		{"  spaced answer  ", "spaced answer"},             // trims
	}
	for _, c := range cases {
		if got := stripThink(c.in); got != c.want {
			t.Errorf("stripThink(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestClaudeCmd locks the transport tree (direct / isolate / remote) — the command
// construction, verifiable without touching docker or ssh.
func TestClaudeCmd(t *testing.T) {
	ctx := context.Background()
	args := []string{"-p", "--input-format", "stream-json"}

	// direct: claude … with cwd = Workdir
	c := claudeCmd(ctx, "claude", SessionOpts{Workdir: "/repo"}, args)
	if c.Args[0] != "claude" || c.Dir != "/repo" {
		t.Errorf("direct: args0=%q dir=%q", c.Args[0], c.Dir)
	}

	// isolate: docker run … loom-claude claude … (Windows path → forward slashes)
	c = claudeCmd(ctx, "claude", SessionOpts{Isolate: true, Workdir: `C:\repo`}, args)
	j := strings.Join(c.Args, " ")
	if c.Args[0] != "docker" || !strings.Contains(j, "loom-claude") || !strings.Contains(j, "C:/repo:/work") {
		t.Errorf("isolate: %v", c.Args)
	}

	// isolate + claude-home: the home is mounted as the container's ~/.claude
	// (writable → persisted sessions + skills/instructions injection)
	c = claudeCmd(ctx, "claude", SessionOpts{Isolate: true, Workdir: `C:\repo`, ClaudeHome: `C:\cfg`}, args)
	if !strings.Contains(strings.Join(c.Args, " "), "C:/cfg:/root/.claude") {
		t.Errorf("isolate+claude-home: %v", c.Args)
	}

	// remote: ssh -T host bash -lc 'cd <dir> && claude …' — the login shell (-lc) is
	// load-bearing: a plain non-interactive ssh command misses the remote's claude PATH.
	c = claudeCmd(ctx, "claude", SessionOpts{Remote: "cpuchip@box", Workdir: "/r"}, args)
	j = strings.Join(c.Args, " ")
	if c.Args[0] != "ssh" || !strings.Contains(j, "-T cpuchip@box") ||
		!strings.Contains(j, "bash -lc") || !strings.Contains(j, "cd /r && claude -p") {
		t.Errorf("remote: %v", c.Args)
	}

	// remote+isolate: ssh → docker on the REMOTE, paths resolved there ($HOME, remote --dir)
	c = claudeCmd(ctx, "claude", SessionOpts{Remote: "cpuchip@box", Isolate: true, Workdir: "/r"}, args)
	j = strings.Join(c.Args, " ")
	if c.Args[0] != "ssh" || !strings.Contains(j, "bash -lc") || !strings.Contains(j, "docker run") ||
		!strings.Contains(j, "loom-claude") || !strings.Contains(j, "/r:/work") ||
		!strings.Contains(j, "$HOME/.claude/.credentials.json") {
		t.Errorf("remote+isolate: %v", c.Args)
	}
	// remote+isolate without --dir falls back to the remote $HOME
	c = claudeCmd(ctx, "claude", SessionOpts{Remote: "cpuchip@box", Isolate: true}, args)
	if !strings.Contains(strings.Join(c.Args, " "), "$HOME:/work") {
		t.Errorf("remote+isolate no --dir should mount $HOME: %v", c.Args)
	}
}

// TestClaudeArgs locks the persistent-session flags and the --resume wiring.
func TestClaudeArgs(t *testing.T) {
	base := strings.Join(claudeArgs(SessionOpts{}), " ")
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--verbose"} {
		if !strings.Contains(base, want) {
			t.Errorf("base args missing %q: %s", want, base)
		}
	}
	if strings.Contains(base, "--resume") || strings.Contains(base, "--model") {
		t.Errorf("empty opts should not add --resume/--model: %s", base)
	}
	withResume := strings.Join(claudeArgs(SessionOpts{Model: "haiku", Resume: "sess-abc"}), " ")
	if !strings.Contains(withResume, "--model haiku") || !strings.Contains(withResume, "--resume sess-abc") {
		t.Errorf("resume args: %s", withResume)
	}

	// the substrate-integration passthroughs (the hinge + headless + instructions)
	full := strings.Join(claudeArgs(SessionOpts{
		MCPConfig: "/cfg/mcp.json", AllowedTools: "mcp__pg,Bash", PermissionMode: "acceptEdits",
		SkipPermissions: true, SystemPromptFile: "/cfg/sys.md",
	}), " ")
	for _, want := range []string{
		"--mcp-config /cfg/mcp.json", "--allowed-tools mcp__pg,Bash", "--permission-mode acceptEdits",
		"--dangerously-skip-permissions", "--append-system-prompt-file /cfg/sys.md",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("config args missing %q: %s", want, full)
		}
	}
}

func TestBackendsRegistry(t *testing.T) {
	bs := Backends()
	if _, ok := bs["claude"]; !ok {
		t.Error("claude backend missing")
	}
	if _, ok := bs["agy"]; !ok {
		t.Error("agy backend missing")
	}
}

// TestClaudeMultiTurnSmoke drives the REAL claude binary — it spends a little
// money, so it is opt-in via LOOM_SMOKE=1. It is the loom oracle: proves the
// persistent stream-json session holds context across turns (the reason loom
// exists). Mirrors the 2026-06-29 manual probe (remember 42 → recall 42).
func TestClaudeMultiTurnSmoke(t *testing.T) {
	if os.Getenv("LOOM_SMOKE") != "1" {
		t.Skip("set LOOM_SMOKE=1 to run the live claude smoke test (spends a little money)")
	}
	b := ClaudeBackend{Bin: "claude"}
	sess, err := b.Open(context.Background(), SessionOpts{Model: "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Send(context.Background(), "Remember this number: 42. Acknowledge with just: OK"); err != nil {
		t.Fatal(err)
	}
	r, err := sess.Send(context.Background(), "What number did I ask you to remember one message ago? Reply with ONLY the digits.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "42") {
		t.Fatalf("turn 2 did not recall the number across turns; got %q", r.Text)
	}
	if r.SessionID == "" {
		t.Error("expected a stable session_id")
	}
	t.Logf("multi-turn OK: turn2=%q session=%s cost=$%.4f", r.Text, r.SessionID, r.CostUSD)
}

// TestClaudeResumeSmoke is the durable-session oracle: process A remembers a
// number and EXITS; a FRESH process B resumes that session by id and recalls it
// across the restart. This is what makes a remote session survive a dropped pipe.
// Opt-in via LOOM_SMOKE=1 (spends a little money).
func TestClaudeResumeSmoke(t *testing.T) {
	if os.Getenv("LOOM_SMOKE") != "1" {
		t.Skip("set LOOM_SMOKE=1 to run the live resume smoke (spends a little money)")
	}
	b := ClaudeBackend{Bin: "claude"}
	// process A: remember, capture the session id, then close (EOF → exit, saved)
	a, err := b.Open(context.Background(), SessionOpts{Model: "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(context.Background(), "Remember this number: 73. Reply with just: OK"); err != nil {
		t.Fatal(err)
	}
	id := a.SessionID()
	if id == "" {
		t.Fatal("no session id from process A")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	// process B: a FRESH backend, resume the SAME id, recall across the restart
	c, err := b.Open(context.Background(), SessionOpts{Model: "haiku", Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Send(context.Background(), "What number did I ask you to remember? Reply with ONLY the digits.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "73") {
		t.Fatalf("resumed session did not recall across the process restart; got %q", r.Text)
	}
	t.Logf("resume OK across processes: id=%s turn=%q", id, r.Text)
}

// TestClaudeInterruptSmoke proves a turn in flight can be STOPPED, and the session
// then STEERED (a fresh instruction on the still-live session). The interrupt
// terminates the turn with is_error (subtype error_during_execution) — a clean,
// deterministic signal, not a timing guess. Opt-in via LOOM_SMOKE=1.
func TestClaudeInterruptSmoke(t *testing.T) {
	if os.Getenv("LOOM_SMOKE") != "1" {
		t.Skip("set LOOM_SMOKE=1 to run the live interrupt smoke (spends a little money)")
	}
	short := func(s string) string {
		if len(s) > 48 {
			return s[:48] + "…"
		}
		return s
	}
	b := ClaudeBackend{Bin: "claude"}
	sess, err := b.Open(context.Background(), SessionOpts{Model: "haiku"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	it, ok := sess.(Interruptible)
	if !ok {
		t.Fatal("claude session should be Interruptible")
	}

	// a long task (many tokens, no tools) → still generating at +2s when we interrupt
	done := make(chan Reply, 1)
	go func() {
		r, _ := sess.SendStream(context.Background(),
			"Count from 1 to 200. Put each number on its own line and after each write one full sentence about it. Do not use any tools.", nil)
		done <- r
	}()

	time.Sleep(2 * time.Second)
	tInt := time.Now()
	if err := it.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	select {
	case r := <-done:
		// A clean completion has Err=="" (subtype success); an interrupt sets is_error.
		if r.Err == "" {
			t.Fatalf("turn completed instead of interrupting (Err empty); text=%q", short(r.Text))
		}
		t.Logf("interrupted in %.1fs (err=%q)", time.Since(tInt).Seconds(), r.Err)
	case <-time.After(45 * time.Second):
		t.Fatal("turn did not return within 45s of interrupt")
	}

	// STEER: the subprocess is still alive — a new instruction must work
	r, err := sess.Send(context.Background(), "Never mind that. Reply with ONLY the single word: ALIVE")
	if err != nil {
		t.Fatalf("steer send after interrupt: %v", err)
	}
	if !strings.Contains(strings.ToUpper(r.Text), "ALIVE") {
		t.Fatalf("session not steerable after interrupt; got %q", short(r.Text))
	}
	t.Logf("steer OK after interrupt: %q", short(r.Text))
}
