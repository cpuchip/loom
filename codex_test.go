package loom

import (
	"slices"
	"strings"
	"testing"
)

// codexArgs turn shape: turn 1 is `exec` with the full flag set (-C workdir, -s
// sandbox); a resumed turn is `exec resume <id>` whose narrower flag set carries
// the sandbox via -c. Both end with the `-` stdin-prompt sentinel.
func TestCodexArgs(t *testing.T) {
	t.Run("initial turn", func(t *testing.T) {
		a := codexArgs(SessionOpts{Workdir: "/w", Model: "gpt-5.3-codex"}, "")
		for _, must := range []string{"exec", "--json", "--skip-git-repo-check", "-m", "gpt-5.3-codex", "-C", "/w"} {
			if !slices.Contains(a, must) {
				t.Errorf("initial args missing %q: %v", must, a)
			}
		}
		if slices.Contains(a, "resume") {
			t.Errorf("initial turn must not resume: %v", a)
		}
		if a[len(a)-1] != "-" {
			t.Errorf("prompt must be the stdin sentinel `-`, got %q", a[len(a)-1])
		}
	})
	t.Run("resumed turn: narrower flags, no -C/-s", func(t *testing.T) {
		a := codexArgs(SessionOpts{Workdir: "/w", Isolate: true}, "thread-123")
		if a[0] != "exec" || a[1] != "resume" || a[2] != "thread-123" {
			t.Errorf("resume argv prefix wrong: %v", a)
		}
		if slices.Contains(a, "-C") || slices.Contains(a, "-s") {
			t.Errorf("`exec resume` rejects -C/-s (verified 0.141.0); args=%v", a)
		}
		if !slices.Contains(a, `sandbox_mode="workspace-write"`) {
			t.Errorf("resumed isolate must carry the sandbox via -c: %v", a)
		}
	})
	t.Run("trust ladder", func(t *testing.T) {
		if a := codexArgs(SessionOpts{SkipPermissions: true, Consult: true, Isolate: true}, ""); !slices.Contains(a, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("SkipPermissions outranks the ladder: %v", a)
		} else if slices.Contains(a, "-s") {
			t.Errorf("bypass must not also set a sandbox: %v", a)
		}
		if a := codexArgs(SessionOpts{Consult: true, Isolate: true}, ""); !slices.Contains(a, "read-only") {
			t.Errorf("Consult means read-only enforcement, even with Isolate set: %v", a)
		}
		if a := codexArgs(SessionOpts{Isolate: true}, ""); !slices.Contains(a, "workspace-write") {
			t.Errorf("Isolate maps to the native workspace-write wall: %v", a)
		}
		if a := codexArgs(SessionOpts{}, ""); slices.Contains(a, "-s") {
			t.Errorf("no opts → codex's own default sandbox, no -s: %v", a)
		}
	})
}

// handleCodexLine against the LIVE-captured JSONL of a real two-turn session
// (codex-cli 0.141.0, 2026-07-02): thread id, tool call/result, last-message-wins
// answer text, turn count. Unknown/garbage lines are skipped.
func TestHandleCodexLine(t *testing.T) {
	lines := []string{
		`{"type":"thread.started","thread_id":"019f2557-10dd-7f21-a86f-ddeef0bf394d"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'echo hello-from-tool'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'echo hello-from-tool'","aggregated_output":"hello-from-tool\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"The exact word was PONG."}}`,
		`not json at all`,
		`{"type":"turn.completed","usage":{"input_tokens":57924,"cached_input_tokens":31360,"output_tokens":140}}`,
	}
	var r Reply
	var evs []Event
	for _, l := range lines {
		handleCodexLine([]byte(l), &r, func(e Event) { evs = append(evs, e) })
	}
	if r.SessionID != "019f2557-10dd-7f21-a86f-ddeef0bf394d" {
		t.Errorf("SessionID = %q", r.SessionID)
	}
	if r.Text != "The exact word was PONG." {
		t.Errorf("Text = %q (last agent_message wins)", r.Text)
	}
	if r.Turns != 1 || r.Err != "" {
		t.Errorf("Turns=%d Err=%q", r.Turns, r.Err)
	}
	kinds := make([]EventKind, len(evs))
	for i, e := range evs {
		kinds[i] = e.Kind
	}
	want := []EventKind{EvToolCall, EvToolResult, EvAssistant}
	if !slices.Equal(kinds, want) {
		t.Errorf("event kinds = %v, want %v", kinds, want)
	}
	if evs[0].Tool != "shell" || !strings.Contains(evs[0].Text, "echo hello-from-tool") {
		t.Errorf("tool_call event = %+v", evs[0])
	}
	if !strings.Contains(evs[1].Text, "hello-from-tool") {
		t.Errorf("tool_result should carry the aggregated output: %+v", evs[1])
	}

	t.Run("turn.failed surfaces the error", func(t *testing.T) {
		var r Reply
		handleCodexLine([]byte(`{"type":"turn.failed","error":{"message":"quota exhausted"}}`), &r, nil)
		if r.Err != "quota exhausted" {
			t.Errorf("Err = %q", r.Err)
		}
	})
}

// resolveCodexBin mirrors resolveClaudeBin — the daemon-PATH gotcha applies the
// same way (volta/npm-global installs invisible to a detached loom serve).
func TestResolveCodexBin(t *testing.T) {
	t.Setenv("LOOM_CODEX_BIN", "/custom/codex")
	if got := resolveCodexBin("codex"); got != "/custom/codex" {
		t.Errorf("LOOM_CODEX_BIN override = %q", got)
	}
	t.Setenv("LOOM_CODEX_BIN", "")
	if got := resolveCodexBin("/opt/x/codex"); got != "/opt/x/codex" {
		t.Errorf("explicit path changed: %q", got)
	}
	const missing = "loom-no-such-binary-xyzzy"
	if got := resolveCodexBin(missing); got != missing {
		t.Errorf("missing bin should fall through unchanged: %q", got)
	}
}
