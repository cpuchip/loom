package main

// CLI-worker → transcript correlation. An enumerated `loom run` worker is just a PID and a
// command line; its actual output lives in the run-lifecycle record the wrapper streams to
// LOOM_HOME/runs/<run-id>/ (manifest.json + output.log + a done sentinel — see loom's
// runmanifest.go, the shared contract). This file matches a live worker to its run dir by
// wrapper_pid, GUARDED against PID recycling, and lifts a transcript tail + status + usage
// off it — so the phone's Live view shows what the worker is actually saying instead of an
// honest-but-empty "no transcript" note.
//
// The recycled-PID guard is the safety spine: an OS reuses PIDs, so a bare wrapper_pid ==
// process.pid match is not enough. A confident correlation additionally requires (a) the
// run to still be OPEN — a manifest with finished_at whose pid matches a LIVE process is by
// definition a recycled pid, the original wrapper having already exited — and (b) the
// manifest's started_at to be time-consistent with the process's observed creation time
// (the wrapper records started_at a moment AFTER the OS creates the process, so a genuine
// match lands within a small forward window; an old dead run's manifest sits far outside
// it). Fail either and we show the worker with no transcript, exactly as before — a false
// "no transcript" degrades gracefully; a false transcript (someone else's) would not.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cpuchip/loom"
)

const (
	// runMatchForwardWindow bounds how long AFTER a process starts its run manifest may
	// have been recorded. The wrapper writes started_at (time.Now()) just after the OS
	// creates the process, so a real match is a few hundred ms to a couple seconds; the
	// generous 90s tolerates a slow cold start while staying far tighter than any realistic
	// PID-recycle gap (a recycled pid's old manifest started minutes/hours earlier).
	runMatchForwardWindow = 90 * time.Second
	// runMatchBackSlack tolerates minor skew where the manifest's started_at reads
	// marginally BEFORE the reported process creation time. They share one wall clock, but
	// Win32 CreationDate and Go's time.Now() round to different precisions.
	runMatchBackSlack = 3 * time.Second
	// cliWorkerTailLines is how many trailing output.log lines a cli-worker card carries —
	// a glance comparable to a commission's last-few-turns tail, not the whole log (that is
	// what `loom runs tail <id>` is for).
	cliWorkerTailLines = 12
	// logTailWindowBytes caps how much of a (possibly multi-MB) output.log we read to find
	// the last N lines, so correlation cost stays small and fixed per worker.
	logTailWindowBytes = 64 * 1024
)

// runCorrelation is the transcript evidence lifted off a live worker's run record.
type runCorrelation struct {
	RunID   string  // the correlated run-id
	Status  string  // loom.RunStatus of an OPEN run: "running" | "heartbeat-stale"
	Tail    string  // last N output.log lines, newline-joined (the transcript glance)
	CostUSD float64 // manifest usage — present only after finish, so ~always 0 while live
}

// correlateWorker finds the OPEN run manifest whose wrapper_pid matches the worker's PID,
// guarded against PID recycling, and returns its run-id, derived status, transcript tail,
// and usage. ok=false means no confident match (the worker predates the durability
// package, its run dir is gone, or the only pid match failed the recycled-pid guard); the
// caller then renders the worker without a transcript, as before.
func correlateWorker(w cliWorker, runsDir string, now time.Time) (runCorrelation, bool) {
	dir, man, ok := findRunDirForPID(runsDir, w.PID, w.StartedAt, now)
	if !ok {
		return runCorrelation{}, false
	}
	// A correlated worker is OPEN (findRunDirForPID rejected finished manifests), so status
	// resolves to "running" or "heartbeat-stale" purely on heartbeat freshness — the latter
	// is the "this worker may be wedged" signal the card surfaces.
	return runCorrelation{
		RunID:   man.RunID,
		Status:  loom.RunStatus(man, nil, now),
		Tail:    readLogTail(filepath.Join(dir, "output.log"), cliWorkerTailLines),
		CostUSD: man.CostUSD,
	}, true
}

// findRunDirForPID scans the runs dir for the OPEN manifest whose wrapper_pid == pid and
// whose started_at is time-consistent with procStart. Among any (defensively) multiple
// candidates it keeps the one started closest to the process — there should be at most one
// open, time-consistent match per live pid. A missing/unreadable runs dir yields ok=false
// (no correlation), never an error: the transcript is a best-effort enrichment.
func findRunDirForPID(runsDir string, pid int, procStart, now time.Time) (dir string, man loom.RunManifest, ok bool) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return "", loom.RunManifest{}, false
	}
	var best loom.RunManifest
	var bestDir string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(runsDir, e.Name())
		m, err := loom.ReadManifest(d)
		if err != nil {
			continue
		}
		if m.WrapperPID != pid {
			continue
		}
		if m.FinishedAt != nil {
			continue // a finished run's wrapper has exited; a live pid match here is recycled
		}
		if !runStartConsistent(m.StartedAt, procStart) {
			continue // time-inconsistent → recycled pid (or a different run that reused it)
		}
		if !ok || closerTo(procStart, m.StartedAt, best.StartedAt) {
			best, bestDir, ok = m, d, true
		}
	}
	return bestDir, best, ok
}

// runStartConsistent reports whether a manifest recorded at manStart plausibly belongs to a
// process observed to have started at procStart. The manifest is written just AFTER the OS
// creates the process, so manStart must sit within [procStart-backSlack, procStart+forwardWindow].
// When procStart is unknown (zero — the platform reported no creation time) the time check
// is skipped and the finished_at guard alone stands.
func runStartConsistent(manStart, procStart time.Time) bool {
	if procStart.IsZero() {
		return true
	}
	delta := manStart.Sub(procStart)
	return delta >= -runMatchBackSlack && delta <= runMatchForwardWindow
}

// closerTo reports whether candidate a is nearer to target than b is (used to pick the best
// of multiple pid matches). A zero target (unknown process start) treats all as equal, so
// the first match wins.
func closerTo(target, a, b time.Time) bool {
	if target.IsZero() {
		return false
	}
	return absDur(a.Sub(target)) < absDur(b.Sub(target))
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// readLogTail returns up to the last n lines of a log file, timestamp-stripped and
// newline-joined. It reads only the trailing logTailWindowBytes so a long run's large log
// costs a small fixed read. Returns "" for a missing/empty/unreadable file.
func readLogTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()
	start := int64(0)
	if size > logTailWindowBytes {
		start = size - logTailWindowBytes
	}
	buf := make([]byte, size-start)
	nRead, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return ""
	}
	s := strings.ReplaceAll(string(buf[:nRead]), "\r\n", "\n")
	// If we started mid-file, drop the partial first line so the glance begins cleanly.
	if start > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, ln := range lines {
		lines[i] = stripLogTimestamp(ln)
	}
	return strings.Join(lines, "\n")
}

// stripLogTimestamp drops the leading "<RFC3339> " that the run recorder prefixes to every
// output.log line, leaving just the event ("→ tool: Bash", "assistant: …") for a cleaner
// glance. A line that does not begin with a parseable timestamp passes through unchanged.
func stripLogTimestamp(line string) string {
	if i := strings.IndexByte(line, ' '); i > 0 {
		if _, err := time.Parse(time.RFC3339, line[:i]); err == nil {
			return line[i+1:]
		}
	}
	return line
}
