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

// TestMirrorSkills_BothTargets — a skill lands in BOTH .claude/skills and
// .agents/skills of the workdir, with supporting files intact.
func TestMirrorSkills_BothTargets(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeSkill(t, src, "greet", "---\nname: greet\ndescription: d\n---\nhello")
	// a supporting file should travel with the skill
	if err := os.WriteFile(filepath.Join(src, "greet", "ref.txt"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}); err != nil {
		t.Fatalf("mirrorSkills: %v", err)
	}
	for _, rel := range []string{".claude/skills", ".agents/skills"} {
		skillMd := filepath.Join(work, filepath.FromSlash(rel), "greet", "SKILL.md")
		if got := read(t, skillMd); got != "---\nname: greet\ndescription: d\n---\nhello" {
			t.Errorf("%s content wrong: %q", rel, got)
		}
		if !exists(filepath.Join(work, filepath.FromSlash(rel), "greet", "ref.txt")) {
			t.Errorf("%s: supporting file ref.txt did not travel", rel)
		}
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

	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".claude/skills", ".agents/skills"} {
		base := filepath.Join(work, filepath.FromSlash(rel))
		if !exists(filepath.Join(base, "alpha", "SKILL.md")) || !exists(filepath.Join(base, "beta", "SKILL.md")) {
			t.Errorf("%s: alpha/beta not both mirrored", rel)
		}
		if exists(filepath.Join(base, "not-a-skill")) {
			t.Errorf("%s: non-skill dir should not be mirrored", rel)
		}
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
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}); err != nil {
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

	// Pre-existing target: an old greet (with a stale file) + an unrelated skill.
	oldGreet := filepath.Join(work, ".claude", "skills", "greet")
	if err := os.MkdirAll(oldGreet, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(oldGreet, "SKILL.md"), []byte("OLD"), 0o644)
	os.WriteFile(filepath.Join(oldGreet, "stale.txt"), []byte("stale"), 0o644)
	writeSkill(t, filepath.Join(work, ".claude", "skills"), "mine", "---\nname: mine\ndescription: m\n---\nMINE")

	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work}); err != nil {
		t.Fatal(err)
	}
	// greet replaced with new content, stale file gone.
	if got := read(t, filepath.Join(oldGreet, "SKILL.md")); got != "---\nname: greet\ndescription: new\n---\nNEW" {
		t.Errorf("greet not replaced: %q", got)
	}
	if exists(filepath.Join(oldGreet, "stale.txt")) {
		t.Error("stale file survived replacement")
	}
	// the user's unrelated skill survives.
	if !exists(filepath.Join(work, ".claude", "skills", "mine", "SKILL.md")) {
		t.Error("unrelated existing skill was clobbered")
	}
}

// TestMirrorSkills_NoOps — remote sessions and an empty SkillsDir do nothing.
func TestMirrorSkills_NoOps(t *testing.T) {
	src := t.TempDir()
	writeSkill(t, src, "greet", "---\nname: greet\ndescription: d\n---\nx")

	// Remote → no-op (must not write locally).
	work := t.TempDir()
	if err := mirrorSkills(SessionOpts{SkillsDir: src, Workdir: work, Remote: "user@host"}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(work, ".claude", "skills", "greet")) {
		t.Error("remote session must not mirror skills locally")
	}

	// Empty SkillsDir → no-op.
	work2 := t.TempDir()
	if err := mirrorSkills(SessionOpts{Workdir: work2}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(work2, ".claude")) {
		t.Error("empty SkillsDir should create nothing")
	}
}
