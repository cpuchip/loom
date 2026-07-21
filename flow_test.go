package loom

// flow_test.go — hermetic tests for `loom flow` (flow.go): parse validation,
// DAG ordering, bounded parallelism, failure propagation (dependents stop,
// independents don't), resume-skips-green, journal round-trip, budget refusal.
// The fake backend records per-step sends and in-flight concurrency — no
// process, no money.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flowFake is a hermetic Backend for flow tests. failSteps marks step prompts
// that fail their turn; each session's reply carries usage so budget paths are
// exercised. Steps are identified by their prompt (the fake echoes it).
type flowFake struct {
	mu        sync.Mutex
	sends     []string       // prompts in completion order
	sendCount map[string]int // per-prompt send tally
	failOnce  map[string]bool
	inFlight  int32
	maxSeen   int32
	delay     time.Duration
	usageEach *Usage
}

func newFlowFake() *flowFake {
	return &flowFake{sendCount: map[string]int{}, failOnce: map[string]bool{}}
}

func (b *flowFake) Name() string { return "fake" }

func (b *flowFake) Open(_ context.Context, opts SessionOpts) (Session, error) {
	return &flowFakeSession{b: b, opts: opts}, nil
}

type flowFakeSession struct {
	b    *flowFake
	opts SessionOpts
}

func (s *flowFakeSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *flowFakeSession) SendStream(_ context.Context, prompt string, _ func(Event)) (Reply, error) {
	n := atomic.AddInt32(&s.b.inFlight, 1)
	for {
		max := atomic.LoadInt32(&s.b.maxSeen)
		if n <= max || atomic.CompareAndSwapInt32(&s.b.maxSeen, max, n) {
			break
		}
	}
	if s.b.delay > 0 {
		time.Sleep(s.b.delay)
	}
	defer atomic.AddInt32(&s.b.inFlight, -1)

	s.b.mu.Lock()
	s.b.sends = append(s.b.sends, prompt)
	s.b.sendCount[prompt]++
	fail := s.b.failOnce[prompt]
	if fail {
		delete(s.b.failOnce, prompt) // fail once, succeed on the re-run
	}
	usage := s.b.usageEach
	s.b.mu.Unlock()

	if fail {
		return Reply{Backend: "fake", Err: "scripted failure"}, fmt.Errorf("scripted failure")
	}
	return Reply{Backend: "fake", Text: "did:" + prompt, SessionID: "fx", Usage: usage}, nil
}

func (s *flowFakeSession) SessionID() string { return "fx" }
func (s *flowFakeSession) Close() error      { return nil }

func (b *flowFake) count(prompt string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendCount[prompt]
}

func (b *flowFake) order() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sends...)
}

// writeFlow writes a flow file into a temp dir and parses it.
func writeFlow(t *testing.T, doc string) (*FlowFile, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test-flow.json")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ParseFlowFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return f, dir
}

func runTestFlow(t *testing.T, f *FlowFile, fake *flowFake, mut func(*FlowConfig)) FlowResult {
	t.Helper()
	cfg := FlowConfig{
		File:     f,
		Backends: map[string]Backend{"fake": fake},
		FlowDir:  filepath.Join(t.TempDir(), "flowdir"),
	}
	if mut != nil {
		mut(&cfg)
	}
	res, err := RunFlow(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestFlowParseValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(doc string) string {
		p := filepath.Join(dir, "f.json")
		if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct{ name, doc, wantErr string }{
		{"no steps", `{"steps":[]}`, "no steps"},
		{"missing id", `{"steps":[{"prompt":"x"}]}`, "no id"},
		{"duplicate id", `{"steps":[{"id":"a","prompt":"x"},{"id":"a","prompt":"y"}]}`, "duplicate"},
		{"both prompts", `{"steps":[{"id":"a","prompt":"x","prompt_file":"y"}]}`, "exactly one"},
		{"neither prompt", `{"steps":[{"id":"a"}]}`, "exactly one"},
		{"unknown need", `{"steps":[{"id":"a","prompt":"x","needs":["zed"]}]}`, "unknown step"},
		{"self need", `{"steps":[{"id":"a","prompt":"x","needs":["a"]}]}`, "needs itself"},
		{"cycle", `{"steps":[{"id":"a","prompt":"x","needs":["b"]},{"id":"b","prompt":"y","needs":["a"]}]}`, "cycle"},
		{"missing prompt file", `{"steps":[{"id":"a","prompt_file":"nope.md"}]}`, "prompt_file"},
	}
	for _, c := range cases {
		if _, err := ParseFlowFile(write(c.doc)); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.wantErr)
		}
	}
	// defaults: flow name from the file, agent claude, dir = the flow file's dir
	p := write(`{"steps":[{"id":"a","prompt":"x"}]}`)
	f, err := ParseFlowFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Flow != "f" {
		t.Errorf("flow name = %q, want file basename", f.Flow)
	}
	if f.Steps[0].Agent != "claude" || f.Steps[0].Dir != dir {
		t.Errorf("defaults: agent=%q dir=%q", f.Steps[0].Agent, f.Steps[0].Dir)
	}
}

