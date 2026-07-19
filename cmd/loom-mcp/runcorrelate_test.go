package main

// Proofs for the CLI-worker → transcript correlation, with the recycled-PID guard as the
// centerpiece: a live process whose PID happens to match an old/finished/time-inconsistent
// manifest must NEVER be shown someone else's transcript.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cpuchip/loom"
)

// writeManifestFixture drops a runs/<id>/manifest.json (+ optional output.log) for the
// correlator to read — the same on-disk shape a real `loom run` writes.
func writeManifestFixture(t *testing.T, root, id string, m loom.RunManifest, log string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.RunID = id
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if log != "" {
		if err := os.WriteFile(filepath.Join(dir, "output.log"), []byte(log), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCorrelateWorkerMatch(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	procStart := now.Add(-3 * time.Minute)

	// A genuine live worker: manifest recorded ~1s after the process started, no finish,
	// fresh heartbeat, with a couple of streamed log lines.
	log := "2026-07-19T01:57:01Z run 20260719-...-abc started — claude [run hi]\n" +
		"2026-07-19T01:57:05Z → tool: Bash\n" +
		"2026-07-19T01:57:07Z assistant: building the deliverable\n"
	writeManifestFixture(t, root, "20260719-015700-abc", loom.RunManifest{
		WrapperPID:  18652,
		StartedAt:   procStart.Add(1 * time.Second),
		HeartbeatAt: now.Add(-10 * time.Second),
		Backend:     "claude",
	}, log)

	c, ok := correlateWorker(cliWorker{PID: 18652, StartedAt: procStart}, root, now)
	if !ok {
		t.Fatal("a genuine live worker must correlate to its run record")
	}
	if c.RunID != "20260719-015700-abc" {
		t.Errorf("run_id = %q, want the matched id", c.RunID)
	}
	if c.Status != "running" {
		t.Errorf("status = %q, want running (fresh heartbeat, no finish)", c.Status)
	}
	// The tail is the last lines (≤ cliWorkerTailLines), timestamp-stripped — the
	// app-facing glance. All three lines fit, so all three come back clean.
	want := "run 20260719-...-abc started — claude [run hi]\n" +
		"→ tool: Bash\n" +
		"assistant: building the deliverable"
	if c.Tail != want {
		t.Errorf("tail = %q, want %q", c.Tail, want)
	}
}

func TestCorrelateWorkerHeartbeatStale(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	procStart := now.Add(-10 * time.Minute)

	// Open run (no finish) whose heartbeat froze 5 minutes ago = the wedged/dead signal.
	writeManifestFixture(t, root, "20260719-015000-wed", loom.RunManifest{
		WrapperPID:  777,
		StartedAt:   procStart.Add(500 * time.Millisecond),
		HeartbeatAt: now.Add(-5 * time.Minute),
	}, "2026-07-19T01:50:10Z assistant: last thing it said\n")

	c, ok := correlateWorker(cliWorker{PID: 777, StartedAt: procStart}, root, now)
	if !ok {
		t.Fatal("a wedged worker still correlates — the stale status IS the signal")
	}
	if c.Status != "heartbeat-stale" {
		t.Errorf("status = %q, want heartbeat-stale", c.Status)
	}
}

// TestCorrelateRejectsRecycledPIDByFinish: a manifest whose wrapper_pid matches a live
// process BUT is already finished is a recycled PID (that wrapper exited) — never shown.
func TestCorrelateRejectsRecycledPIDByFinish(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	procStart := now.Add(-30 * time.Second)
	fin := now.Add(-20 * time.Second)

	writeManifestFixture(t, root, "20260719-015900-fin", loom.RunManifest{
		WrapperPID:  4242,
		StartedAt:   procStart.Add(1 * time.Second),
		HeartbeatAt: fin,
		FinishedAt:  &fin, // the original run for PID 4242 already exited
	}, "2026-07-19T01:59:10Z assistant: someone else's transcript\n")

	if _, ok := correlateWorker(cliWorker{PID: 4242, StartedAt: now}, root, now); ok {
		t.Fatal("a FINISHED manifest must not be attached to a live process reusing its PID")
	}
}

// TestCorrelateRejectsRecycledPIDByTime: the old run for this PID started long before the
// live process now holding it — the time-consistency guard rejects the mismatch.
func TestCorrelateRejectsRecycledPIDByTime(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)

	// Old OPEN run: wrapper_pid 4242, started 3 hours ago (wrapper since died un-gracefully,
	// so no finished_at) — but the heartbeat is fresh enough to look "running" if we ignored
	// time. The live process now holding PID 4242 started just now.
	writeManifestFixture(t, root, "20260718-230000-old", loom.RunManifest{
		WrapperPID:  4242,
		StartedAt:   now.Add(-3 * time.Hour),
		HeartbeatAt: now.Add(-5 * time.Second),
	}, "2026-07-18T23:00:10Z assistant: stale run from hours ago\n")

	if _, ok := correlateWorker(cliWorker{PID: 4242, StartedAt: now}, root, now); ok {
		t.Fatal("a time-inconsistent manifest (started hours before the live process) must be rejected")
	}
}

// TestCorrelatePicksClosestOpenMatch: if two OPEN manifests carry the same wrapper_pid
// (an extreme edge), the one whose start is closest to the process wins.
func TestCorrelatePicksClosestOpenMatch(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	procStart := now.Add(-1 * time.Minute)

	writeManifestFixture(t, root, "20260719-015830-near", loom.RunManifest{
		WrapperPID: 909, StartedAt: procStart.Add(2 * time.Second), HeartbeatAt: now,
	}, "near\n")
	writeManifestFixture(t, root, "20260719-015855-far", loom.RunManifest{
		WrapperPID: 909, StartedAt: procStart.Add(50 * time.Second), HeartbeatAt: now,
	}, "far\n")

	c, ok := correlateWorker(cliWorker{PID: 909, StartedAt: procStart}, root, now)
	if !ok {
		t.Fatal("want a match")
	}
	if c.RunID != "20260719-015830-near" {
		t.Errorf("run_id = %q, want the closest-started match 'near'", c.RunID)
	}
}

// TestCorrelateNoRunsDir: a missing runs dir (nothing has ever run, or LOOM_HOME differs)
// yields no correlation, gracefully — the worker just shows without a transcript.
func TestCorrelateNoRunsDir(t *testing.T) {
	if _, ok := correlateWorker(cliWorker{PID: 1}, filepath.Join(t.TempDir(), "nope"), time.Now()); ok {
		t.Fatal("a missing runs dir must yield ok=false, not a match")
	}
}

// TestCorrelateNoMatchingPID: run records exist, but none for this worker's PID.
func TestCorrelateNoMatchingPID(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeManifestFixture(t, root, "20260719-010000-xxx", loom.RunManifest{
		WrapperPID: 111, StartedAt: now, HeartbeatAt: now,
	}, "hi\n")
	if _, ok := correlateWorker(cliWorker{PID: 999, StartedAt: now}, root, now); ok {
		t.Fatal("a worker with no matching manifest PID must not correlate")
	}
}

// TestCorrelateZeroProcStartSkipsTimeGuard: when the platform reports no process start
// time, the time guard is skipped and the finished_at guard alone stands (a match is
// allowed on an open manifest).
func TestCorrelateZeroProcStartSkipsTimeGuard(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeManifestFixture(t, root, "20260719-010000-noproc", loom.RunManifest{
		WrapperPID: 222, StartedAt: now.Add(-2 * time.Hour), HeartbeatAt: now.Add(-3 * time.Second),
	}, "still running\n")
	c, ok := correlateWorker(cliWorker{PID: 222, StartedAt: time.Time{}}, root, now)
	if !ok {
		t.Fatal("with an unknown process start time, an open PID match should still correlate")
	}
	if c.Status != "running" {
		t.Errorf("status = %q, want running", c.Status)
	}
}

func TestReadLogTailBasics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")

	// missing file → empty
	if got := readLogTail(path, 5); got != "" {
		t.Errorf("missing file tail = %q, want empty", got)
	}

	// last-N with timestamp stripping + CRLF tolerance
	content := "2026-07-19T01:00:00Z line one\r\n" +
		"2026-07-19T01:00:01Z line two\n" +
		"2026-07-19T01:00:02Z line three\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := readLogTail(path, 2), "line two\nline three"; got != want {
		t.Errorf("tail(2) = %q, want %q", got, want)
	}
	if got, want := readLogTail(path, 10), "line one\nline two\nline three"; got != want {
		t.Errorf("tail(10) = %q, want %q", got, want)
	}
}

