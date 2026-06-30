package loom

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
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
