package loom

// skills.go — "author once, the harness sees it." A skill authored in one source
// directory is mirrored into the session workdir at the exact skill folder the
// backend BEING RUN reads — nothing redundant, because Open always knows its
// backend. The agentic backends split on which folder they read:
//
//	claude    → .claude/skills/                (only)
//	codex     → .agents/skills/                (only)
//	copilot   → .claude/skills/ AND .agents/skills/
//	opencode  → .claude/skills/ AND .agents/skills/  (+ .opencode/skills/)
//
// So loom writes .claude/skills/ for claude, .agents/skills/ for codex, and — for
// copilot/opencode, which read both — the single .claude/skills/ (one copy is
// enough; they'd dedupe a mirrored pair by name anyway). agy/local read no skills.
// This is a filesystem convention, not a protocol: loom just places the files
// where the harness already looks. Local only — a remote session's filesystem is
// owned by that box.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// skillDirsFor returns the workdir-relative skill directories the given backend
// actually reads. Writing exactly these (rather than every possible dir) keeps the
// workdir clean: claude and codex each read one; copilot and opencode read both,
// so the single shared .claude/skills/ suffices. agy/local read no skills → nil.
func skillDirsFor(backend string) []string {
	switch backend {
	case "claude":
		return []string{filepath.Join(".claude", "skills")}
	case "codex":
		return []string{filepath.Join(".agents", "skills")}
	case "copilot", "opencode":
		// Both read .claude/skills/ (and .agents/skills/); one copy is enough.
		return []string{filepath.Join(".claude", "skills")}
	default:
		return nil
	}
}

// mirrorSkills copies every skill under opts.SkillsDir into the skill dir(s) the
// given backend reads, within the session workdir. No-op when SkillsDir is unset,
// the session is remote, or the backend reads no skills. The target is
// opts.Workdir, or the current working directory when Workdir is empty (matching
// how a backend with an empty Workdir resolves its cwd). A same-named skill already
// present is REPLACED (the authored source is authoritative); other skills are left
// untouched.
func mirrorSkills(opts SessionOpts, backend string) error {
	if opts.SkillsDir == "" {
		return nil
	}
	if opts.Remote != "" {
		// The remote box owns its filesystem; skills there must be provisioned on
		// that box. Documented, not silent (callers may log this).
		return nil
	}
	dirs := skillDirsFor(backend)
	if len(dirs) == 0 {
		return nil // this backend reads no skills
	}
	target := opts.Workdir
	if target == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("mirror skills: resolve cwd: %w", err)
		}
		target = wd
	}
	skills, err := enumerateSkills(opts.SkillsDir)
	if err != nil {
		return fmt.Errorf("mirror skills from %q: %w", opts.SkillsDir, err)
	}
	for _, sk := range skills {
		for _, rel := range dirs {
			dest := filepath.Join(target, rel, sk.name)
			if err := os.RemoveAll(dest); err != nil {
				return fmt.Errorf("mirror skill %q: clear %q: %w", sk.name, dest, err)
			}
			if err := copyTree(sk.dir, dest); err != nil {
				return fmt.Errorf("mirror skill %q -> %q: %w", sk.name, dest, err)
			}
		}
	}
	return nil
}

type skillSrc struct {
	name string
	dir  string
}

// enumerateSkills finds the skills under src. A skill is a directory containing a
// SKILL.md. If src ITSELF holds a SKILL.md it is a single skill (named after src's
// basename); otherwise every immediate subdirectory holding a SKILL.md is a skill.
func enumerateSkills(src string) ([]skillSrc, error) {
	if hasSkillFile(src) {
		return []skillSrc{{name: filepath.Base(src), dir: src}}, nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	var out []skillSrc
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(src, e.Name())
		if hasSkillFile(d) {
			out = append(out, skillSrc{name: e.Name(), dir: d})
		}
	}
	return out, nil
}

func hasSkillFile(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !fi.IsDir()
}

// copyTree recursively copies the file tree at src into dst (creating dst and any
// subdirectories), so a skill's supporting files travel with its SKILL.md.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
