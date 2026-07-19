package loom

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Run-lifecycle contract (shared). A `loom run` wrapper writes three artifacts under
// RunsDir()/<run-id>/ — manifest.json (this crash-legible record), output.log (the
// worker's streamed events), and a `done` sentinel on graceful exit. The WRITER lives in
// the loom CLI (cmd/loom); the READERS are the CLI's own `loom runs` history AND loom-mcp,
// which correlates a live `loom run` process to its manifest to serve the phone a
// transcript tail. Both sides share THIS definition — the on-disk JSON contract lives in
// exactly one place so writer and reader can never drift (the recurring live↔repo-drift
// class of bug). It stays in loom's zero-dependency core: stdlib only, no new deps.

// HeartbeatStaleAfter is how long without a heartbeat marks a still-open run as
// "heartbeat-stale" (probably a dead or wedged wrapper). Three missed 30s beats.
const HeartbeatStaleAfter = 95 * time.Second

// RunManifest is the lifecycle record one `loom run` writes to
// RunsDir()/<run-id>/manifest.json. StartedAt/HeartbeatAt/FinishedAt drive the derived
// status; WrapperPID is what a supervisor correlates a live process against; the usage
// fields (CostUSD/Turns/SessionID) are stamped in at finish and so are absent while a run
// is still open.
type RunManifest struct {
	RunID       string     `json:"run_id"`
	StartedAt   time.Time  `json:"started_at"`
	Argv        []string   `json:"argv"`
	Cwd         string     `json:"cwd"`
	Backend     string     `json:"backend"`
	Model       string     `json:"model,omitempty"`
	WrapperPID  int        `json:"wrapper_pid"`
	ChildPIDs   []int      `json:"child_pids,omitempty"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	ExitError   string     `json:"exit_error,omitempty"`
	CostUSD     float64    `json:"cost_usd,omitempty"`
	Turns       int        `json:"turns,omitempty"`
	SessionID   string     `json:"session_id,omitempty"`
}

// DoneSentinel is the tiny JSON written to the `done` file on a graceful exit.
type DoneSentinel struct {
	RunID      string    `json:"run_id"`
	Status     string    `json:"status"` // "ok" | "failed"
	FinishedAt time.Time `json:"finished_at"`
	ExitError  string    `json:"exit_error,omitempty"`
}

// RunStatus derives the operable status of a run from its manifest + optional done
// sentinel. The ordering matters: a written sentinel or finished_at is terminal; only an
// OPEN run (neither) is judged by heartbeat freshness — a stale heartbeat with no finish
// is the machine-readable "the wrapper died" verdict.
func RunStatus(m RunManifest, sentinel *DoneSentinel, now time.Time) string {
	if sentinel != nil {
		if sentinel.Status == "failed" {
			return "failed"
		}
		return "done"
	}
	if m.FinishedAt != nil {
		if m.ExitError != "" {
			return "failed"
		}
		return "done"
	}
	if now.Sub(m.HeartbeatAt) > HeartbeatStaleAfter {
		return "heartbeat-stale"
	}
	return "running"
}

// ReadManifest reads and decodes <dir>/manifest.json.
func ReadManifest(dir string) (RunManifest, error) {
	var m RunManifest
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// ReadSentinel reads <dir>/done. A missing sentinel returns (nil, error) — the caller
// treats a read error as "no sentinel yet" (the run is still open, or died un-gracefully).
func ReadSentinel(dir string) (*DoneSentinel, error) {
	b, err := os.ReadFile(filepath.Join(dir, "done"))
	if err != nil {
		return nil, err
	}
	var s DoneSentinel
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
