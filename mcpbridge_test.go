package loom

// mcpbridge_test.go — the claude-JSON → codex/opencode MCP-config translators.
// The codex golden shapes mirror what `codex mcp add` writes and `codex mcp get`
// reads back (verified live, codex-cli 0.144.6): stdio = command/args/env,
// streamable HTTP = url/http_headers. The opencode shape mirrors `opencode mcp
// add` output (verified live, opencode-ai 1.17.15): local = type/command[]/
// environment, remote = type/url/headers.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const bridgeFixture = `{
  "mcpServers": {
    "webster": {
      "type": "stdio",
      "command": "C:/tools/webster-mcp.exe",
      "args": ["-dict", "C:/data/webster.json.gz"],
      "env": {"WEBSTER_MODE": "1828"}
    },
    "stewards": {
      "type": "http",
      "url": "http://host.docker.internal:8091/mcp",
      "headers": {"X-Stewards-Session": "wi--abc123--critique"}
    }
  }
}`

func writeBridgeFixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexMCPArgs(t *testing.T) {
	args, err := codexMCPArgs(writeBridgeFixture(t, bridgeFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		// sorted server names (stewards < webster), each value a TOML literal
		"-c", `mcp_servers.stewards.url="http://host.docker.internal:8091/mcp"`,
		"-c", `mcp_servers.stewards.http_headers.X-Stewards-Session="wi--abc123--critique"`,
		"-c", `mcp_servers.webster.command="C:/tools/webster-mcp.exe"`,
		"-c", `mcp_servers.webster.args=["-dict","C:/data/webster.json.gz"]`,
		"-c", `mcp_servers.webster.env.WEBSTER_MODE="1828"`,
	}
	if !slices.Equal(args, want) {
		t.Errorf("codexMCPArgs:\n got %q\nwant %q", args, want)
	}

	t.Run("empty path is no-op", func(t *testing.T) {
		if a, err := codexMCPArgs(""); err != nil || a != nil {
			t.Errorf("empty path: args=%v err=%v", a, err)
		}
	})
	t.Run("missing file errors", func(t *testing.T) {
		if _, err := codexMCPArgs(filepath.Join(t.TempDir(), "nope.json")); err == nil {
			t.Error("missing file must error (loud at Open, not a silently toolless worker)")
		}
	})
	t.Run("bad json errors", func(t *testing.T) {
		if _, err := codexMCPArgs(writeBridgeFixture(t, "not json")); err == nil {
			t.Error("unparseable config must error")
		}
	})
	t.Run("non-bare server name errors", func(t *testing.T) {
		_, err := codexMCPArgs(writeBridgeFixture(t, `{"mcpServers":{"has space":{"command":"x"}}}`))
		if err == nil || !strings.Contains(err.Error(), "has space") {
			t.Errorf("non-bare key must error naming the server, got %v", err)
		}
	})
	t.Run("windows backslash paths survive as TOML", func(t *testing.T) {
		a, err := codexMCPArgs(writeBridgeFixture(t, `{"mcpServers":{"w":{"command":"C:\\tools\\w.exe"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		// json.Marshal escapes the backslash — valid TOML basic-string escaping too.
		if !slices.Contains(a, `mcp_servers.w.command="C:\\tools\\w.exe"`) {
			t.Errorf("backslash path mangled: %q", a)
		}
	})
}

func TestOpencodeMCPConfig(t *testing.T) {
	data, err := opencodeMCPConfig(writeBridgeFixture(t, bridgeFixture))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Schema string `json:"$schema"`
		MCP    map[string]struct {
			Type        string            `json:"type"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			URL         string            `json:"url"`
			Headers     map[string]string `json:"headers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("translated config is not JSON: %v\n%s", err, data)
	}
	if got.Schema != "https://opencode.ai/config.json" {
		t.Errorf("$schema = %q", got.Schema)
	}
	w, ok := got.MCP["webster"]
	if !ok || w.Type != "local" {
		t.Fatalf("webster entry: %+v", got.MCP)
	}
	if !slices.Equal(w.Command, []string{"C:/tools/webster-mcp.exe", "-dict", "C:/data/webster.json.gz"}) {
		t.Errorf("local command joins command+args: %q", w.Command)
	}
	if w.Environment["WEBSTER_MODE"] != "1828" {
		t.Errorf("environment: %+v", w.Environment)
	}
	s, ok := got.MCP["stewards"]
	if !ok || s.Type != "remote" || s.URL != "http://host.docker.internal:8091/mcp" {
		t.Fatalf("stewards entry: %+v", s)
	}
	if s.Headers["X-Stewards-Session"] != "wi--abc123--critique" {
		t.Errorf("headers: %+v", s.Headers)
	}
}

// TestOpencodeOpenWritesAndClosesTempConfig drives the real Open/Close pair:
// Open materializes the translated temp config, Close removes it.
func TestOpencodeOpenWritesAndClosesTempConfig(t *testing.T) {
	sess, err := OpencodeBackend{}.Open(context.Background(), SessionOpts{MCPConfig: writeBridgeFixture(t, bridgeFixture)})
	if err != nil {
		t.Fatal(err)
	}
	oc := sess.(*opencodeSession)
	p := oc.mcpConfigPath
	if p == "" {
		t.Fatal("Open must materialize a temp opencode config")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("temp config missing: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("Close must remove the temp config (stat err=%v)", err)
	}

	t.Run("bad config fails Open", func(t *testing.T) {
		if _, err := (OpencodeBackend{}).Open(context.Background(), SessionOpts{MCPConfig: writeBridgeFixture(t, "not json")}); err == nil {
			t.Error("a bad mcp-config must fail Open loudly")
		}
	})
	t.Run("remote session skips the bridge", func(t *testing.T) {
		sess, err := OpencodeBackend{}.Open(context.Background(), SessionOpts{MCPConfig: writeBridgeFixture(t, bridgeFixture), Remote: "user@box"})
		if err != nil {
			t.Fatal(err)
		}
		if sess.(*opencodeSession).mcpConfigPath != "" {
			t.Error("a remote session must not materialize a local temp config it can't use")
		}
	})
}

// TestCodexOpenTranslatesMCP drives the real codex Open: a good config yields
// the -c overrides, a bad one fails loudly, remote skips.
func TestCodexOpenTranslatesMCP(t *testing.T) {
	sess, err := CodexBackend{}.Open(context.Background(), SessionOpts{MCPConfig: writeBridgeFixture(t, bridgeFixture)})
	if err != nil {
		t.Fatal(err)
	}
	cs := sess.(*codexSession)
	if len(cs.mcpArgs) == 0 || !slices.Contains(cs.mcpArgs, `mcp_servers.webster.command="C:/tools/webster-mcp.exe"`) {
		t.Fatalf("Open must translate MCPConfig into -c overrides: %q", cs.mcpArgs)
	}
	if _, err := (CodexBackend{}).Open(context.Background(), SessionOpts{MCPConfig: filepath.Join(t.TempDir(), "nope.json")}); err == nil {
		t.Error("a bad mcp-config must fail Open loudly")
	}
	sess, err = CodexBackend{}.Open(context.Background(), SessionOpts{MCPConfig: writeBridgeFixture(t, bridgeFixture), Remote: "user@box"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.(*codexSession).mcpArgs) != 0 {
		t.Error("a remote codex session must not carry -c overrides (bash -lc would mangle the TOML)")
	}
}
