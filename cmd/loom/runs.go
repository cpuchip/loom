package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cpuchip/loom"
)

// cmdRuns lists recent `loom run` lifecycle records (from their manifests) with a derived
// status, or tails one run's streamed log. This is what makes the durability package
// OPERABLE: a foreman (or a human) can see running / heartbeat-stale / done / failed at a
// glance, and read the output of a run whose wrapper died.
//
//	loom runs                 list recent runs, newest first
//	loom runs tail <run-id>   print that run's output.log
func cmdRuns(args []string) error {
	if len(args) >= 1 && args[0] == "tail" {
		if len(args) < 2 {
			return fmt.Errorf("runs tail: need a run-id (see `loom runs`)")
		}
		return tailRun(args[1])
	}
	rows, err := gatherRuns(loom.RunsDir(), time.Now())
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "no runs recorded yet (%s)\n", loom.RunsDir())
		return nil
	}
	fmt.Printf("%-24s  %-16s  %-12s  %-8s  %s\n", "RUN-ID", "STATUS", "BACKEND", "AGE", "DIR/NOTE")
	for _, r := range rows {
		fmt.Printf("%-24s  %-16s  %-12s  %-8s  %s\n",
			r.man.RunID, r.status, backendLabel(r.man), humanAge(r.age), r.note)
	}
	return nil
}

// runRow is one recorded run plus its derived status.
type runRow struct {
	man    runManifest
	status string
	age    time.Duration
	note   string
}

// gatherRuns reads every <run-id>/manifest.json under root and derives each status,
// newest-started first. A run dir without a readable manifest is skipped (not fatal).
func gatherRuns(root string, now time.Time) ([]runRow, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runs dir: %w", err)
	}
	var rows []runRow
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		man, err := readManifest(dir)
		if err != nil {
			continue
		}
		sent, _ := readSentinel(dir)
		rows = append(rows, runRow{
			man:    man,
			status: runStatus(man, sent, now),
			age:    now.Sub(man.StartedAt),
			note:   dir,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].man.StartedAt.After(rows[j].man.StartedAt) })
	return rows, nil
}

// runStatus derives the operable status of a run from its manifest + optional done
// sentinel. The ordering matters: a written sentinel or finished_at is terminal; only an
// OPEN run (neither) is judged by heartbeat freshness — a stale heartbeat with no
// finish is the machine-readable "the wrapper died" verdict.
func runStatus(m runManifest, sentinel *doneSentinel, now time.Time) string {
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
	if now.Sub(m.HeartbeatAt) > heartbeatStaleAfter {
		return "heartbeat-stale"
	}
	return "running"
}

func readManifest(dir string) (runManifest, error) {
	var m runManifest
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func readSentinel(dir string) (*doneSentinel, error) {
	b, err := os.ReadFile(filepath.Join(dir, "done"))
	if err != nil {
		return nil, err
	}
	var s doneSentinel
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func tailRun(id string) error {
	dir := filepath.Join(loom.RunsDir(), id)
	b, err := os.ReadFile(filepath.Join(dir, "output.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("runs tail: no run %q under %s", id, loom.RunsDir())
		}
		return err
	}
	_, _ = os.Stdout.Write(b)
	return nil
}

func backendLabel(m runManifest) string {
	if m.Model != "" {
		return m.Backend + "/" + m.Model
	}
	return m.Backend
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
