package loom

import (
	"slices"
	"strings"
	"testing"
)

// copilotArgs: -p prompt + JSONL output + the always-on hygiene flags; the trust
// ladder maps onto copilot's native permission system.
func TestCopilotArgs(t *testing.T) {
	a := copilotArgs(SessionOpts{Model: "claude-sonnet-4.6"}, "", "hi")
	for _, must := range []string{"-p", "hi", "--output-format", "json", "--log-level", "none", "--no-color", "--no-auto-update", "--model", "claude-sonnet-4.6"} {
		if !slices.Contains(a, must) {
			t.Errorf("args missing %q: %v", must, a)
		}
	}
	if slices.Contains(a, "--resume") || slices.Contains(a, "--allow-all-tools") || slices.Contains(a, "--allow-all") {
		t.Errorf("fresh unprivileged turn must not resume or auto-allow: %v", a)
	}

	a = copilotArgs(SessionOpts{Isolate: true}, "4317640b", "again")
	if i := slices.Index(a, "--resume"); i < 0 || a[i+1] != "4317640b" {
		t.Errorf("resume must ride --resume <id>: %v", a)
	}
	if !slices.Contains(a, "--allow-all-tools") || slices.Contains(a, "--allow-all") {
		t.Errorf("Isolate = tools auto-run but paths stay walled (--allow-all-tools only): %v", a)
	}

	if a = copilotArgs(SessionOpts{SkipPermissions: true, Isolate: true}, "", "x"); !slices.Contains(a, "--allow-all") {
		t.Errorf("SkipPermissions outranks the ladder (--allow-all): %v", a)
	}

	a = copilotArgs(SessionOpts{MCPConfig: "/tmp/mcp.json", AllowedTools: "powershell,view"}, "", "x")
	if i := slices.Index(a, "--additional-mcp-config"); i < 0 || a[i+1] != "@/tmp/mcp.json" {
		t.Errorf("MCPConfig must ride --additional-mcp-config @<path>: %v", a)
	}
	if !slices.Contains(a, "--allow-tool=powershell") || !slices.Contains(a, "--allow-tool=view") {
		t.Errorf("AllowedTools must expand to repeated --allow-tool flags: %v", a)
	}
}

// handleCopilotLine against LIVE-captured JSONL (copilot 1.0.69, 2026-07-08):
// last assistant.message wins, tool start/complete map to call/result, the final
// result line carries the stable session id, deltas and session noise are skipped.
func TestHandleCopilotLine(t *testing.T) {
	lines := []string{
		`{"type":"session.mcp_servers_loaded","data":{"servers":[]},"id":"x","ephemeral":true}`,
		`{"type":"user.message","data":{"content":"Run the shell command: echo loom-probe. Then reply with exactly: DONE"},"id":"y"}`,
		`{"type":"assistant.message_delta","data":{"messageId":"m1","deltaContent":"I'll"},"id":"z","ephemeral":true}`,
		`{"type":"assistant.message","data":{"messageId":"m1","model":"claude-sonnet-4.6","content":"I'll run that command."},"id":"a1"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"powershell","arguments":{"command":"echo loom-probe","description":"Run echo loom-probe"},"model":"claude-sonnet-4.6"},"id":"a2"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true,"result":{"content":"loom-probe\n<shellId: 0 completed with exit code 0>"}},"id":"a3"}`,
		`{"type":"assistant.message","data":{"messageId":"m2","model":"claude-sonnet-4.6","content":"DONE"},"id":"a4"}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0","model":"claude-sonnet-4.6"},"id":"a5"}`,
		`{"type":"result","timestamp":"2026-07-08T23:08:25.611Z","sessionId":"4317640b-3288-437a-81c4-5709c79bb3e1","exitCode":0,"usage":{"premiumRequests":1}}`,
	}
	var r Reply
	var evs []Event
	for _, l := range lines {
		handleCopilotLine([]byte(l), &r, func(e Event) { evs = append(evs, e) })
	}
	if r.SessionID != "4317640b-3288-437a-81c4-5709c79bb3e1" {
		t.Errorf("SessionID = %q", r.SessionID)
	}
	if r.Text != "DONE" {
		t.Errorf("Text = %q (last assistant.message wins)", r.Text)
	}
	if r.Turns != 1 || r.Err != "" {
		t.Errorf("Turns=%d Err=%q", r.Turns, r.Err)
	}
	kinds := make([]EventKind, len(evs))
	for i, e := range evs {
		kinds[i] = e.Kind
	}
	want := []EventKind{EvAssistant, EvToolCall, EvToolResult, EvAssistant}
	if !slices.Equal(kinds, want) {
		t.Errorf("event kinds = %v, want %v", kinds, want)
	}
	if evs[1].Tool != "powershell" || !strings.Contains(evs[1].Text, "echo loom-probe") {
		t.Errorf("tool_call event = %+v", evs[1])
	}

	t.Run("nonzero exitCode surfaces as the error", func(t *testing.T) {
		var r Reply
		handleCopilotLine([]byte(`{"type":"result","sessionId":"s","exitCode":3,"usage":{}}`), &r, nil)
		if r.Err == "" || !strings.Contains(r.Err, "3") {
			t.Errorf("Err = %q", r.Err)
		}
	})
}

func TestResolveCopilotBin(t *testing.T) {
	t.Setenv("LOOM_COPILOT_BIN", "/custom/copilot")
	if got := resolveCopilotBin("copilot"); got != "/custom/copilot" {
		t.Errorf("LOOM_COPILOT_BIN override = %q", got)
	}
	t.Setenv("LOOM_COPILOT_BIN", "")
	if got := resolveCopilotBin("/opt/x/copilot"); got != "/opt/x/copilot" {
		t.Errorf("explicit path changed: %q", got)
	}
}
