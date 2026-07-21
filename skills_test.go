package loom

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkill creates <root>/<name>/SKILL.md with the given body.
func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestSkillDirsFor — each backend maps to exactly the skill dir(s) it reads.
func TestSkillDirsFor(t *testing.T) {
	claudeDir := filepath.Join(".claude", "skills")
	agentsDir := filepath.Join(".agents", "skills")
	cases := map[string][]string{
		"claude":   {claudeDir},
		"codex":    {agentsDir},
		"copilot":  {claudeDir}, // reads both; one copy suffices
		"opencode": {claudeDir}, // reads both; one copy suffices
		"agy":      nil,
		"local":    nil,
		"unknown":  nil,
	}
	for backend, want := range cases {
		got := skillDirsFor(backend)
		if len(got) != len(want) {
			t.Errorf("skillDirsFor(%q) = %v, want %v", backend, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("skillDirsFor(%q)[%d] = %q, want %q", backend, i, got[i], want[i])
			}
		}
	}
}

// TestMirrorSkills_PerBackendTarget — loom writes ONLY the dir the target backend
// reads: claude→.claude/skills (not .agents), codex→.agents/skills (not .claude),
// copilot/opencode→.claude/skills (the single shared dir).
func TestMirrorSkills_PerBackendTarget(t *testing.T) {
	claudeRel := filepath.Join(".claude", "skills", "greet", "SKILL.md")
	agentsRel := filepath.Join(".agents", "skills", "greet", "SKILL.md")

	newSrc := func() string {
		s := t.TempDir()
		writeSkill(t, s, "greet", "---\nname: greet\ndescription: d\n---\nx")
		return s
	}

	// claude → .claude only
	work := t.TempDir()
	if err := mirrorSkills(SessionOpts{SkillsDir: newSrc(), Workdir: work}, "claude"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(work, claudeRel)) || exists(filepath.Join(work, agentsRel)) {
		t.Error("claude should write .claude/skills ONLY")
	}

	// codex → .agents only
	work = t.TempDir()
	if err := mirrorSkills(SessionOpts{SkillsDir: newSrc(), Workdir: work}, "codex"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(work, agentsRel)) || exists(filepath.Join(work, claudeRel)) {
		t.Error("codex should write .agents/skills ONLY")
	}

	// copilot / opencode → .claude only (one copy for a both-reader)
	for _, backend := range []string{"copilot", "opencode"} {
		work = t.TempDir()
		if err := mirrorSkills(SessionOpts{SkillsDir: newSrc(), Workdir: work}, backend); err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(work, claudeRel)) || exists(filepath.Join(work, agentsRel)) {
			t.Errorf("%s should write .claude/skills ONLY (one copy)", backend)
		}
	}
}

// TestMirrorSkills_CopiesTreeAndSupportingFiles — a skill's supporting files travel.
func TestMirrorSkills_CopiesTreeAndSupportingFiles(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeSkill(t, src, "greet", "---\nname: greet\ndescription: d\n---\nhello")
	if err := os.WriteFile(filepath.Join(src, "greet", "ref.txt"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}, "claude"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(work, ".claude", "skills", "greet")
	if got := read(t, filepath.Join(base, "SKILL.md")); got != "---\nname: greet\ndescription: d\n---\nhello" {
		t.Errorf("SKILL.md content wrong: %q", got)
	}
	if !exists(filepath.Join(base, "ref.txt")) {
		t.Error("supporting file ref.txt did not travel")
	}
}

// TestMirrorSkills_MultipleAndIgnoresNonSkills — every subdir with a SKILL.md is a
// skill; a plain directory without one is ignored.
func TestMirrorSkills_MultipleAndIgnoresNonSkills(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeSkill(t, src, "alpha", "---\nname: alpha\ndescription: a\n---\nA")
	writeSkill(t, src, "beta", "---\nname: beta\ndescription: b\n---\nB")
	if err := os.MkdirAll(filepath.Join(src, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "not-a-skill", "readme.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}, "claude"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(work, ".claude", "skills")
	if !exists(filepath.Join(base, "alpha", "SKILL.md")) || !exists(filepath.Join(base, "beta", "SKILL.md")) {
		t.Error("alpha/beta not both mirrored")
	}
	if exists(filepath.Join(base, "not-a-skill")) {
		t.Error("non-skill dir should not be mirrored")
	}
}

// TestMirrorSkills_SingleSkillSource — pointing --skills at a single skill folder
// (SKILL.md directly inside) uses the folder's basename as the skill name.
func TestMirrorSkills_SingleSkillSource(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "solo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: solo\ndescription: s\n---\nS"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}, "claude"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(work, ".claude", "skills", "solo", "SKILL.md")) {
		t.Error("single-skill source not mirrored under its basename")
	}
}

// TestMirrorSkills_ReplacesSameNameKeepsOthers — a same-named target skill is
// replaced by the authored source (no stale files); an unrelated existing skill is
// left untouched.
func TestMirrorSkills_ReplacesSameNameKeepsOthers(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeSkill(t, src, "greet", "---\nname: greet\ndescription: new\n---\nNEW")

	oldGreet := filepath.Join(work, ".claude", "skills", "greet")
	if err := os.MkdirAll(oldGreet, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(oldGreet, "SKILL.md"), []byte("OLD"), 0o644)
	os.WriteFile(filepath.Join(oldGreet, "stale.txt"), []byte("stale"), 0o644)
	writeSkill(t, filepath.Join(work, ".claude", "skills"), "mine", "---\nname: mine\ndescription: m\n---\nMINE")

	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}, "claude"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(oldGreet, "SKILL.md")); got != "---\nname: greet\ndescription: new\n---\nNEW" {
		t.Errorf("greet not replaced: %q", got)
	}
	if exists(filepath.Join(oldGreet, "stale.txt")) {
		t.Error("stale file survived replacement")
	}
	if !exists(filepath.Join(work, ".claude", "skills", "mine", "SKILL.md")) {
		t.Error("unrelated existing skill was clobbered")
	}
}

// TestMirrorSkills_NoOps — remote sessions, an empty SkillsDir, and a backend that
// reads no skills (agy/local) all do nothing.
func TestMirrorSkills_NoOps(t *testing.T) {
	src := t.TempDir()
	writeSkill(t, src, "greet", "---\nname: greet\ndescription: d\n---\nx")

	// Remote → no-op.
	work := t.TempDir()
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work, Remote: "user@host"}, "claude"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(work, ".claude", "skills", "greet")) {
		t.Error("remote session must not mirror skills locally")
	}

	// A no-skills backend → no-op even with a source.
	work2 := t.TempDir()
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work2}, "agy"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(work2, ".claude")) || exists(filepath.Join(work2, ".agents")) {
		t.Error("a no-skills backend should create nothing")
	}

	// Empty SkillsDir → no-op.
	work3 := t.TempDir()
	if err := mirrorSkills(SessionOpts{Workdir: work3}, "claude"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(work3, ".claude")) {
		t.Error("empty SkillsDir should create nothing")
	}
}
