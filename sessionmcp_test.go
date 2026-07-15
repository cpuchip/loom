package loom

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #333: a wi-- session derives a per-session MCP config with the
// X-Stewards-Session header injected into every server entry, existing
// headers (the bearer token) preserved, and the in-container path under the
// mounted home. Any config problem degrades to ("", "") — never an error.
func TestSessionMCPConfig(t *testing.T) {
	home := t.TempDir()
	base := map[string]any{
		"mcpServers": map[string]any{
			"pg-ai-stewards": map[string]any{
				"type": "http",
				"url":  "http://host.docker.internal:8093/mcp",
				"headers": map[string]any{
					"Authorization": "Bearer secret",
				},
			},
		},
	}
	raw, _ := json.Marshal(base)
	if err := os.WriteFile(filepath.Join(home, "stewards-mcp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	inC, host := sessionMCPConfig(home, "/home/node/.claude/stewards-mcp.json", "wi--848e9d2b--wargame")
	if inC == "" || host == "" {
		t.Fatal("expected derived config, got fallback")
	}
	defer os.Remove(host)
	if !strings.HasPrefix(inC, "/home/node/.claude/.session-mcp/") {
		t.Errorf("in-container path outside the mounted home: %s", inC)
	}
	got, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg["mcpServers"].(map[string]any)["pg-ai-stewards"].(map[string]any)
	headers := srv["headers"].(map[string]any)
	if headers["X-Stewards-Session"] != "wi--848e9d2b--wargame" {
		t.Errorf("session header missing/wrong: %v", headers)
	}
	if headers["Authorization"] != "Bearer secret" {
		t.Errorf("existing auth header clobbered: %v", headers)
	}

	// degradation: missing base config, empty home, empty session
	if a, b := sessionMCPConfig(home, "/home/node/.claude/nope.json", "wi--x"); a != "" || b != "" {
		t.Error("missing base config must fall back")
	}
	if a, b := sessionMCPConfig("", "/home/node/.claude/stewards-mcp.json", "wi--x"); a != "" || b != "" {
		t.Error("empty home must fall back")
	}
	if a, b := sessionMCPConfig(home, "/home/node/.claude/stewards-mcp.json", ""); a != "" || b != "" {
		t.Error("empty session must fall back")
	}
}

// effectiveMCPConfig gates a home-anchored --mcp-config on the config file
// actually being present in the mounted home. A bare model (no home) or a home
// missing the file must degrade to toolless ("") rather than hand claude a path
// it can't open — which exits the process and surfaces as "stream ended before a
// result event". A non-anchored path (image-baked) passes through untouched.
func TestEffectiveMCPConfig(t *testing.T) {
	home := t.TempDir()
	anchored := "/home/node/.claude/stewards-mcp.json"

	// home mounted but file NOT present yet → drop the hinge, don't crash.
	if got := effectiveMCPConfig(anchored, home); got != "" {
		t.Errorf("home without the config file: got %q, want \"\"", got)
	}
	// bare model (no home) → drop the hinge (the regression: bare sonnet crashed).
	if got := effectiveMCPConfig(anchored, ""); got != "" {
		t.Errorf("no home mount: got %q, want \"\"", got)
	}
	// file present in the mounted home → pass the anchored path through.
	if err := os.WriteFile(filepath.Join(home, "stewards-mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := effectiveMCPConfig(anchored, home); got != anchored {
		t.Errorf("home with the config file: got %q, want %q", got, anchored)
	}
	// non-home-anchored path (e.g. an image-baked binary config) → untouched,
	// even with no home, since loom can't verify a path it doesn't own.
	if got := effectiveMCPConfig("/opt/mcp/config.json", ""); got != "/opt/mcp/config.json" {
		t.Errorf("non-anchored path must pass through: got %q", got)
	}
	// empty config → empty (nothing to wire).
	if got := effectiveMCPConfig("", home); got != "" {
		t.Errorf("empty config: got %q, want \"\"", got)
	}
}
