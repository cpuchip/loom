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
