package main

import (
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
	for _, line := range summarizeDay(rows, time.Now()) {
		fmt.Println(line)
	}
	return nil
}

// summarizeDay renders the per-backend spend totals for runs STARTED today
// (local time) — the fleet-level "what did today cost" view (parity #3). Each
// backend's line reports what its runs honestly recorded: real USD where the
// CLI reports cost, tokens where it reports only tokens, and an unreported
// count for runs whose manifest carries neither (old manifests, agy, crashes).
// Empty when no runs started today.
func summarizeDay(rows []runRow, now time.Time) []string {
	type agg struct {
		runs, tokens, unreported int
		usd                      float64
	}
	y, m, d := now.Date()
	perBackend := map[string]*agg{}
	var order []string
	for _, r := range rows {
		ry, rm, rd := r.man.StartedAt.Local().Date()
		if ry != y || rm != m || rd != d {
			continue
		}
		a := perBackend[r.man.Backend]
		if a == nil {
			a = &agg{}
			perBackend[r.man.Backend] = a
			order = append(order, r.man.Backend)
		}
		a.runs++
		switch u := r.man.Usage; {
		case u != nil && u.CostSource == loom.CostReal:
			a.usd += u.CostUSD
		case u != nil && u.TotalTokens() > 0:
			a.tokens += u.TotalTokens()
		case r.man.CostUSD > 0: // pre-Usage manifests recorded bare cost
			a.usd += r.man.CostUSD
		default:
			a.unreported++
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.Strings(order)
	out := []string{"", "today:"}
	for _, b := range order {
		a := perBackend[b]
		line := fmt.Sprintf("  %-12s  %d run(s)", b, a.runs)
		if a.usd > 0 {
			line += fmt.Sprintf("  $%.4f", a.usd)
		}
		if a.tokens > 0 {
			line += fmt.Sprintf("  %d tokens", a.tokens)
		}
		if a.unreported > 0 {
			line += fmt.Sprintf("  (%d unreported)", a.unreported)
		}
		out = append(out, line)
	}
	return out
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
		man, err := loom.ReadManifest(dir)
		if err != nil {
			continue
		}
		sent, _ := loom.ReadSentinel(dir)
		rows = append(rows, runRow{
			man:    man,
			status: loom.RunStatus(man, sent, now),
			age:    now.Sub(man.StartedAt),
			note:   dir,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].man.StartedAt.After(rows[j].man.StartedAt) })
	return rows, nil
}

// runStatus, manifest reads, and the stale-heartbeat threshold now live in loom's core
// (runmanifest.go) so the `loom runs` history view and loom-mcp's live-worker correlation
// derive status from one shared implementation. See loom.RunStatus / loom.ReadManifest /
// loom.ReadSentinel, used by gatherRuns above.

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
