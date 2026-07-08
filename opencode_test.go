package loom

import (
	"slices"
	"strings"
	"testing"
)

// opencodeArgs: `run --format json`, -s only when resuming, --auto only under
// SkipPermissions, prompt as the final positional.
func TestOpencodeArgs(t *testing.T) {
	a := opencodeArgs(SessionOpts{Model: "opencode-go/glm-5.2"}, "", "hi there")
	for _, must := range []string{"run", "--format", "json", "-m", "opencode-go/glm-5.2"} {
		if !slices.Contains(a, must) {
			t.Errorf("initial args missing %q: %v", must, a)
		}
	}
	if slices.Contains(a, "-s") || slices.Contains(a, "--auto") {
		t.Errorf("fresh unprivileged turn must not resume or auto-approve: %v", a)
	}
	if a[len(a)-1] != "hi there" {
		t.Errorf("prompt must be the final positional, got %q", a[len(a)-1])
	}

	a = opencodeArgs(SessionOpts{SkipPermissions: true}, "ses_abc", "again")
	if i := slices.Index(a, "-s"); i < 0 || a[i+1] != "ses_abc" {
		t.Errorf("resume must ride -s <id>: %v", a)
	}
	if !slices.Contains(a, "--auto") {
		t.Errorf("SkipPermissions maps to --auto: %v", a)
	}
}

// handleOpencodeLine against LIVE-captured events (opencode-ai 1.17.15,
// 2026-07-08): session id from any event, last-text-wins answer, tool part →
// call+result events, step_finish accumulates USD cost and step count.
func TestHandleOpencodeLine(t *testing.T) {
	lines := []string{
		`{"type":"step_start","timestamp":1783551962820,"sessionID":"ses_0bc04d637ffeANBvplbGS6ptFC","part":{"id":"prt_1","messageID":"msg_1","sessionID":"ses_0bc04d637ffeANBvplbGS6ptFC","type":"step-start"}}`,
		`{"type":"text","timestamp":1783552094000,"sessionID":"ses_0bc04d637ffeANBvplbGS6ptFC","part":{"id":"prt_2","type":"text","text":"I'll run that now."}}`,
		`{"type":"tool_use","timestamp":1783552094409,"sessionID":"ses_0bc04d637ffeANBvplbGS6ptFC","part":{"type":"tool","tool":"bash","callID":"call_00","state":{"status":"completed","input":{"command":"echo loom-probe"},"output":"loom-probe\n","title":"echo loom-probe"},"id":"prt_3"}}`,
		`not json at all`,
		`{"type":"text","timestamp":1783551963942,"sessionID":"ses_0bc04d637ffeANBvplbGS6ptFC","part":{"id":"prt_4","type":"text","text":"DONE"}}`,
		`{"type":"step_finish","timestamp":1783551963942,"sessionID":"ses_0bc04d637ffeANBvplbGS6ptFC","part":{"id":"prt_5","reason":"stop","type":"step-finish","tokens":{"total":7348,"input":7324,"output":3},"cost":0.00103208}}`,
	}
	var r Reply
	var evs []Event
	for _, l := range lines {
		handleOpencodeLine([]byte(l), &r, func(e Event) { evs = append(evs, e) })
	}
	if r.SessionID != "ses_0bc04d637ffeANBvplbGS6ptFC" {
		t.Errorf("SessionID = %q", r.SessionID)
	}
	if r.Text != "DONE" {
		t.Errorf("Text = %q (last text part wins)", r.Text)
	}
	if r.Turns != 1 || r.CostUSD != 0.00103208 || r.Err != "" {
		t.Errorf("Turns=%d CostUSD=%v Err=%q", r.Turns, r.CostUSD, r.Err)
	}
	kinds := make([]EventKind, len(evs))
	for i, e := range evs {
		kinds[i] = e.Kind
	}
	want := []EventKind{EvAssistant, EvToolCall, EvToolResult, EvAssistant}
	if !slices.Equal(kinds, want) {
		t.Errorf("event kinds = %v, want %v", kinds, want)
	}
	if evs[1].Tool != "bash" || !strings.Contains(evs[2].Text, "loom-probe") {
		t.Errorf("tool events = %+v / %+v", evs[1], evs[2])
	}
}

func TestResolveOpencodeBin(t *testing.T) {
	t.Setenv("LOOM_OPENCODE_BIN", "/custom/opencode")
	if got := resolveOpencodeBin("opencode"); got != "/custom/opencode" {
		t.Errorf("LOOM_OPENCODE_BIN override = %q", got)
	}
	t.Setenv("LOOM_OPENCODE_BIN", "")
	if got := resolveOpencodeBin("/opt/x/opencode"); got != "/opt/x/opencode" {
		t.Errorf("explicit path changed: %q", got)
	}
}
