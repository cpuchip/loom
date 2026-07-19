package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cpuchip/loom"
)

func TestRunStatus(t *testing.T) {
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	fin := now.Add(-time.Minute)

	cases := []struct {
		name     string
		m        runManifest
		sentinel *doneSentinel
		want     string
	}{
		{"running-fresh-heartbeat", runManifest{HeartbeatAt: now.Add(-10 * time.Second)}, nil, "running"},
		// THE incident verdict: an open run whose heartbeat froze = the wrapper died.
		{"stale-heartbeat-no-finish-is-DEAD", runManifest{HeartbeatAt: now.Add(-5 * time.Minute)}, nil, "heartbeat-stale"},
		{"done-via-sentinel", runManifest{HeartbeatAt: now.Add(-5 * time.Minute)}, &doneSentinel{Status: "ok"}, "done"},
		{"failed-via-sentinel", runManifest{HeartbeatAt: now}, &doneSentinel{Status: "failed"}, "failed"},
		{"done-via-manifest-finished", runManifest{FinishedAt: &fin}, nil, "done"},
		{"failed-via-manifest-exiterr", runManifest{FinishedAt: &fin, ExitError: "boom"}, nil, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := loom.RunStatus(c.m, c.sentinel, now); got != c.want {
				t.Errorf("runStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// writeRun drops a fixture run dir (manifest + optional sentinel + log) for gatherRuns.
func writeRun(t *testing.T, root, id string, m runManifest, sentinel *doneSentinel, log string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.RunID = id
	if err := writeManifestFile(dir, m); err != nil {
		t.Fatal(err)
	}
	if sentinel != nil {
		b, _ := json.Marshal(sentinel)
		if err := os.WriteFile(filepath.Join(dir, "done"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if log != "" {
		if err := os.WriteFile(filepath.Join(dir, "output.log"), []byte(log), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatherRunsStatusesAndOrder(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)

	// oldest: a dead run (stale heartbeat, no finish)
	writeRun(t, root, "20260719-010000-aaa",
		runManifest{StartedAt: now.Add(-50 * time.Minute), Backend: "claude", HeartbeatAt: now.Add(-40 * time.Minute)},
		nil, "some streamed output\n")
	// middle: a fresh running run
	writeRun(t, root, "20260719-013000-bbb",
		runManifest{StartedAt: now.Add(-20 * time.Minute), Backend: "codex", HeartbeatAt: now.Add(-5 * time.Second)},
		nil, "")
	// newest: a done run
	fin := now.Add(-1 * time.Minute)
	writeRun(t, root, "20260719-015000-ccc",
		runManifest{StartedAt: now.Add(-2 * time.Minute), Backend: "claude", FinishedAt: &fin, HeartbeatAt: fin},
		&doneSentinel{Status: "ok"}, "")

	rows, err := gatherRuns(root, now)
	if err != nil {
		t.Fatalf("gatherRuns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// newest-started first
	if rows[0].man.RunID != "20260719-015000-ccc" || rows[2].man.RunID != "20260719-010000-aaa" {
		t.Errorf("rows not sorted newest-first: %s .. %s", rows[0].man.RunID, rows[2].man.RunID)
	}
	want := map[string]string{
		"20260719-015000-ccc": "done",
		"20260719-013000-bbb": "running",
		"20260719-010000-aaa": "heartbeat-stale",
	}
	for _, r := range rows {
		if got := r.status; got != want[r.man.RunID] {
			t.Errorf("run %s status = %q, want %q", r.man.RunID, got, want[r.man.RunID])
		}
	}
}

func TestGatherRunsMissingDir(t *testing.T) {
	rows, err := gatherRuns(filepath.Join(t.TempDir(), "does-not-exist"), time.Now())
	if err != nil {
		t.Fatalf("gatherRuns on missing dir should be non-fatal: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("want 0 rows, got %d", len(rows))
	}
}

func TestTailRunMissing(t *testing.T) {
	t.Setenv("LOOM_HOME", t.TempDir())
	if err := tailRun("nope-not-a-run"); err == nil {
		t.Fatal("tailRun should error for a missing run-id")
	}
}
