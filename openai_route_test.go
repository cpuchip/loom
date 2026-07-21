package loom

// openai_route_test.go — the shim's model→backend routing (shimBackendFor +
// serveOpenAI's backend selection). The historical bug: the shim hardwired
// agent := "claude", so "gpt-5.6-terra#capcom" became `claude --model
// gpt-5.6-terra` — a broken turn on the wrong harness. These tests pin the
// routing table AND drive the REAL serveOpenAI handler over httptest with
// hermetic stubs (warmStub, openai_warm_test.go) to prove: the routed backend
// gets opened, claude keeps its historical opts byte-identically, non-claude
// seats never get SkipPermissions (host wall intact), and warm stays
// claude-only.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShimBackendFor(t *testing.T) {
	cases := []struct {
		model, agent, bare string
	}{
		// claude fallthrough — byte-identical legacy (model untouched, even weird ones)
		{"sonnet", "claude", "sonnet"},
		{"opus", "claude", "opus"},
		{"claude-sonnet-4-5", "claude", "claude-sonnet-4-5"},
		{"", "claude", ""},
		{"some-unknown-model", "claude", "some-unknown-model"},
		{"sonnet#companion", "claude", "sonnet#companion"}, // no-home-root legacy shape survives
		// codex by name shape
		{"gpt-5.6-terra", "codex", "gpt-5.6-terra"},
		{"gpt-5.4-mini", "codex", "gpt-5.4-mini"},
		{"codex-auto-review", "codex", "codex-auto-review"},
		{"terra", "codex", "terra"},
		{"sol", "codex", "sol"},
		{"luna", "codex", "luna"},
		{"GPT-5.5", "codex", "GPT-5.5"}, // case-insensitive match, original case kept
		// a #role that survived (no --openai-home-root) is stripped for non-claude
		{"gpt-5.6-terra#capcom", "codex", "gpt-5.6-terra"},
		// bare backend names → that backend's default model
		{"codex", "codex", ""},
		{"copilot", "copilot", ""},
		{"opencode", "opencode", ""},
		// copilot- prefix peels
		{"copilot-gpt-5", "copilot", "gpt-5"},
		{"copilot-claude-sonnet-4.5", "copilot", "claude-sonnet-4.5"},
		// explicit backend:model pin
		{"codex:gpt-5.6-terra", "codex", "gpt-5.6-terra"},
		{"opencode:zen/glm-5.2", "opencode", "zen/glm-5.2"},
		{"claude:sonnet", "claude", "sonnet"},
		{"copilot:gpt-5", "copilot", "gpt-5"},
		// an unrecognized pin prefix is NOT a pin — falls through to claude
		{"zen:glm-5.2", "claude", "zen:glm-5.2"},
		// agy/local are not shim-routable — a pin attempt falls through to claude
		{"agy:gemini-3.5", "claude", "agy:gemini-3.5"},
	}
	for _, c := range cases {
		agent, bare := shimBackendFor(c.model)
		if agent != c.agent || bare != c.bare {
			t.Errorf("shimBackendFor(%q) = (%q, %q), want (%q, %q)", c.model, agent, bare, c.agent, c.bare)
		}
	}
}

func TestHostMCPConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// container-anchored + file present in the role home → mapped to the host path
	if got := hostMCPConfig("/home/node/.claude/mcp.json", home); got != filepath.Join(home, "mcp.json") {
		t.Errorf("anchored+present = %q", got)
	}
	// container-anchored + missing file → toolless, not a crashed turn
	if got := hostMCPConfig("/home/node/.claude/nope.json", home); got != "" {
		t.Errorf("anchored+missing = %q, want \"\"", got)
	}
	// container-anchored + no home → nothing to map to
	if got := hostMCPConfig("/home/node/.claude/mcp.json", ""); got != "" {
		t.Errorf("anchored+no-home = %q, want \"\"", got)
	}
	// a host path passes through untouched (loom can't second-guess it)
	if got := hostMCPConfig("C:/cfg/mcp.json", home); got != "C:/cfg/mcp.json" {
		t.Errorf("host path = %q", got)
	}
	if got := hostMCPConfig("", home); got != "" {
		t.Errorf("empty = %q", got)
	}
}

