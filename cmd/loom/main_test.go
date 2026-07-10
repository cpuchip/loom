package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageIncludesRace(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	usage()
	_ = w.Close()
	os.Stderr = old
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "loom race") {
		t.Fatalf("usage omitted race:\n%s", output)
	}
}

func TestCmdRaceRejectsMissingOracleAndConnect(t *testing.T) {
	if err := cmdRace([]string{"-contenders", "local", "prompt"}); err == nil || !strings.Contains(err.Error(), "-oracle is required") {
		t.Fatalf("missing oracle error = %v", err)
	}
	err := cmdRace([]string{"-contenders", "local", "-oracle", "exit 0", "-connect", "ws://example", "prompt"})
	if err == nil || !strings.Contains(err.Error(), "--connect is not supported") {
		t.Fatalf("connect error = %v", err)
	}
}

func TestResolveCloneTempDir(t *testing.T) {
	origin := makeOrigin(t)
	var stderr bytes.Buffer

	dir, err := resolveClone(origin, "", "", &stderr)
	if err != nil {
		t.Fatalf("resolveClone() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if !strings.HasPrefix(filepath.Base(dir), "loom-clone-") {
		t.Errorf("clone dir %q does not use the loom-clone- prefix", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Errorf("cloned file missing: %v", err)
	}
	if !strings.Contains(stderr.String(), dir) {
		t.Errorf("stderr %q does not identify clone dir %q", stderr.String(), dir)
	}
}

func TestResolveCloneEmptyDir(t *testing.T) {
	origin := makeOrigin(t)
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveClone(origin, dir, "", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveClone() error = %v", err)
	}
	if got != dir {
		t.Errorf("resolveClone() dir = %q, want %q", got, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Errorf("cloned file missing: %v", err)
	}
}

func TestResolveCloneRejectsUnsafeDestinations(t *testing.T) {
	origin := makeOrigin(t)
	nonEmpty := filepath.Join(t.TempDir(), "occupied")
	if err := os.Mkdir(nonEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		cloneURL string
		dir      string
		remote   string
		want     string
	}{
		{
			name:     "non-empty destination",
			cloneURL: origin,
			dir:      nonEmpty,
			want:     "exists and is not empty",
		},
		{
			name:     "clone failure includes git stderr",
			cloneURL: filepath.Join(t.TempDir(), "missing-origin"),
			dir:      filepath.Join(t.TempDir(), "checkout"),
			want:     "fatal:",
		},
		{
			name:     "remote backend",
			cloneURL: origin,
			remote:   "worker.example",
			want:     "--clone cannot be used with --remote",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveClone(tt.cloneURL, tt.dir, tt.remote, &bytes.Buffer{})
			if err == nil {
				t.Fatal("resolveClone() error = nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("resolveClone() error = %q, want substring %q", err, tt.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(nonEmpty, "keep.txt")); err != nil {
		t.Errorf("non-empty destination was changed: %v", err)
	}
}

func makeOrigin(t *testing.T) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin")
	runGitCommand(t, "", "init", origin)
	if err := os.WriteFile(filepath.Join(origin, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, origin, "add", "hello.txt")
	runGitCommand(t, origin, "-c", "user.name=Loom Test", "-c", "user.email=loom-test@example.invalid", "commit", "-m", "initial")
	return origin
}

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
