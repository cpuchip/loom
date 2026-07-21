package loom

// flow.go — `loom flow`: deterministic multi-step orchestration with cached
// resume (native-harness parity #5; design: docs/proposals/loom-flow.md).
//
// A flow file declares steps — each one loom session with its own working dir,
// prompt, dependency edges, and a deterministic ORACLE (shell cmd, exit 0 =
// green). The scheduler runs the DAG with bounded parallelism; every step's
// Reply + oracle verdict is journaled under $LOOM_HOME/flows/<flow-id>/; a
// resume SKIPS steps whose latest journal record is green. Green = cached is
// only sound because green is an exit code, not the model's opinion — the
// oracle is the load-bearing piece.
//
// `loom race` = N contenders, one oracle (competition). `loom duo` = worker +
// critic (opposition). `loom flow` = many steps, each with its OWN oracle,
// dependency-ordered (composition).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Flow-record statuses (the journal's `status` field).
const (
	FlowGreen         = "green"
	FlowAgentFailed   = "agent_failed"
	FlowOracleFailed  = "oracle_failed"
	FlowDepFailed     = "dependency_failed"
	FlowBudgetRefused = "budget_refused"
)

// FlowStep is one step of a flow file. Dir and PromptFile are resolved to
// ABSOLUTE paths at parse (against the flow file's directory), so the saved
// copy under LOOM_HOME is self-contained and a resume needs no original.
type FlowStep struct {
	ID         string   `json:"id"`
	Agent      string   `json:"agent,omitempty"` // default "claude"
	Model      string   `json:"model,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	Prompt     string   `json:"prompt,omitempty"`
	PromptFile string   `json:"prompt_file,omitempty"`
	Needs      []string `json:"needs,omitempty"`
	Oracle     string   `json:"oracle,omitempty"` // "" = the turn succeeding is green (weaker)
}

// FlowFile is a parsed, validated flow.
type FlowFile struct {
	Flow        string     `json:"flow"`
	Concurrency int        `json:"concurrency,omitempty"`
	Steps       []FlowStep `json:"steps"`
}

// ParseFlowFile reads + validates a flow file and resolves relative step dirs /
// prompt files against the flow file's own directory (flows are portable; the
// file is the anchor). Validation is a hard gate: nothing runs on a flow with
// duplicate ids, unknown needs, cycles, a prompt/prompt_file conflict, or an
// unreadable prompt file — half a flow burned on a typo is the failure mode
// this prevents.
func ParseFlowFile(path string) (*FlowFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("flow: %w", err)
	}
	var f FlowFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("flow: parse %s: %w", path, err)
	}
	if f.Flow == "" {
		f.Flow = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("flow: %w", err)
	}
	base := filepath.Dir(abs)
	if len(f.Steps) == 0 {
		return nil, errors.New("flow: no steps")
	}
	byID := map[string]bool{}
	for i := range f.Steps {
		st := &f.Steps[i]
		if st.ID == "" {
			return nil, fmt.Errorf("flow: step %d has no id", i)
		}
		if byID[st.ID] {
			return nil, fmt.Errorf("flow: duplicate step id %q", st.ID)
		}
		byID[st.ID] = true
		if (st.Prompt == "") == (st.PromptFile == "") {
			return nil, fmt.Errorf("flow: step %q needs exactly one of prompt / prompt_file", st.ID)
		}
		if st.Agent == "" {
			st.Agent = "claude"
		}
		if st.Dir == "" {
			st.Dir = base
		} else if !filepath.IsAbs(st.Dir) {
			st.Dir = filepath.Join(base, st.Dir)
		}
		if st.PromptFile != "" {
			if !filepath.IsAbs(st.PromptFile) {
				st.PromptFile = filepath.Join(base, st.PromptFile)
			}
			if _, err := os.Stat(st.PromptFile); err != nil {
				return nil, fmt.Errorf("flow: step %q: prompt_file: %w", st.ID, err)
			}
		}
	}
	for _, st := range f.Steps {
		for _, n := range st.Needs {
			if !byID[n] {
				return nil, fmt.Errorf("flow: step %q needs unknown step %q", st.ID, n)
			}
			if n == st.ID {
				return nil, fmt.Errorf("flow: step %q needs itself", st.ID)
			}
		}
	}
	if cyc := flowCycle(f.Steps); cyc != "" {
		return nil, fmt.Errorf("flow: dependency cycle through %q", cyc)
	}
	return &f, nil
}

// flowCycle returns a step id on a dependency cycle ("" = acyclic). Iterative
// Kahn peel: whatever cannot be peeled sits on (or behind) a cycle.
func flowCycle(steps []FlowStep) string {
	remaining := map[string][]string{}
	for _, st := range steps {
		remaining[st.ID] = append([]string(nil), st.Needs...)
	}
	for changed := true; changed; {
		changed = false
		for id, needs := range remaining {
			ok := true
			for _, n := range needs {
				if _, still := remaining[n]; still {
					ok = false
					break
				}
			}
			if ok {
				delete(remaining, id)
				changed = true
			}
		}
	}
	for id := range remaining {
		return id
	}
	return ""
}

// FlowRecord is one journal line — a step attempt's full account.
type FlowRecord struct {
	Step       string    `json:"step"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Reply      *Reply    `json:"reply,omitempty"`
	OracleRC   *int      `json:"oracle_rc,omitempty"`
	OracleTail string    `json:"oracle_tail,omitempty"`
	Cached     bool      `json:"cached,omitempty"` // resume served this green from the journal
}

