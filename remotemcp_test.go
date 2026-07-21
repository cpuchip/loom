package loom

// remotemcp_test.go — remote MCP carriage (remotemcp.go + each backend's remote
// transport). These are ARGV-FIXTURE tests: they pin the exact command loom
// hands ssh, verifiable without a remote box. The live-remote status of each
// backend is recorded in the README (claude: transport proven on a real remote;
// codex/opencode/copilot: UNVERIFIED on a real remote — no codex on the
// practice box).

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mcpFixture = `{"mcpServers":{"pg":{"command":"stewards-mcp","args":["--flag","two words"],"env":{"DSN":"postgres://x"}}}}`

// unshellQuote inverts shellQuote — tests assert on the INNER script a remote
// bash -lc actually executes, not on its escaped transport form.
func unshellQuote(s string) string {
	s = strings.TrimPrefix(s, "'")
	s = strings.TrimSuffix(s, "'")
	return strings.ReplaceAll(s, `'\''`, "'")
}

func writeMCPFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(mcpFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRemoteMaterialize(t *testing.T) {
	data := []byte(`{"a": "b c", "quote": "it's"}`)
	prelude, remotePath := remoteMaterialize(data)
	if !strings.HasPrefix(remotePath, "/tmp/loom-mcp-") || !strings.HasSuffix(remotePath, ".json") {
		t.Errorf("remote path = %q", remotePath)
	}
	// the config rides as base64 — NO shell metacharacters from the payload
	b64 := base64.StdEncoding.EncodeToString(data)
	if !strings.Contains(prelude, b64) {
		t.Errorf("prelude must carry the base64 payload: %s", prelude)
	}
	if strings.Contains(prelude, `it's`) {
		t.Error("raw payload must never appear in the script (that is the mangling this exists to avoid)")
	}
	if !strings.Contains(prelude, "base64 -d > "+remotePath) {
		t.Errorf("prelude must decode into the remote path: %s", prelude)
	}
	if !strings.Contains(prelude, "trap 'rm -f "+remotePath+"' EXIT") {
		t.Errorf("prelude must clean up on exit: %s", prelude)
	}
	if !strings.HasSuffix(prelude, "&& ") {
		t.Errorf("prelude must chain into the command: %q", prelude)
	}
	// two materializations never collide
	_, p2 := remoteMaterialize(data)
	if p2 == remotePath {
		t.Error("remote paths must be unique per call")
	}
}

func TestRemoteMCPFromFile(t *testing.T) {
	if _, _, ok := remoteMCPFromFile(""); ok {
		t.Error("empty path must not materialize")
	}
	// a path with NO local file keeps the legacy remote-path contract
	if _, _, ok := remoteMCPFromFile(filepath.Join(t.TempDir(), "nope.json")); ok {
		t.Error("missing local file must not materialize (remote-path passthrough)")
	}
	p := writeMCPFixture(t)
	prelude, remotePath, ok := remoteMCPFromFile(p)
	if !ok || prelude == "" || remotePath == "" {
		t.Fatalf("existing local file must materialize: ok=%v", ok)
	}
}

func TestReplaceFlagValue(t *testing.T) {
	in := []string{"-p", "--mcp-config", "/local/mcp.json", "--verbose"}
	out := replaceFlagValue(in, "--mcp-config", "/tmp/planted.json")
	if out[2] != "/tmp/planted.json" {
		t.Errorf("value not swapped: %v", out)
	}
	if in[2] != "/local/mcp.json" {
		t.Error("input slice must not be mutated")
	}
	same := replaceFlagValue(in, "--not-present", "x")
	if strings.Join(same, " ") != strings.Join(in, " ") {
		t.Errorf("argv without the flag must come back unchanged: %v", same)
	}
}

// TestClaudeCmdRemoteMCP pins the remote claude transport: a LOCAL config file
// is planted (base64 prelude) and --mcp-config repointed at the planted path;
// remote+isolate mounts the planted file into the container instead. A config
// path with no local file passes through untouched (legacy remote-path).
func TestClaudeCmdRemoteMCP(t *testing.T) {
	ctx := context.Background()
	p := writeMCPFixture(t)

	c := claudeCmd(ctx, "claude", SessionOpts{Remote: "cpuchip@box", Workdir: "/r", MCPConfig: p}, claudeArgs(SessionOpts{MCPConfig: p}))
	j := strings.Join(c.Args, " ")
	if !strings.Contains(j, "base64 -d > /tmp/loom-mcp-") || !strings.Contains(j, "trap") {
		t.Errorf("remote claude must plant the local config: %v", j)
	}
	if !strings.Contains(j, "--mcp-config /tmp/loom-mcp-") {
		t.Errorf("claude must be pointed at the planted path: %v", j)
	}
	if strings.Contains(j, "--mcp-config "+p) {
		t.Errorf("the local path must not leak to the remote argv: %v", j)
	}

	// remote + isolate: the planted file mounts read-only into the sandbox
	c = claudeCmd(ctx, "claude", SessionOpts{Remote: "cpuchip@box", Isolate: true, Workdir: "/r", MCPConfig: p}, claudeArgs(SessionOpts{MCPConfig: p}))
	j = strings.Join(c.Args, " ")
	if !strings.Contains(j, ":/loom-mcp.json:ro") || !strings.Contains(j, "--mcp-config /loom-mcp.json") {
		t.Errorf("remote+isolate must mount the planted file and point inside the container: %v", j)
	}

	// legacy: a config path with NO local file names a file on the REMOTE
	remoteOnly := "/etc/claude/mcp.json"
	c = claudeCmd(ctx, "claude", SessionOpts{Remote: "cpuchip@box", MCPConfig: remoteOnly}, claudeArgs(SessionOpts{MCPConfig: remoteOnly}))
	j = strings.Join(c.Args, " ")
	if strings.Contains(j, "base64") || !strings.Contains(j, "--mcp-config "+remoteOnly) {
		t.Errorf("remote-path config must pass through untouched: %v", j)
	}
}

// TestCodexRemoteQuoting pins the fix that makes remote codex MCP carriable at
// all: every argv element is shell-quoted, so `-c` TOML values (quotes,
// brackets, spaces) survive `bash -lc` intact — the exact values the old raw
// space-join mangled.
func TestCodexRemoteQuoting(t *testing.T) {
	p := writeMCPFixture(t)
	mcpArgs, err := codexMCPArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	opts := SessionOpts{Remote: "cpuchip@box", Workdir: "/my repo", Consult: true}
	c := codexCmd(context.Background(), "codex", opts, codexArgs(opts, "", mcpArgs))
	if c.Args[0] != "ssh" {
		t.Fatalf("remote transport: %v", c.Args)
	}
	script := unshellQuote(c.Args[len(c.Args)-1])
	// the TOML string value arrives as ONE quoted element, quotes intact
	if !strings.Contains(script, `'mcp_servers.pg.command="stewards-mcp"'`) {
		t.Errorf("command override not quoted intact: %s", script)
	}
	if !strings.Contains(script, `'mcp_servers.pg.args=["--flag","two words"]'`) {
		t.Errorf("args array not quoted intact: %s", script)
	}
	if !strings.Contains(script, `'mcp_servers.pg.env.DSN="postgres://x"'`) {
		t.Errorf("env override not quoted intact: %s", script)
	}
	// a workdir with a space survives
	if !strings.Contains(script, `cd '/my repo' &&`) {
		t.Errorf("workdir not quoted: %s", script)
	}
	// a RESUMED turn carries its sandbox as a -c TOML override — the exact value
	// the old raw join let bash strip the quotes from
	resumed := codexCmd(context.Background(), "codex", opts, codexArgs(opts, "thread-1", mcpArgs))
	rscript := unshellQuote(resumed.Args[len(resumed.Args)-1])
	if !strings.Contains(rscript, `'sandbox_mode="read-only"'`) {
		t.Errorf("resumed sandbox override not quoted intact: %s", rscript)
	}
}

// TestCodexOpenRemoteMCP: Open now translates MCP for remote sessions too (the
// local file is the source of truth).
func TestCodexOpenRemoteMCP(t *testing.T) {
	p := writeMCPFixture(t)
	sess, err := CodexBackend{}.Open(context.Background(), SessionOpts{Remote: "cpuchip@box", MCPConfig: p})
	if err != nil {
		t.Fatal(err)
	}
	cs := sess.(*codexSession)
	if len(cs.mcpArgs) == 0 {
		t.Error("remote codex session must carry the translated -c overrides")
	}
	// a bad config file is still loud at Open, remote or not
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := (CodexBackend{}).Open(context.Background(), SessionOpts{Remote: "cpuchip@box", MCPConfig: bad}); err == nil {
		t.Error("bad mcp-config must fail at Open for remote sessions too")
	}
}

// TestOpencodeOpenRemoteMCP: a remote session translates the doc at Open and
// keeps the bytes for per-turn planting — no local temp file.
func TestOpencodeOpenRemoteMCP(t *testing.T) {
	p := writeMCPFixture(t)
	sess, err := OpencodeBackend{}.Open(context.Background(), SessionOpts{Remote: "cpuchip@box", MCPConfig: p})
	if err != nil {
		t.Fatal(err)
	}
	oc := sess.(*opencodeSession)
	if len(oc.mcpRemoteDoc) == 0 {
		t.Error("remote opencode session must hold the translated config document")
	}
	if oc.mcpConfigPath != "" {
		t.Errorf("remote session must not write a local temp file, got %q", oc.mcpConfigPath)
	}
	if !strings.Contains(string(oc.mcpRemoteDoc), `"pg"`) {
		t.Errorf("translated doc lost the server: %s", oc.mcpRemoteDoc)
	}
}

// TestOnehotCmdRemotePrelude: the prelude rides inside the remote script,
// before the cd — and a local spawn ignores it entirely.
func TestOnehotCmdRemotePrelude(t *testing.T) {
	pre, remotePath := remoteMaterialize([]byte(`{"mcp":{}}`))
	prelude := pre + "export OPENCODE_CONFIG=" + remotePath + " && "
	c := onehotCmd(context.Background(), "opencode", SessionOpts{Remote: "cpuchip@box", Workdir: "/r"}, []string{"run", "hi"}, prelude)
	script := unshellQuote(c.Args[len(c.Args)-1])
	if !strings.Contains(script, "export OPENCODE_CONFIG="+remotePath) {
		t.Errorf("prelude missing from remote script: %s", script)
	}
	if idx := strings.Index(script, "export OPENCODE_CONFIG"); idx > strings.Index(script, "cd '/r'") {
		t.Errorf("prelude must run before cd: %s", script)
	}
	local := onehotCmd(context.Background(), "opencode", SessionOpts{Workdir: "/r"}, []string{"run", "hi"}, prelude)
	if strings.Contains(strings.Join(local.Args, " "), "OPENCODE_CONFIG") {
		t.Errorf("local spawn must ignore the remote prelude: %v", local.Args)
	}
}

// TestCopilotRemoteMCPArgs: the @path flag value is repointed at the planted
// file for a remote turn (exercised via the same replaceFlagValue path
// copilot.SendStream uses).
func TestCopilotRemoteMCPArgs(t *testing.T) {
	p := writeMCPFixture(t)
	args := copilotArgs(SessionOpts{MCPConfig: p}, "", "hi")
	pre, remotePath, ok := remoteMCPFromFile(p)
	if !ok {
		t.Fatal("fixture must materialize")
	}
	swapped := replaceFlagValue(args, "--additional-mcp-config", "@"+remotePath)
	j := strings.Join(swapped, " ")
	if !strings.Contains(j, "--additional-mcp-config @"+remotePath) {
		t.Errorf("flag not repointed: %v", swapped)
	}
	if strings.Contains(j, p) {
		t.Errorf("local path must not survive: %v", swapped)
	}
	if !strings.Contains(pre, "base64") {
		t.Errorf("prelude shape: %s", pre)
	}
}
