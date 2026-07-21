package loom

// openai_skills_test.go — serve-side skills: the shim delivers authored skills
// to HOST-RUN seats (codex/copilot/opencode) by handing Open the role's
// <root>/<role>-skills dir, so mirrorSkills places them in the seat's workdir.
// Claude seats deliberately get NO SkillsDir — their skills already arrive via
// the mounted role home (<role>-claude-home/skills/ IS ~/.claude/skills/).
// Hermetic: the real serveOpenAI handler over httptest; no model spend.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withRoleRoot builds a home-root with a capcom role (workdir + one authored
// skill, no claude home — the host-run seat shape) and points the shim at it.
func withRoleRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		filepath.Join(root, "capcom-workdir"),
		filepath.Join(root, "capcom-skills", "greeter"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "capcom-skills", "greeter", "SKILL.md"),
		[]byte("---\nname: greeter\n---\nAlways greet by name.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRoot, oldHome := openaiHomeRoot, openaiClaudeHome
	t.Cleanup(func() { openaiHomeRoot, openaiClaudeHome = oldRoot, oldHome })
	openaiHomeRoot, openaiClaudeHome = root, ""
	return root
}

// TestServeOpenAISkillsPlumbing: a codex-routed "#capcom" request opens with
// SkillsDir = the role's skills dir and Workdir = the role workdir (which now
// resolves WITHOUT a claude home); a claude-routed request opens with NO
// SkillsDir even when the role has one.
func TestServeOpenAISkillsPlumbing(t *testing.T) {
	resetSticky()
	root := withRoleRoot(t)
	claudeStub, codexStub := &warmStub{}, &warmStub{}
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	srv := newServer(ts, map[string]Backend{"claude": claudeStub, "codex": codexStub}, time.Hour)

	if txt, code := postWarmTurn(t, srv, "gpt-5.6-terra#capcom", "", convo(msg("user", "hi"))); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("codex turn: code=%d txt=%q", code, txt)
	}
	xo := codexStub.openedOpts()
	if xo.SkillsDir != filepath.Join(root, "capcom-skills") {
		t.Errorf("codex seat SkillsDir = %q, want the role skills dir", xo.SkillsDir)
	}
	if xo.Workdir != filepath.Join(root, "capcom-workdir") {
		t.Errorf("codex seat Workdir = %q, want the role workdir (no claude home required)", xo.Workdir)
	}

	if _, code := postWarmTurn(t, srv, "sonnet#capcom", "", convo(msg("user", "hi"))); code != 200 {
		t.Fatalf("claude turn: code=%d", code)
	}
	if co := claudeStub.openedOpts(); co.SkillsDir != "" {
		t.Errorf("claude seat must NOT get SkillsDir (home mount carries skills), got %q", co.SkillsDir)
	}
}

// TestServeOpenAISkillsNoWorkdirNoScribble: a role with skills but NO workdir
// must not receive a SkillsDir — mirrorSkills would land in the serve process's
// own cwd otherwise.
func TestServeOpenAISkillsNoWorkdirNoScribble(t *testing.T) {
	resetSticky()
	root := withRoleRoot(t)
	if err := os.Remove(filepath.Join(root, "capcom-workdir")); err != nil {
		t.Fatal(err)
	}
	codexStub := &warmStub{}
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	srv := newServer(ts, map[string]Backend{"codex": codexStub}, time.Hour)
	if _, code := postWarmTurn(t, srv, "gpt-5.6-terra#capcom", "", convo(msg("user", "hi"))); code != 200 {
		t.Fatalf("turn: code=%d", code)
	}
	if xo := codexStub.openedOpts(); xo.SkillsDir != "" || xo.Workdir != "" {
		t.Errorf("no role workdir → no SkillsDir and no Workdir, got %+v", xo)
	}
}

// TestServeOpenAISkillsRealMirror drives the REAL opencode backend's Open path
// (with a deliberately missing binary, so nothing spawns and nothing is spent):
// the request fails at Send, but by then mirrorSkills has already placed the
// authored skill inside the role workdir — the actual delivery contract.
func TestServeOpenAISkillsRealMirror(t *testing.T) {
	resetSticky()
	root := withRoleRoot(t)
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	srv := newServer(ts, map[string]Backend{
		"opencode": OpencodeBackend{Bin: filepath.Join(root, "no-such-opencode-binary")},
	}, time.Hour)

	// the turn errors (missing binary) — that's expected and cheap
	if _, code := postWarmTurn(t, srv, "opencode#capcom", "", convo(msg("user", "hi"))); code == 200 {
		t.Fatal("expected the missing-binary turn to fail")
	}
	mirrored := filepath.Join(root, "capcom-workdir", ".claude", "skills", "greeter", "SKILL.md")
	b, err := os.ReadFile(mirrored)
	if err != nil {
		t.Fatalf("skill not mirrored into the role workdir: %v", err)
	}
	if !strings.Contains(string(b), "greet by name") {
		t.Errorf("mirrored skill content wrong: %s", b)
	}
}