// ReadFlowJournal reads <flowDir>/journal.jsonl and returns the LATEST record
// per step — a later green supersedes an earlier failure. A missing journal is
// an empty map (a flow that never ran).
func ReadFlowJournal(flowDir string) (map[string]FlowRecord, error) {
	raw, err := os.ReadFile(filepath.Join(flowDir, "journal.jsonl"))
	if os.IsNotExist(err) {
		return map[string]FlowRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]FlowRecord{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec FlowRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // a torn tail line (crash mid-write) must not poison the readable history
		}
		out[rec.Step] = rec
	}
	return out, nil
}

// FlowObserver narrates a flow run (all fields optional / nil-safe).
type FlowObserver struct {
	StepStart func(id string)
	StepDone  func(rec FlowRecord)
	Warn      func(msg string)
}

func (o FlowObserver) start(id string) {
	if o.StepStart != nil {
		o.StepStart(id)
	}
}
func (o FlowObserver) done(rec FlowRecord) {
	if o.StepDone != nil {
		o.StepDone(rec)
	}
}

// FlowConfig is one flow execution.
type FlowConfig struct {
	File     *FlowFile
	Backends map[string]Backend
	FlowDir  string // $LOOM_HOME/flows/<flow-id> — the journal home (created if missing)
	// BaseOpts carries the FLOW-WIDE trust/plumbing flags (Isolate,
	// SkipPermissions, MCPConfig, SkillsDir) applied to every step's session;
	// per-step Workdir/Model override it.
	BaseOpts    SessionOpts
	Budget      *Budget // nil = no ceiling
	Concurrency int     // overrides File.Concurrency when > 0
	Resume      map[string]FlowRecord
	Observer    FlowObserver
}

// FlowResult is the outcome: the final record per step, and whether every step
// went green (the exit-code contract).
type FlowResult struct {
	Flow     string                `json:"flow"`
	Records  map[string]FlowRecord `json:"records"`
	AllGreen bool                  `json:"all_green"`
}

// stepOutcome crosses the worker→scheduler channel.
type stepOutcome struct {
	rec FlowRecord
}

