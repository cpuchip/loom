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
	rw := dockerArgs("C:/ws", false, "", "", "", nil, []string{"-p", "hi"})
	ro := dockerArgs("C:/ws", true, "", "", "", nil, []string{"-p", "hi"})
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

// ExtraMounts add writable islands ALONGSIDE /work — the grounding shape loom-mcp
// commissions: /work read-only for the workspace, plus a build dir and a scratch dir
// the seat can actually write to. Each entry is passed through verbatim as a -v value.
func TestDockerArgsExtraMounts(t *testing.T) {
	islands := []string{
		"C:/commissions/abc/work:/commission",
		"C:/commissions/abc/scratch:/scratch",
	}
	got := dockerArgs("C:/ws", true, "", "", "", islands, []string{"-p", "hi"})
	joined := strings.Join(got, " ")
	// /work is still there and read-only (grounding is not sacrificed for the islands).
	if !slices.Contains(got, "C:/ws:/work:ro") {
		t.Errorf("workspace /work:ro mount missing with extra mounts: %v", got)
	}
	// each island appears as its own -v value.
	for _, m := range islands {
		if !slices.Contains(got, m) {
			t.Errorf("extra mount %q not present: %v", m, got)
		}
	}
	// a -v precedes each island (they are real bind mounts, not stray args).
	for _, m := range islands {
		if !strings.Contains(joined, "-v "+m) {
			t.Errorf("extra mount %q not introduced by -v: %v", m, joined)
		}
	}
	// empty entries are skipped, never emitted as a bare -v "".
	if n := dockerArgs("C:/ws", false, "", "", "", []string{"", "  "}, nil); slices.Contains(n, "") {
		t.Errorf("empty extra mount should be skipped, got %v", n)
	}
}