// TestReadLogTailBoundedWindow proves a large log is read only from its tail (bounded
// window) and still yields clean last-N lines with no partial first line.
func TestReadLogTailBoundedWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")

	var sb []byte
	for i := 0; i < 5000; i++ { // ~ many KB, well over the 64KB window
		sb = append(sb, []byte("2026-07-19T01:00:00Z filler line to grow the log past the window\n")...)
	}
	sb = append(sb, []byte("2026-07-19T02:00:00Z the final event line\n")...)
	if err := os.WriteFile(path, sb, 0o644); err != nil {
		t.Fatal(err)
	}
	got := readLogTail(path, 3)
	if !hasSuffixLine(got, "the final event line") {
		t.Errorf("bounded tail should end with the final line; got %q", got)
	}
	// No partial/garbled first line: every returned line is timestamp-stripped cleanly.
	for _, ln := range splitLines(got) {
		if len(ln) == 0 {
			continue
		}
		if ln[0] == '2' && len(ln) > 10 && ln[4] == '-' {
			t.Errorf("line still carries a raw timestamp (partial-line leak?): %q", ln)
		}
	}
}

func TestStripLogTimestamp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-07-19T01:00:02Z → tool: Bash", "→ tool: Bash"},
		{"assistant: no timestamp here", "assistant: no timestamp here"},
		{"", ""},
		{"notatimestamp still passes", "notatimestamp still passes"},
	}
	for _, c := range cases {
		if got := stripLogTimestamp(c.in); got != c.want {
			t.Errorf("stripLogTimestamp(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func hasSuffixLine(s, want string) bool {
	lines := splitLines(s)
	return len(lines) > 0 && lines[len(lines)-1] == want
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}
