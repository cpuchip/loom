package loom

import (
	"slices"
	"strings"
	"testing"
)

// resolveClaudeBin lets a detached daemon (loom serve) find claude even when its
// PATH misses ~/.local/bin. Order: LOOM_CLAUDE_BIN → path-y bin as-is → PATH lookup
// → common install dirs → fall back to the input.
func TestResolveClaudeBin(t *testing.T) {
	t.Run("env override wins", func(t *testing.T) {
		t.Setenv("LOOM_CLAUDE_BIN", "/custom/claude")
		if got := resolveClaudeBin("claude"); got != "/custom/claude" {
			t.Errorf("LOOM_CLAUDE_BIN override = %q, want /custom/claude", got)
		}
	})
	t.Run("explicit path used as given", func(t *testing.T) {
		t.Setenv("LOOM_CLAUDE_BIN", "")
		for _, p := range []string{"/opt/x/claude", "./claude", `C:\tools\claude.exe`} {
			if got := resolveClaudeBin(p); got != p {
				t.Errorf("resolveClaudeBin(%q) = %q, want it unchanged (already a path)", p, got)
			}
		}
	})
	t.Run("resolves a bare name on PATH", func(t *testing.T) {
		t.Setenv("LOOM_CLAUDE_BIN", "")
		// `go` is on PATH in the test env; it must resolve to a real path, not echo back.
		got := resolveClaudeBin("go")
		if got == "go" || !strings.Contains(got, "go") {
			t.Errorf("resolveClaudeBin(\"go\") = %q, want a resolved path containing 'go'", got)
		}
	})
	t.Run("falls back to input when absent", func(t *testing.T) {
		t.Setenv("LOOM_CLAUDE_BIN", "")
		const missing = "loom-no-such-binary-xyzzy"
		if got := resolveClaudeBin(missing); got != missing {
			t.Errorf("resolveClaudeBin(%q) = %q, want the input back so exec surfaces the normal error", missing, got)
		}
	})
}

// --consult injects the read-only directive at the system-prompt layer, and only
// when set — so a QUESTION drive can't sprawl into edits/commits, but a normal drive
// is unaffected.
func TestConsultDirective(t *testing.T) {
	on := claudeArgs(SessionOpts{Consult: true})
	if !slices.Contains(on, "--append-system-prompt") || !slices.Contains(on, consultDirective) {
		t.Errorf("Consult:true should append the read-only system prompt; args=%v", on)
	}
	off := claudeArgs(SessionOpts{})
	if slices.Contains(off, consultDirective) {
		t.Errorf("Consult:false must NOT inject the directive; args=%v", off)
	}
	// the directive must actually forbid the sprawl behaviors it exists to stop
	for _, must := range []string{"read-only", "commit", "journal"} {
		if !strings.Contains(strings.ToLower(consultDirective), must) {
			t.Errorf("consultDirective missing the %q guard", must)
		}
	}
}
