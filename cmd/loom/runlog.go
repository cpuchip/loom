package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cpuchip/loom"
)

// Run lifecycle durability. Every `loom run` writes three artifacts under
// LOOM_HOME/runs/<run-id>/ so a dying wrapper loses nothing and a supervisor can autopsy
// without guesswork (the 2026-07-18 incident: buffered output lost, no lifecycle record,
// child orphaned, foreman never woken):
//
//   - output.log   — the worker's streamed events, written line-by-line AS THEY ARRIVE,
//                    independent of stdout. Wrapper death loses nothing already emitted.
//   - manifest.json — started_at, argv, cwd, wrapper/child pids, a heartbeat touched
//                     every ~30s, and on exit finished_at + exit status + usage. A
//                     manifest with a STALE heartbeat and NO finished_at IS the
//                     machine-readable "the wrapper died" verdict.
//   - done          — a completion sentinel written on every graceful exit path, so a
//                     supervisor polls the sentinel instead of trusting process return.
//
// Only graceful exits and panics run the deferred finish(); a hard TerminateProcess
// leaves the manifest without finished_at on purpose — that absence, plus the stale
// heartbeat, is the durable evidence the run died un-gracefully.

const heartbeatInterval = 30 * time.Second

// runManifest and doneSentinel are the run-lifecycle records. Their fields and JSON tags
// live in loom's core (runmanifest.go) as the SHARED on-disk contract with the readers
// (`loom runs` here, and loom-mcp's live-worker correlation) — these aliases keep the
// wrapper's local write code terse while there is exactly one definition to drift from.
type runManifest = loom.RunManifest
type doneSentinel = loom.DoneSentinel

type runRecorder struct {
	dir  string
	mu   sync.Mutex
	man  runManifest
	logF *os.File
	stop chan struct{}
	done chan struct{}
}

