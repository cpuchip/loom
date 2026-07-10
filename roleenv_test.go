package loom

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shim's role environment: "<model>#<role>" resolves the role claude-home
// and, when present, the sibling <role>-workdir (mounted ro as /work — the
// seat's grounding context). A missing dir degrades, never fails.
func TestResolveModelHomeRoleWorkdir(t *testing.T) {
	root := t.TempDir()
	defHome := filepath.Join(root, "default-home")
	for _, d := range []string{
		defHome,
		filepath.Join(root, "wargame-claude-home"),
		filepath.Join(root, "wargame-workdir"),
		filepath.Join(root, "critic-claude-home"), // home but NO workdir
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldHome, oldRoot := openaiClaudeHome, openaiHomeRoot
	defer func() { openaiClaudeHome, openaiHomeRoot = oldHome, oldRoot }()
	openaiClaudeHome, openaiHomeRoot = defHome, root

	tests := []struct {
		model                   string
		wantModel, wantHomeTail string
		wantWorkdir             bool
	}{
		{"sonnet#wargame", "sonnet", "wargame-claude-home", true}, // home + workdir
		{"sonnet#critic", "sonnet", "critic-claude-home", false},  // home only
		{"sonnet#nosuchrole", "sonnet", "default-home", false},    // no home → default, no workdir
		{"sonnet", "sonnet", "default-home", false},               // bare model
	}
	for _, tc := range tests {
		gotModel, gotHome, gotWorkdir := resolveModelHome(tc.model)
		if gotModel != tc.wantModel {
			t.Errorf("%s: model = %q, want %q", tc.model, gotModel, tc.wantModel)
		}
		if filepath.Base(gotHome) != tc.wantHomeTail {
			t.Errorf("%s: home = %q, want tail %q", tc.model, gotHome, tc.wantHomeTail)
		}
		if (gotWorkdir != "") != tc.wantWorkdir {
			t.Errorf("%s: workdir = %q, want present=%v", tc.model, gotWorkdir, tc.wantWorkdir)
		}
	}
}

// A read-only workdir mounts as /work:ro; the default stays writable (the
// harness_run exfil channel depends on rw).
func TestDockerArgsWorkdirRO(t *testing.T) {
	rw := dockerArgs("C:/ws", false, "", "", "", []string{"-p", "hi"})
	ro := dockerArgs("C:/ws", true, "", "", "", []string{"-p", "hi"})
	if !slices.Contains(rw, "C:/ws:/work") || slices.Contains(rw, "C:/ws:/work:ro") {
		t.Errorf("rw mount wrong: %v", rw)
	}
	if !slices.Contains(ro, "C:/ws:/work:ro") {
		t.Errorf("ro mount missing: %v", ro)
	}
	// both still set the working dir
	for _, args := range [][]string{rw, ro} {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-w /work") {
			t.Errorf("missing -w /work: %v", args)
		}
	}
}
