package loom

// usage_test.go — fixture-driven tests for the uniform usage accounting. Every
// fixture line below was LIVE-CAPTURED 2026-07-20 on this box (tiny "reply PONG"
// turns against the real CLIs), then trimmed to the fields under test — so the
// parsers are pinned to what the harnesses actually emit, not to documentation.

import (
	"encoding/json"
	"strings"
	"testing"
)

// codex-cli 0.14x `codex exec --json`, live capture: input_tokens INCLUDES the
// cached portion — the parser must subtract so InputTokens = fresh input.
func TestCodexUsageParsing(t *testing.T) {
	r := Reply{Backend: "codex"}
	lines := []string{
		`{"type":"thread.started","thread_id":"019f82f5-ca47-7602-8d2b-d8d0eaaa7317"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"PONG"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":12445,"cached_input_tokens":10496,"output_tokens":6,"reasoning_output_tokens":0}}`,
	}
	for _, ln := range lines {
		handleCodexLine([]byte(ln), &r, nil)
	}
	u := r.Usage
	if u == nil {
		t.Fatal("no usage parsed from turn.completed")
	}
	if u.InputTokens != 12445-10496 {
		t.Errorf("InputTokens = %d, want %d (fresh input = input - cached)", u.InputTokens, 12445-10496)
	}
	if u.CacheReadTokens != 10496 || u.OutputTokens != 6 {
		t.Errorf("cacheRead=%d output=%d, want 10496/6", u.CacheReadTokens, u.OutputTokens)
	}
	if u.CostSource != CostNone || u.CostUSD != 0 {
		t.Errorf("codex reports no USD — CostSource=%q CostUSD=%v, want none/0", u.CostSource, u.CostUSD)
	}
	if u.TotalTokens() != 12445+6 {
		t.Errorf("TotalTokens = %d, want %d", u.TotalTokens(), 12445+6)
	}
}

// A multi-turn codex Send (turn.completed per turn) accumulates.
func TestCodexUsageAccumulates(t *testing.T) {
	r := Reply{Backend: "codex"}
	ln := `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10}}`
	handleCodexLine([]byte(ln), &r, nil)
	handleCodexLine([]byte(ln), &r, nil)
	if r.Usage.InputTokens != 120 || r.Usage.CacheReadTokens != 80 || r.Usage.OutputTokens != 20 {
		t.Errorf("accumulated usage = %+v", r.Usage)
	}
	if r.Turns != 2 {
		t.Errorf("turns = %d, want 2", r.Turns)
	}
}

// copilot 1.0.69 live capture: result.usage carries premiumRequests (+durations,
// codeChanges) and NO token counts; assistant.message events each carry
// outputTokens. The parser records what exists and invents nothing.
func TestCopilotUsageParsing(t *testing.T) {
	r := Reply{Backend: "copilot"}
	lines := []string{
		`{"type":"assistant.message","data":{"messageId":"m1","model":"x","content":"PONG","outputTokens":30}}`,
		`{"type":"result","timestamp":"2026-07-21T04:36:22.204Z","sessionId":"e8ca005f","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":2081,"sessionDurationMs":4954,"codeChanges":{"linesAdded":0,"linesRemoved":0,"filesModified":[]}}}`,
	}
	for _, ln := range lines {
		handleCopilotLine([]byte(ln), &r, nil)
	}
	u := r.Usage
	if u == nil {
		t.Fatal("no usage parsed")
	}
	if u.OutputTokens != 30 || u.PremiumRequests != 1 {
		t.Errorf("output=%d premium=%d, want 30/1", u.OutputTokens, u.PremiumRequests)
	}
	if u.InputTokens != 0 {
		t.Errorf("copilot emits no input tokens — InputTokens=%d must stay 0, never invented", u.InputTokens)
	}
	if u.CostSource != CostNone {
		t.Errorf("CostSource = %q, want none", u.CostSource)
	}
}

// opencode 1.17.x live capture: step_finish carries part.tokens + REAL USD cost.
func TestOpencodeUsageParsing(t *testing.T) {
	r := Reply{Backend: "opencode"}
	ln := `{"type":"step_finish","timestamp":1784608585145,"sessionID":"ses_07d0","part":{"id":"prt_1","reason":"stop","type":"step-finish","tokens":{"total":6405,"input":6363,"output":42,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.00621285}}`
	handleOpencodeLine([]byte(ln), &r, nil)
	u := r.Usage
	if u == nil {
		t.Fatal("no usage parsed")
	}
	if u.InputTokens != 6363 || u.OutputTokens != 42 {
		t.Errorf("tokens = %+v, want input 6363 output 42", u)
	}
	if u.CostSource != CostReal || u.CostUSD != 0.00621285 {
		t.Errorf("cost = %q/%v, want real/0.00621285", u.CostSource, u.CostUSD)
	}
	// a second step accumulates BOTH meters (and reasoning folds into output)
	ln2 := `{"type":"step_finish","part":{"type":"step-finish","tokens":{"input":100,"output":10,"reasoning":5,"cache":{"write":7,"read":3}},"cost":0.001}}`
	handleOpencodeLine([]byte(ln2), &r, nil)
	u = r.Usage
	if u.InputTokens != 6463 || u.OutputTokens != 57 || u.CacheWriteTokens != 7 || u.CacheReadTokens != 3 {
		t.Errorf("accumulated = %+v", u)
	}
	if u.CostUSD != 0.00721285 {
		t.Errorf("accumulated cost = %v", u.CostUSD)
	}
}

