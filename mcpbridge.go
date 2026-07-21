package loom

// mcpbridge.go — carry loom's one MCP-config surface (SessionOpts.MCPConfig, a
// claude-format JSON file: {"mcpServers":{…}}) into backends whose CLIs speak a
// DIFFERENT config dialect. claude takes the file natively (--mcp-config) and
// copilot takes the same JSON shape (--additional-mcp-config @file); codex and
// opencode need translation:
//
//   - codex: config.toml [mcp_servers.<name>] entries, delivered per-invocation
//     as `-c mcp_servers.<name>.<key>=<TOML value>` overrides — surgical, never
//     touching the user's ~/.codex/config.toml. Shape verified live against
//     codex-cli 0.144.6 (`codex mcp add` output + `codex mcp get`): stdio =
//     command/args/env; streamable HTTP = url/http_headers.
//   - opencode: an opencode.json {"mcp":{…}} document, delivered as a temp file
//     via the OPENCODE_CONFIG env var (honored for reads — verified live against
//     opencode-ai 1.17.15: entries from a pointed-at config load and connect).
//     Shape verified via `opencode mcp add`: local = type/command[]/environment;
//     remote = type/url/headers.
//
// Both translators parse the SAME source file, so one MCP config drives any
// backend; a server entry a dialect cannot express is skipped, never mangled.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
)

// mcpServerJSON is one server entry in the claude-format MCP config JSON — the
// superset of the stdio ({command,args,env}) and HTTP ({type,url,headers}) forms.
type mcpServerJSON struct {
	Type    string            `json:"type"` // "", "stdio", "http", "sse"
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// parseMCPServers reads a claude-format MCP config file ({"mcpServers":{…}}).
func parseMCPServers(path string) (map[string]mcpServerJSON, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]mcpServerJSON `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg.MCPServers, nil
}

// codexMCPArgs translates the MCP config into codex `-c` overrides:
//
//	-c mcp_servers.<name>.command="…"  -c mcp_servers.<name>.args=["…"]  -c mcp_servers.<name>.env.K="v"
//	-c mcp_servers.<name>.url="…"      -c mcp_servers.<name>.http_headers.H="v"
//
// The `-c` value side is parsed as TOML by codex, so strings ride as TOML basic
// strings (json.Marshal produces a valid one — same escape grammar). Key
// segments (server names, env/header keys) must be TOML BARE keys ([A-Za-z0-9_-])
// because the dotted-path parser owns the left side; a non-bare key is a hard
// error rather than a silently mangled server. An empty path translates to nil
// (no MCP), and a URL entry wins over a command entry (claude's own precedence).
func codexMCPArgs(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	servers, err := parseMCPServers(path)
	if err != nil {
		return nil, err
	}
	var args []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if !tomlBareKey(name) {
			return nil, fmt.Errorf("mcp server %q: name is not a TOML bare key ([A-Za-z0-9_-]) — codex -c overrides cannot address it", name)
		}
		srv := servers[name]
		pre := "mcp_servers." + name + "."
		switch {
		case srv.URL != "":
			args = append(args, "-c", pre+"url="+tomlString(srv.URL))
			for _, k := range slices.Sorted(maps.Keys(srv.Headers)) {
				if !tomlBareKey(k) {
					return nil, fmt.Errorf("mcp server %q: header %q is not a TOML bare key", name, k)
				}
				args = append(args, "-c", pre+"http_headers."+k+"="+tomlString(srv.Headers[k]))
			}
		case srv.Command != "":
			args = append(args, "-c", pre+"command="+tomlString(srv.Command))
			if len(srv.Args) > 0 {
				args = append(args, "-c", pre+"args="+tomlStringArray(srv.Args))
			}
			for _, k := range slices.Sorted(maps.Keys(srv.Env)) {
				if !tomlBareKey(k) {
					return nil, fmt.Errorf("mcp server %q: env key %q is not a TOML bare key", name, k)
				}
				args = append(args, "-c", pre+"env."+k+"="+tomlString(srv.Env[k]))
			}
		}
	}
	return args, nil
}

// opencodeMCPConfig translates the MCP config into an opencode config document:
//
//	{"$schema":…, "mcp": {"<name>": {"type":"local","command":[…],"environment":{…}}
//	                      "<name>": {"type":"remote","url":…,"headers":{…}}}}
//
// The caller writes it to a temp file and points OPENCODE_CONFIG at it. NOTE:
// whether opencode MERGES this with ~/.config/opencode/opencode.json(c) or loads
// it INSTEAD is unverified — only the read path is proven — so a seat driven this
// way should not rely on global-config extras beyond provider auth (which lives
// separately and survives).
func opencodeMCPConfig(path string) ([]byte, error) {
	servers, err := parseMCPServers(path)
	if err != nil {
		return nil, err
	}
	mcp := map[string]any{}
	for name, srv := range servers {
		switch {
		case srv.URL != "":
			m := map[string]any{"type": "remote", "url": srv.URL}
			if len(srv.Headers) > 0 {
				m["headers"] = srv.Headers
			}
			mcp[name] = m
		case srv.Command != "":
			m := map[string]any{"type": "local", "command": append([]string{srv.Command}, srv.Args...)}
			if len(srv.Env) > 0 {
				m["environment"] = srv.Env
			}
			mcp[name] = m
		}
	}
	return json.MarshalIndent(map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"mcp":     mcp,
	}, "", "  ")
}

// tomlBareKey reports whether s is a valid TOML bare key (A-Za-z0-9_- and nonempty).
func tomlBareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// tomlString renders s as a TOML basic string. A JSON string literal is also a
// valid TOML basic string (same quote, same escape grammar; Go's HTML \uXXXX
// escapes are valid TOML unicode escapes), so json.Marshal does the work.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// tomlStringArray renders ss as a TOML array of basic strings (JSON array
// syntax is valid TOML array syntax for string elements).
func tomlStringArray(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}
