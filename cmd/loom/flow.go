package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cpuchip/loom"
)

// cmdFlow — `loom flow run <file.json>` / `loom flow resume <flow-id>`:
// deterministic multi-step orchestration with cached resume (flow.go; design
// doc docs/proposals/loom-flow.md). Trust/plumbing flags are FLOW-WIDE and
// apply to every step's session.
func cmdFlow(args []string) error {
	if len(args) < 1 || (args[0] != "run" && args[0] != "resume") {
		return fmt.Errorf("flow: usage: loom flow run <file.json> | loom flow resume <flow-id>")
	}
	sub := args[0]
	fs := flag.NewFlagSet("flow "+sub, flag.ExitOnError)
	isolate := fs.Bool("isolate", false, "run claude steps in a docker sandbox / codex steps workspace-write")
	skipPerms := fs.Bool("skip-permissions", false, "headless trust for every step (safe only in externally-walled environments)")
	mcpConfig := fs.String("mcp-config", "", "MCP config JSON handed to every step's session")
	skills := fs.String("skills", "", "authored-skills dir mirrored into every step's workdir (per-backend target)")
	budget := fs.Float64("budget", 0, "flow-wide spend ceiling: USD for cost-reporting backends, tokens otherwise; once exceeded, not-yet-started steps are refused")
	concurrency := fs.Int("concurrency", 0, "max steps in flight (overrides the flow file; default 3)")
	fresh := fs.Bool("fresh", false, "(run) archive an existing journal for this flow-id and start clean")
	jsonOut := fs.Bool("json", false, "emit the FlowResult as JSON on stdout")
	_ = fs.Parse(args[1:])
	if fs.NArg() != 1 {
		return fmt.Errorf("flow %s: need exactly one argument (see `loom flow`)", sub)
	}

	var ff *loom.FlowFile
	var flowDir string
	var resume map[string]loom.FlowRecord
	switch sub {
	case "run":
		f, err := loom.ParseFlowFile(fs.Arg(0))
		if err != nil {
			return err
		}
		flowDir = loom.FlowDir(f.Flow)
		if _, err := os.Stat(filepath.Join(flowDir, "journal.jsonl")); err == nil {
			if !*fresh {
				return fmt.Errorf("flow run: %q already has a journal (%s) — `loom flow resume %s` to continue it, or --fresh to archive and start over", f.Flow, flowDir, f.Flow)
			}
			archived := flowDir + "." + time.Now().Format("20060102-150405")
			if err := os.Rename(flowDir, archived); err != nil {
				return fmt.Errorf("flow run: archive old journal: %w", err)
			}
			fmt.Fprintf(os.Stderr, "[flow: archived previous journal to %s]\n", archived)
		}
		if err := loom.SaveFlowCopy(flowDir, f); err != nil {
			return fmt.Errorf("flow run: save flow copy: %w", err)
		}
		ff = f
	case "resume":
		flowDir = loom.FlowDir(fs.Arg(0))
		f, err := loom.ParseFlowFile(filepath.Join(flowDir, "flow.json"))
		if err != nil {
			return fmt.Errorf("flow resume: no saved flow for %q (did `loom flow run` create it?): %w", fs.Arg(0), err)
		}
		prev, err := loom.ReadFlowJournal(flowDir)
		if err != nil {
			return fmt.Errorf("flow resume: read journal: %w", err)
		}
		ff, resume = f, prev
	}

	obs := loom.FlowObserver{}
	if !*jsonOut {
		obs.StepStart = func(id string) { fmt.Fprintf(os.Stderr, "[flow] %s: started\n", id) }
		obs.StepDone = func(rec loom.FlowRecord) {
			note := ""
			if rec.Cached {
				note = " (cached)"
			}
			if rec.OracleRC != nil {
				note += fmt.Sprintf(" (oracle rc=%d)", *rec.OracleRC)
			}
			if rec.Status != loom.FlowGreen && rec.Reply != nil && rec.Reply.Err != "" {
				note += " — " + oneLine(rec.Reply.Err, 120)
			}
			fmt.Fprintf(os.Stderr, "[flow] %s: %s%s\n", rec.Step, rec.Status, note)
		}
	}

	res, err := loom.RunFlow(context.Background(), loom.FlowConfig{
		File:     ff,
		Backends: loom.Backends(),
		FlowDir:  flowDir,
		BaseOpts: loom.SessionOpts{
			Isolate: *isolate, SkipPermissions: *skipPerms,
			MCPConfig: *mcpConfig, SkillsDir: *skills,
		},
		Budget:      loom.NewBudget(*budget),
		Concurrency: *concurrency,
		Resume:      resume,
		Observer:    obs,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			return err
		}
	} else {
		green := 0
		for _, r := range res.Records {
			if r.Status == loom.FlowGreen {
				green++
			}
		}
		fmt.Printf("flow %s: %d/%d steps green — journal: %s\n", res.Flow, green, len(ff.Steps), flowDir)
	}
	if !res.AllGreen {
		return fmt.Errorf("flow %s: not all steps green (resume: loom flow resume %s)", res.Flow, res.Flow)
	}
	return nil
}