// TestServeOpenAIRoutesByModel drives the real handler with claude+codex+copilot
// stubs registered and proves each model name lands on its backend with the
// right per-backend session shape.
func TestServeOpenAIRoutesByModel(t *testing.T) {
	resetSticky()
	claudeStub, codexStub, copilotStub := &warmStub{}, &warmStub{}, &warmStub{}
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	srv := newServer(ts, map[string]Backend{
		"claude": claudeStub, "codex": codexStub, "copilot": copilotStub,
	}, time.Hour)

	// claude-family name → claude backend, historical opts byte-identical.
	if txt, code := postWarmTurn(t, srv, "sonnet", "", convo(msg("user", "hi"))); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("sonnet turn: code=%d txt=%q", code, txt)
	}
	if claudeStub.openCount() != 1 || codexStub.openCount() != 0 {
		t.Fatalf("sonnet must open claude only (claude=%d codex=%d)", claudeStub.openCount(), codexStub.openCount())
	}
	co := claudeStub.openedOpts()
	if co.Model != "sonnet" || !co.Isolate || !co.SkipPermissions {
		t.Fatalf("claude opts changed (regression!): %+v", co)
	}

	// gpt name → codex backend; the host wall stays (NO SkipPermissions).
	if txt, code := postWarmTurn(t, srv, "gpt-5.6-terra#capcom", "", convo(msg("user", "hi"))); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("terra turn: code=%d txt=%q", code, txt)
	}
	if codexStub.openCount() != 1 {
		t.Fatalf("gpt-5.6-terra#capcom must open codex (opens=%d)", codexStub.openCount())
	}
	xo := codexStub.openedOpts()
	if xo.Model != "gpt-5.6-terra" {
		t.Fatalf("codex model = %q, want gpt-5.6-terra (role stripped)", xo.Model)
	}
	if xo.SkipPermissions {
		t.Fatal("a host-run codex seat must NOT get SkipPermissions (it would strip the native sandbox)")
	}
	if !xo.Isolate {
		t.Fatal("codex seat should carry Isolate (the native workspace-write wall)")
	}
	if xo.ClaudeHome != "" {
		t.Fatalf("codex seat must not inherit a claude home (got %q)", xo.ClaudeHome)
	}

	// copilot- prefix → copilot backend, prefix peeled.
	if txt, code := postWarmTurn(t, srv, "copilot-gpt-5", "", convo(msg("user", "hi"))); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("copilot turn: code=%d txt=%q", code, txt)
	}
	if copilotStub.openCount() != 1 {
		t.Fatalf("copilot-gpt-5 must open copilot (opens=%d)", copilotStub.openCount())
	}
	if po := copilotStub.openedOpts(); po.Model != "gpt-5" || po.SkipPermissions {
		t.Fatalf("copilot opts = %+v, want model gpt-5 without SkipPermissions", po)
	}
}

// TestServeOpenAIMissingBackend: a model that routes to an unregistered backend
// errors clearly — it must NOT silently fall back to claude (that was the bug).
func TestServeOpenAIMissingBackend(t *testing.T) {
	resetSticky()
	claudeStub := &warmStub{}
	srv := newWarmTestServer(claudeStub, time.Hour)

	body, code := postWarmTurn(t, srv, "gpt-5.6-terra", "", convo(msg("user", "hi")))
	if code == 200 {
		t.Fatalf("expected an error status, got 200 (body=%q)", body)
	}
	if !strings.Contains(body, "codex") {
		t.Fatalf("error should name the missing backend: %q", body)
	}
	if claudeStub.openCount() != 0 {
		t.Fatal("must not fall back to claude for a codex-routed model")
	}
}

// TestServeOpenAIWarmClaudeOnly: a sticky non-claude conversation still works
// (cold, Resume-carried) but NEVER holds a warm seat — warm exists to skip
// claude's spawn floor and stickyOverview reports warm seats as claude.
func TestServeOpenAIWarmClaudeOnly(t *testing.T) {
	resetSticky()
	defer func(p bool) { openaiWarm = p }(openaiWarm)
	openaiWarm = true

	codexStub := &warmStub{}
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	srv := newServer(ts, map[string]Backend{"claude": &warmStub{}, "codex": codexStub}, time.Hour)

	postWarmTurn(t, srv, "gpt-5.6-terra", "sticky:cap", convo(msg("user", "hi")))
	postWarmTurn(t, srv, "gpt-5.6-terra", "sticky:cap",
		convo(msg("user", "hi"), msg("assistant", "hey"), msg("user", "again")))
	if n := codexStub.openCount(); n != 2 {
		t.Fatalf("non-claude sticky must stay cold per-turn: opens=%d want 2", n)
	}
	if stickyWarmN.Load() != 0 {
		t.Fatalf("non-claude sticky must never warm (count=%d)", stickyWarmN.Load())
	}
	// The lineage still carries: turn 2 resumed turn 1's session id.
	if got := codexStub.lastResumeSeen(); got != "warm-1" {
		t.Fatalf("sticky resume = %q, want warm-1 (Resume carries the lineage cold)", got)
	}
}