// RunFlow executes the DAG: a bounded pool dispatches any step whose needs are
// all green; a failed step marks its transitive dependents dependency_failed
// (recorded, never silent) while independent branches keep running; the budget
// gates DISPATCH (in-flight steps complete). Resume entries whose status is
// green are served from the journal as cached results. Every outcome is
// appended to the journal AS IT HAPPENS — a crash mid-flow loses only the
// in-flight steps, which is exactly what resume re-runs.
func RunFlow(ctx context.Context, cfg FlowConfig) (FlowResult, error) {
	res := FlowResult{Flow: cfg.File.Flow, Records: map[string]FlowRecord{}}
	if err := os.MkdirAll(cfg.FlowDir, 0o755); err != nil {
		return res, fmt.Errorf("flow: %w", err)
	}
	journal, err := os.OpenFile(filepath.Join(cfg.FlowDir, "journal.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return res, fmt.Errorf("flow: open journal: %w", err)
	}
	defer journal.Close()
	record := func(rec FlowRecord) { // scheduler-goroutine only — single writer
		res.Records[rec.Step] = rec
		if b, err := json.Marshal(rec); err == nil {
			fmt.Fprintf(journal, "%s\n", b)
		}
		cfg.Observer.done(rec)
	}

	// Cached greens first: a resumed green is a real result, served from the
	// journal — re-recorded with cached:true so this run's journal reads whole.
	for _, prev := range cfg.Resume {
		if prev.Status == FlowGreen {
			prev.Cached = true
			record(prev)
		}
	}

	limit := cfg.Concurrency
	if limit <= 0 {
		limit = cfg.File.Concurrency
	}
	if limit <= 0 {
		limit = 3
	}
	steps := cfg.File.Steps
	started := map[string]bool{}
	running := 0
	results := make(chan stepOutcome)

	finished := func(id string) (string, bool) { r, ok := res.Records[id]; return r.Status, ok }
	for {
		// Marking fixpoint: settle everything decidable without a turn —
		// dependency failures propagate transitively, budget refusals stamp
		// not-yet-started steps — then dispatch what's ready, up to the limit.
		for changed := true; changed; {
			changed = false
			for _, st := range steps {
				if started[st.ID] {
					continue
				}
				if _, done := finished(st.ID); done {
					continue
				}
				failedNeed := false
				readyNeeds := true
				for _, n := range st.Needs {
					s, done := finished(n)
					if !done {
						readyNeeds = false
						continue
					}
					if s != FlowGreen {
						failedNeed = true
					}
				}
				now := time.Now()
				switch {
				case failedNeed:
					record(FlowRecord{Step: st.ID, Status: FlowDepFailed, StartedAt: now, FinishedAt: now})
					changed = true
				case readyNeeds && cfg.Budget.Exceeded():
					record(FlowRecord{Step: st.ID, Status: FlowBudgetRefused, StartedAt: now, FinishedAt: now})
					changed = true
				case readyNeeds && running < limit:
					started[st.ID] = true
					running++
					cfg.Observer.start(st.ID)
					go func(st FlowStep) {
						results <- stepOutcome{rec: cfg.runStep(ctx, st)}
					}(st)
					changed = true
				}
			}
		}
		if running == 0 {
			break // nothing in flight and nothing dispatchable — the flow is settled
		}
		out := <-results
		running--
		cfg.Budget.Note(replyOf(out.rec))
		record(out.rec)
	}

	res.AllGreen = true
	for _, st := range steps {
		if r, ok := res.Records[st.ID]; !ok || r.Status != FlowGreen {
			res.AllGreen = false
		}
	}
	return res, nil
}

func replyOf(rec FlowRecord) Reply {
	if rec.Reply != nil {
		return *rec.Reply
	}
	return Reply{}
}

// runStep runs ONE step: a fresh session in the step's dir, one turn, then the
// oracle in the same dir. Runs on a worker goroutine; everything it touches is
// its own.
func (cfg FlowConfig) runStep(ctx context.Context, st FlowStep) FlowRecord {
	rec := FlowRecord{Step: st.ID, StartedAt: time.Now()}
	fail := func(status string, r Reply) FlowRecord {
		rec.Status = status
		rec.Reply = &r
		rec.FinishedAt = time.Now()
		return rec
	}
	prompt := st.Prompt
	if st.PromptFile != "" {
		b, err := os.ReadFile(st.PromptFile)
		if err != nil { // validated at parse; re-checked because resume re-reads
			return fail(FlowAgentFailed, Reply{Err: "prompt_file: " + err.Error()})
		}
		prompt = string(b)
	}
	be := cfg.Backends[st.Agent]
	if be == nil {
		return fail(FlowAgentFailed, Reply{Err: fmt.Sprintf("unknown agent %q", st.Agent)})
	}
	opts := cfg.BaseOpts
	opts.Workdir = st.Dir
	opts.Model = st.Model
	sess, err := be.Open(ctx, opts)
	if err != nil {
		return fail(FlowAgentFailed, Reply{Backend: st.Agent, Err: err.Error()})
	}
	r, err := sess.Send(ctx, prompt)
	_ = sess.Close()
	if err != nil {
		if r.Err == "" {
			r.Err = err.Error()
		}
		return fail(FlowAgentFailed, r)
	}
	if r.Err != "" {
		return fail(FlowAgentFailed, r)
	}
	rec.Reply = &r
	if st.Oracle != "" {
		rc, tail := runFlowOracle(ctx, st.Oracle, st.Dir)
		rec.OracleRC = &rc
		rec.OracleTail = tail
		if rc != 0 {
			rec.Status = FlowOracleFailed
			rec.FinishedAt = time.Now()
			return rec
		}
	}
	rec.Status = FlowGreen
	rec.FinishedAt = time.Now()
	return rec
}

// runFlowOracle runs the step's oracle in its dir (the `loom race` oracle
// contract: cmd /C on Windows, sh -c elsewhere; exit 0 = green) and captures a
// bounded output tail for the journal.
func runFlowOracle(ctx context.Context, command, dir string) (rc int, tail string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	const tailMax = 2000
	if len(out) > tailMax {
		out = out[len(out)-tailMax:]
	}
	tail = string(out)
	if err == nil {
		return 0, tail
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), tail
	}
	return -1, tail + "\noracle: " + err.Error()
}

// FlowDir returns the journal home for a flow id: <loom home>/flows/<id>.
func FlowDir(flowID string) string { return filepath.Join(Home(), "flows", flowID) }

// SaveFlowCopy writes the RESOLVED flow (absolute dirs/prompt files) into the
// flow dir — the deterministic source a resume reads.
func SaveFlowCopy(flowDir string, f *FlowFile) error {
	if err := os.MkdirAll(flowDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(flowDir, "flow.json"), b, 0o644)
}