func TestFlowPromptFileResolves(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte("the brief"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "f.json")
	if err := os.WriteFile(p, []byte(`{"steps":[{"id":"a","agent":"fake","prompt_file":"brief.md"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ParseFlowFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFlowFake()
	res := runTestFlow(t, f, fake, nil)
	if !res.AllGreen || fake.count("the brief") != 1 {
		t.Errorf("prompt_file content must be the prompt: green=%v sends=%v", res.AllGreen, fake.order())
	}
}

func TestFlowDagOrdering(t *testing.T) {
	f, _ := writeFlow(t, `{"steps":[
		{"id":"c","agent":"fake","prompt":"pc","needs":["b"]},
		{"id":"a","agent":"fake","prompt":"pa"},
		{"id":"b","agent":"fake","prompt":"pb","needs":["a"]}]}`)
	fake := newFlowFake()
	res := runTestFlow(t, f, fake, nil)
	if !res.AllGreen {
		t.Fatalf("chain must go green: %+v", res.Records)
	}
	order := fake.order()
	if len(order) != 3 || order[0] != "pa" || order[1] != "pb" || order[2] != "pc" {
		t.Errorf("dependency order violated: %v", order)
	}
}

func TestFlowParallelIndependence(t *testing.T) {
	f, _ := writeFlow(t, `{"concurrency":3,"steps":[
		{"id":"a","agent":"fake","prompt":"pa"},
		{"id":"b","agent":"fake","prompt":"pb"},
		{"id":"c","agent":"fake","prompt":"pc"}]}`)
	fake := newFlowFake()
	fake.delay = 50 * time.Millisecond
	res := runTestFlow(t, f, fake, nil)
	if !res.AllGreen {
		t.Fatalf("independent steps must all green: %+v", res.Records)
	}
	if max := atomic.LoadInt32(&fake.maxSeen); max < 2 {
		t.Errorf("independent steps should overlap (max in-flight %d)", max)
	}

	// bounded: concurrency 1 serializes
	f2, _ := writeFlow(t, `{"steps":[
		{"id":"a","agent":"fake","prompt":"qa"},
		{"id":"b","agent":"fake","prompt":"qb"},
		{"id":"c","agent":"fake","prompt":"qc"}]}`)
	fake2 := newFlowFake()
	fake2.delay = 20 * time.Millisecond
	runTestFlow(t, f2, fake2, func(c *FlowConfig) { c.Concurrency = 1 })
	if max := atomic.LoadInt32(&fake2.maxSeen); max != 1 {
		t.Errorf("concurrency 1 must serialize (max in-flight %d)", max)
	}
}

func TestFlowFailureStopsDependentsNotIndependents(t *testing.T) {
	f, _ := writeFlow(t, `{"steps":[
		{"id":"a","agent":"fake","prompt":"pa"},
		{"id":"b","agent":"fake","prompt":"pb","needs":["a"]},
		{"id":"d","agent":"fake","prompt":"pd","needs":["b"]},
		{"id":"c","agent":"fake","prompt":"pc"}]}`)
	fake := newFlowFake()
	fake.failOnce["pa"] = true
	res := runTestFlow(t, f, fake, nil)
	if res.AllGreen {
		t.Fatal("a failed — the flow cannot be all green")
	}
	if res.Records["a"].Status != FlowAgentFailed {
		t.Errorf("a = %q", res.Records["a"].Status)
	}
	if res.Records["b"].Status != FlowDepFailed || res.Records["d"].Status != FlowDepFailed {
		t.Errorf("dependents must be dependency_failed (transitively): b=%q d=%q",
			res.Records["b"].Status, res.Records["d"].Status)
	}
	if res.Records["c"].Status != FlowGreen {
		t.Errorf("the independent branch must still run: c=%q", res.Records["c"].Status)
	}
	if fake.count("pb") != 0 || fake.count("pd") != 0 {
		t.Errorf("no turn may be spent on a dependent of a failure: pb=%d pd=%d", fake.count("pb"), fake.count("pd"))
	}
}

func TestFlowOracleGate(t *testing.T) {
	// real oracle, cross-platform: "exit 0" / "exit 1" work under cmd /C and sh -c
	f, _ := writeFlow(t, `{"steps":[
		{"id":"good","agent":"fake","prompt":"pg","oracle":"exit 0"},
		{"id":"bad","agent":"fake","prompt":"pb2","oracle":"exit 1"}]}`)
	fake := newFlowFake()
	res := runTestFlow(t, f, fake, nil)
	if res.Records["good"].Status != FlowGreen || *res.Records["good"].OracleRC != 0 {
		t.Errorf("good = %+v", res.Records["good"])
	}
	if res.Records["bad"].Status != FlowOracleFailed || *res.Records["bad"].OracleRC != 1 {
		t.Errorf("the turn succeeded but the oracle failed — status must be oracle_failed: %+v", res.Records["bad"])
	}
}

func TestFlowResumeSkipsGreen(t *testing.T) {
	doc := `{"flow":"resume-t","steps":[
		{"id":"a","agent":"fake","prompt":"pa"},
		{"id":"b","agent":"fake","prompt":"pb","needs":["a"]}]}`
	f, _ := writeFlow(t, doc)
	fake := newFlowFake()
	fake.failOnce["pb"] = true
	flowDir := filepath.Join(t.TempDir(), "resume-t")

	res1, err := RunFlow(context.Background(), FlowConfig{
		File: f, Backends: map[string]Backend{"fake": fake}, FlowDir: flowDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res1.AllGreen || res1.Records["a"].Status != FlowGreen || res1.Records["b"].Status != FlowAgentFailed {
		t.Fatalf("first run: %+v", res1.Records)
	}

	// resume: a is green in the journal → served cached; only b re-runs
	prev, err := ReadFlowJournal(flowDir)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := RunFlow(context.Background(), FlowConfig{
		File: f, Backends: map[string]Backend{"fake": fake}, FlowDir: flowDir, Resume: prev,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.AllGreen {
		t.Fatalf("resume must finish green: %+v", res2.Records)
	}
	if !res2.Records["a"].Cached {
		t.Error("a must be served from the journal (cached)")
	}
	if fake.count("pa") != 1 {
		t.Errorf("a must NOT re-run on resume (sends=%d)", fake.count("pa"))
	}
	if fake.count("pb") != 2 {
		t.Errorf("b must re-run on resume (sends=%d)", fake.count("pb"))
	}
	// the journal's latest word: both green now
	final, err := ReadFlowJournal(flowDir)
	if err != nil {
		t.Fatal(err)
	}
	if final["a"].Status != FlowGreen || final["b"].Status != FlowGreen {
		t.Errorf("journal latest: %+v", final)
	}
}

func TestFlowJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rc := 1
	recs := []FlowRecord{
		{Step: "a", Status: FlowOracleFailed, OracleRC: &rc, OracleTail: "boom", Reply: &Reply{Text: "t1", Usage: &Usage{OutputTokens: 5, CostSource: CostNone}}},
		{Step: "a", Status: FlowGreen, Reply: &Reply{Text: "t2"}},
		{Step: "b", Status: FlowGreen},
	}
	jf, err := os.Create(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		b, _ := json.Marshal(r)
		fmt.Fprintf(jf, "%s\n", b)
	}
	fmt.Fprint(jf, `{"step":"torn`) // a torn tail line must not poison the history
	jf.Close()

	got, err := ReadFlowJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("latest-per-step: %v", got)
	}
	if got["a"].Status != FlowGreen || got["a"].Reply.Text != "t2" {
		t.Errorf("later record must supersede: %+v", got["a"])
	}
	// missing journal = empty, not an error
	empty, err := ReadFlowJournal(filepath.Join(dir, "nowhere"))
	if err != nil || len(empty) != 0 {
		t.Errorf("missing journal: %v %v", empty, err)
	}
}

func TestFlowBudgetRefusal(t *testing.T) {
	f, _ := writeFlow(t, `{"steps":[
		{"id":"a","agent":"fake","prompt":"pa"},
		{"id":"b","agent":"fake","prompt":"pb","needs":["a"]},
		{"id":"c","agent":"fake","prompt":"pc","needs":["b"]}]}`)
	fake := newFlowFake()
	fake.usageEach = &Usage{InputTokens: 600, CostSource: CostNone} // each turn burns 600 tokens
	res := runTestFlow(t, f, fake, func(c *FlowConfig) {
		c.Budget = NewBudget(500) // the FIRST turn crosses it
		c.Concurrency = 1
	})
	if res.AllGreen {
		t.Fatal("budget must stop the flow")
	}
	if res.Records["a"].Status != FlowGreen {
		t.Errorf("the turn in flight completes: a=%q", res.Records["a"].Status)
	}
	if res.Records["b"].Status != FlowBudgetRefused {
		t.Errorf("the next step must be refused, not run: b=%q", res.Records["b"].Status)
	}
	if res.Records["c"].Status != FlowDepFailed {
		t.Errorf("dependents of a refused step are dependency_failed: c=%q", res.Records["c"].Status)
	}
	if fake.count("pb") != 0 && fake.count("pc") != 0 {
		t.Errorf("no turns past the ceiling: pb=%d pc=%d", fake.count("pb"), fake.count("pc"))
	}
}