// claude v2.1.x live capture: the result event's usage is PER TURN and
// input_tokens already excludes cache reads; cost is the caller-computed delta.
func TestClaudeUsageParsing(t *testing.T) {
	const line = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"PONG","session_id":"dd0ec4c4","total_cost_usd":0.0167557,"usage":{"input_tokens":10,"cache_creation_input_tokens":6904,"cache_read_input_tokens":21147,"output_tokens":46}}`
	var ev map[string]any
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}
	u := parseClaudeUsage(ev, 0.0167557)
	if u == nil {
		t.Fatal("no usage parsed")
	}
	if u.InputTokens != 10 || u.OutputTokens != 46 || u.CacheReadTokens != 21147 || u.CacheWriteTokens != 6904 {
		t.Errorf("usage = %+v", u)
	}
	if u.CostSource != CostReal || u.CostUSD != 0.0167557 {
		t.Errorf("cost = %q/%v", u.CostSource, u.CostUSD)
	}
	// an event with no usage block (older CLI) yields nil, not zeros-as-fact
	if got := parseClaudeUsage(map[string]any{"type": "result"}, 0.1); got != nil {
		t.Errorf("no-usage event should parse to nil, got %+v", got)
	}
}

func TestAddUsage(t *testing.T) {
	a := &Usage{InputTokens: 1, OutputTokens: 2, CostUSD: 0.5, CostSource: CostReal}
	b := &Usage{InputTokens: 10, OutputTokens: 20, CostSource: CostNone}
	if got := addUsage(nil, a); got != a {
		t.Error("addUsage(nil, a) should be a")
	}
	if got := addUsage(a, nil); got != a {
		t.Error("addUsage(a, nil) should be a")
	}
	sum := addUsage(a, b)
	if sum.InputTokens != 11 || sum.OutputTokens != 22 || sum.CostUSD != 0.5 {
		t.Errorf("sum = %+v", sum)
	}
	if sum.CostSource != CostNone {
		t.Errorf("mixed real+none must degrade to none (a partial dollar figure is not the whole story); got %q", sum.CostSource)
	}
	if s2 := addUsage(a, a); s2.CostSource != CostReal {
		t.Errorf("real+real stays real; got %q", s2.CostSource)
	}
}

// TestBudget locks the two-meter semantics: real-USD turns count dollars,
// token-only turns count tokens, EITHER meter crossing the one limit trips it,
// and a nil budget never refuses.
func TestBudget(t *testing.T) {
	var nilB *Budget
	if nilB.Exceeded() || !nilB.Allow() {
		t.Fatal("nil budget must never refuse")
	}
	nilB.Note(Reply{CostUSD: 999}) // must not panic
	if NewBudget(0) != nil || NewBudget(-1) != nil {
		t.Fatal("limit <= 0 means no budget")
	}

	b := NewBudget(1.0) // $1 or 1 token — deliberately tiny for the token side
	b.Note(Reply{Usage: &Usage{CostUSD: 0.4, CostSource: CostReal}})
	if b.Exceeded() {
		t.Fatalf("$0.40 of $1 must not trip: %s", b)
	}
	b.Note(Reply{Usage: &Usage{CostUSD: 0.7, CostSource: CostReal}})
	if !b.Exceeded() {
		t.Fatalf("$1.10 of $1 must trip: %s", b)
	}

	tb := NewBudget(1000)
	tb.Note(Reply{Usage: &Usage{InputTokens: 600, OutputTokens: 300, CostSource: CostNone}})
	if tb.Exceeded() {
		t.Fatalf("900 of 1000 tokens must not trip: %s", tb)
	}
	tb.Note(Reply{Usage: &Usage{InputTokens: 200, CostSource: CostNone}})
	if !tb.Exceeded() {
		t.Fatalf("1100 of 1000 tokens must trip: %s", tb)
	}

	// mixed loop: either meter can trip against the same numeric limit
	mb := NewBudget(500)
	mb.Note(Reply{Usage: &Usage{InputTokens: 400, CostSource: CostNone}}) // 400 tokens
	mb.Note(Reply{Usage: &Usage{CostUSD: 0.2, CostSource: CostReal}})     // $0.20
	if mb.Exceeded() {
		t.Fatalf("neither meter crossed 500: %s", mb)
	}
	mb.Note(Reply{Usage: &Usage{InputTokens: 200, CostSource: CostNone}}) // tokens: 600 > 500
	if !mb.Exceeded() {
		t.Fatalf("token meter crossed: %s", mb)
	}

	// legacy replies (no Usage struct, bare CostUSD) still count dollars
	lb := NewBudget(0.5)
	lb.Note(Reply{CostUSD: 0.6})
	if !lb.Exceeded() {
		t.Fatalf("legacy CostUSD must count: %s", lb)
	}
}

// TestReplyUsageJSON locks the wire shape: usage rides the Reply JSON with
// stable keys, and a nil Usage is absent entirely.
func TestReplyUsageJSON(t *testing.T) {
	b, err := json.Marshal(Reply{Backend: "claude", Text: "hi",
		Usage: &Usage{InputTokens: 5, OutputTokens: 7, CacheReadTokens: 9, CostUSD: 0.01, CostSource: CostReal}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"usage":{`, `"input_tokens":5`, `"output_tokens":7`, `"cache_read_tokens":9`, `"cost_usd":0.01`, `"cost_source":"real"`} {
		if !strings.Contains(s, want) {
			t.Errorf("Reply JSON missing %q: %s", want, s)
		}
	}
	b, _ = json.Marshal(Reply{Backend: "agy", Text: "hi"})
	if strings.Contains(string(b), "usage") {
		t.Errorf("nil usage must be omitted: %s", b)
	}
}