// newRunID mints a sortable, collision-resistant run id: UTC timestamp + 6 hex chars.
func newRunID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// newRunRecorder creates LOOM_HOME/runs/<run-id>/, opens output.log for append, writes
// the initial manifest, prints the run dir to stderr, and starts the heartbeat. A
// recorder failure is non-fatal to the run (returns the error for the caller to log and
// continue running without durability rather than aborting the worker).
func newRunRecorder(argv []string, cwd, backend, model string) (*runRecorder, error) {
	now := time.Now()
	id := newRunID(now)
	dir := filepath.Join(loom.RunsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	logF, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open output.log: %w", err)
	}
	r := &runRecorder{
		dir:  dir,
		logF: logF,
		man: runManifest{
			RunID:       id,
			StartedAt:   now,
			Argv:        argv,
			Cwd:         cwd,
			Backend:     backend,
			Model:       model,
			WrapperPID:  os.Getpid(),
			HeartbeatAt: now,
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := r.writeManifest(); err != nil {
		_ = logF.Close()
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	r.logLine(fmt.Sprintf("run %s started — %s %v", id, backend, argv))
	go r.heartbeatLoop()
	return r, nil
}

// startedLine is the "print the run dir at run start" hint (fix #4) so a foreman can note
// where to look — and tail — even before the run finishes.
func (r *runRecorder) startedLine() string {
	return fmt.Sprintf("[run %s — %s — tail: loom runs tail %s]", r.man.RunID, r.dir, r.man.RunID)
}

// logLine appends one timestamped line to output.log. os.File writes go straight to the
// OS (no app-level buffer), so each line is durable the moment it returns — a wrapper
// killed the next instant keeps everything already written.
func (r *runRecorder) logLine(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logF == nil {
		return
	}
	fmt.Fprintf(r.logF, "%s %s\n", time.Now().UTC().Format(time.RFC3339), s)
}

// logEvent records one streamed worker event. This is the durable capture of the worker's
// output — the thing the incident lost when buffered stdout died with the wrapper.
func (r *runRecorder) logEvent(ev loom.Event) {
	switch ev.Kind {
	case loom.EvToolCall:
		r.logLine("→ tool: " + ev.Tool)
	case loom.EvToolResult:
		r.logLine("← tool result: " + ev.Tool)
	case loom.EvThinking:
		r.logLine("· thinking: " + ev.Text)
	case loom.EvAssistant:
		r.logLine("assistant: " + ev.Text)
	case loom.EvResult:
		r.logLine("result: " + ev.Text)
	}
}

// addChildPID records a spawned agent child's PID (installed as loom's child-spawn hook).
// May be called from the turn goroutine; guarded and persisted immediately so the pid is
// on disk before any wrapper death — that is what lets a supervisor confirm the reap.
func (r *runRecorder) addChildPID(pid int) {
	r.mu.Lock()
	for _, p := range r.man.ChildPIDs {
		if p == pid {
			r.mu.Unlock()
			return
		}
	}
	r.man.ChildPIDs = append(r.man.ChildPIDs, pid)
	r.mu.Unlock()
	r.logLine(fmt.Sprintf("child spawned — pid %d", pid))
	_ = r.persistManifest()
}

// heartbeatLoop touches heartbeat_at every interval until finish() stops it. A frozen
// heartbeat on a run with no finished_at is the "wrapper wedged or died" signal.
func (r *runRecorder) heartbeatLoop() {
	defer close(r.done)
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.beat()
		}
	}
}

// beat advances heartbeat_at and persists it. Exposed for tests (no 30s wait needed).
func (r *runRecorder) beat() {
	r.mu.Lock()
	r.man.HeartbeatAt = time.Now()
	r.mu.Unlock()
	_ = r.persistManifest()
}

// finish records the terminal outcome: stops the heartbeat, stamps finished_at + exit
// status + usage into the manifest, and writes the done sentinel. Runs from a defer, so
// it covers graceful returns AND panics — but NOT a hard TerminateProcess, which leaves
// the manifest un-finished on purpose (the durable death evidence).
func (r *runRecorder) finish(runErr error, rep loom.Reply) {
	// stop the heartbeat and wait for the loop to exit so it can't race the final write
	select {
	case <-r.stop: // already stopped
	default:
		close(r.stop)
		<-r.done
	}
	now := time.Now()
	status := "ok"
	exitErr := ""
	if runErr != nil {
		status, exitErr = "failed", runErr.Error()
	} else if rep.Err != "" {
		status, exitErr = "failed", rep.Err
	}
	r.mu.Lock()
	r.man.FinishedAt = &now
	r.man.HeartbeatAt = now
	r.man.ExitError = exitErr
	r.man.CostUSD = rep.CostUSD
	r.man.Turns = rep.Turns
	r.man.SessionID = rep.SessionID
	id := r.man.RunID
	r.mu.Unlock()

	if rep.Text != "" {
		r.logLine("final reply: " + rep.Text)
	}
	r.logLine("run finished — status " + status)
	_ = r.persistManifest()

	sentinel := doneSentinel{RunID: id, Status: status, FinishedAt: now, ExitError: exitErr}
	if b, err := json.MarshalIndent(sentinel, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(r.dir, "done"), b, 0o644)
	}
	r.mu.Lock()
	if r.logF != nil {
		_ = r.logF.Close()
		r.logF = nil
	}
	r.mu.Unlock()
}

// writeManifest persists the manifest under the lock (initial write).
func (r *runRecorder) writeManifest() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return writeManifestFile(r.dir, r.man)
}

// persistManifest snapshots the manifest under the lock, then writes it outside the lock
// (a read-modify-write from callers that already hold no lock).
func (r *runRecorder) persistManifest() error {
	r.mu.Lock()
	m := r.man
	dir := r.dir
	r.mu.Unlock()
	return writeManifestFile(dir, m)
}

// writeManifestFile writes manifest.json atomically (temp + rename in the same dir) so a
// reader never sees a half-written record.
func writeManifestFile(dir string, m runManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}
