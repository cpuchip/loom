package loom

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// scriptSession is a hermetic, pre-scripted Session for the duo tests: it plays a fixed
// list of replies in order and records every prompt it received + the opts it was opened
// with, so a test can assert routing (which message reached the worker vs. the critic) and
// the read-only guarantee (Consult on the critic's open). No process, no model, no money —
// the duo analog of stubSession, but scriptable per turn (stubSession only echoes).
type scriptSession struct {
	id      string
	replies []Reply
	events  []Event // optional: emitted each turn (exercises the -events path)

	mu      sync.Mutex
	idx     int
	prompts []string
	opts    SessionOpts
}

func (s *scriptSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *scriptSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	for _, ev := range s.events {
		emit(onEvent, ev)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, prompt)
	var r Reply
	if s.idx < len(s.replies) {
		r = s.replies[s.idx]
		s.idx++
	}
	r.SessionID = s.id
	return r, nil
}

func (s *scriptSession) SessionID() string { return s.id }
func (s *scriptSession) Close() error      { return nil }

func (s *scriptSession) prompt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.prompts) {
		return ""
	}
	return s.prompts[i]
}

func (s *scriptSession) turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts)
}

// scriptBackend hands out pre-built scriptSessions in Open order (worker first, then
// critic — the order Duo opens them) and records the opts of each Open. One backend can
// serve BOTH seats (the critic-defaults-to-worker case): sessions[0] is the worker,
// sessions[1] the critic.
type scriptBackend struct {
	name     string
	sessions []*scriptSession

	mu    sync.Mutex
	opens []SessionOpts
}

func (b *scriptBackend) Name() string { return b.name }

func (b *scriptBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := len(b.opens)
	b.opens = append(b.opens, opts)
	s := b.sessions[i]
	s.opts = opts
	return s, nil
}

func (b *scriptBackend) openOpts(i int) SessionOpts {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= len(b.opens) {
		return SessionOpts{}
	}
	return b.opens[i]
}

// rep is a tiny constructor for a scripted reply (text only).
func rep(text string) Reply { return Reply{Text: text} }

// criticReply builds a critic reply: feedback above a trailing verdict line, the shape the
// prompts ask the critic to produce.
func criticReply(feedback, verdict string) Reply {
	return rep(feedback + "\nVERDICT: " + verdict)
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantVerdict string
		wantFeed    string
		wantOK      bool
	}{
		{"continue", "looks fine\nVERDICT: CONTINUE", verdictContinue, "looks fine", true},
		{"revise", "you missed the test\nVERDICT: REVISE", verdictRevise, "you missed the test", true},
		{"done", "all green\nVERDICT: DONE", verdictDone, "all green", true},
		{"garbage", "I have thoughts but no verdict line", "", "I have thoughts but no verdict line", false},
		{"trailing wins", "VERDICT: REVISE early\nmore\nVERDICT: DONE", verdictDone, "VERDICT: REVISE early\nmore", true},
		{"lowercase + spaces", "ok\n  verdict:  continue  ", verdictContinue, "ok", true},
		{"no feedback", "VERDICT: DONE", verdictDone, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, f, ok := parseVerdict(c.in)
			if v != c.wantVerdict || ok != c.wantOK || f != c.wantFeed {
				t.Errorf("parseVerdict(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.in, v, f, ok, c.wantVerdict, c.wantFeed, c.wantOK)
			}
		})
	}
}

func TestWorkerReportedComplete(t *testing.T) {
	if !WorkerReportedComplete("  BUILD COMPLETE\nshipped it") {
		t.Error("should detect the sentinel with leading whitespace")
	}
	if WorkerReportedComplete("almost BUILD COMPLETE, one more thing") {
		t.Error("the sentinel must be at the START of the reply, not mid-text")
	}
}

// TestDuoDoneTerminates: the critic says DONE on round 1 → the loop ends after one round,
// status done, and Text is the worker's report the critic judged.
func TestDuoDoneTerminates(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("did the thing")}}
	critic := &scriptSession{id: "c", replies: []Reply{criticReply("perfect", verdictDone)}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	res, err := Duo(context.Background(), DuoConfig{Worker: wb, Critic: cb, Task: "TASKMARKER", Rounds: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != duoStatusDone {
		t.Errorf("status = %q, want %q", res.Status, duoStatusDone)
	}
	if len(res.Rounds) != 1 || res.Rounds[0].Verdict != verdictDone {
		t.Errorf("rounds = %+v, want one DONE", res.Rounds)
	}
	if res.Text != "did the thing" {
		t.Errorf("text = %q, want the worker's report", res.Text)
	}
	if worker.turns() != 1 || critic.turns() != 1 {
		t.Errorf("turns worker=%d critic=%d, want 1/1", worker.turns(), critic.turns())
	}
	if res.WorkerSession != "w" || res.CriticSession != "c" {
		t.Errorf("sessions = %q/%q, want w/c", res.WorkerSession, res.CriticSession)
	}
	// Round 1's worker prompt carries the task + the loom-appended preamble.
	if !strings.Contains(worker.prompt(0), "TASKMARKER") || !strings.Contains(worker.prompt(0), DuoBuildComplete) {
		t.Errorf("worker turn 1 should contain the task and the duo preamble: %q", worker.prompt(0))
	}
}

// TestDuoReviseRoutesFeedback: round 1 REVISE → the critic's feedback is routed into the
// worker (led by the revise prefix); round 2 DONE ends it. Also proves the critic's round-2
// prompt does NOT re-send the task (the resumed critic session accumulates it).
func TestDuoReviseRoutesFeedback(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("first pass"), rep("fixed it")}}
	critic := &scriptSession{id: "c", replies: []Reply{
		criticReply("you forgot the error check", verdictRevise),
		criticReply("good now", verdictDone),
	}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	res, err := Duo(context.Background(), DuoConfig{Worker: wb, Critic: cb, Task: "TASKMARKER", Rounds: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != duoStatusDone || len(res.Rounds) != 2 {
		t.Fatalf("status=%q rounds=%d, want done/2", res.Status, len(res.Rounds))
	}
	if res.Rounds[0].Verdict != verdictRevise || res.Rounds[1].Verdict != verdictDone {
		t.Errorf("verdicts = %q,%q, want REVISE,DONE", res.Rounds[0].Verdict, res.Rounds[1].Verdict)
	}
	w2 := worker.prompt(1)
	if !strings.HasPrefix(w2, duoReviseLead) {
		t.Errorf("worker turn 2 not led by the revise prefix: %q", w2)
	}
	if !strings.Contains(w2, "you forgot the error check") {
		t.Errorf("worker turn 2 missing the critic's feedback: %q", w2)
	}
	if res.Rounds[0].FeedbackSummary != "you forgot the error check" {
		t.Errorf("feedback_summary = %q", res.Rounds[0].FeedbackSummary)
	}
	// critic round 1 gets the task; round 2 gets only the worker's latest report.
	if !strings.Contains(critic.prompt(0), "TASKMARKER") {
		t.Errorf("critic turn 1 should carry the task: %q", critic.prompt(0))
	}
	if strings.Contains(critic.prompt(1), "TASKMARKER") {
		t.Errorf("critic turn 2 should NOT re-send the task (session is resumed): %q", critic.prompt(1))
	}
	if !strings.Contains(critic.prompt(1), "fixed it") {
		t.Errorf("critic turn 2 should carry the worker's 2nd report: %q", critic.prompt(1))
	}
}

// TestDuoContinueProceeds: CONTINUE routes the fixed "proceed" message into the worker (not
// the critic's feedback), then DONE ends it.
func TestDuoContinueProceeds(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("step 1"), rep("step 2")}}
	critic := &scriptSession{id: "c", replies: []Reply{
		criticReply("keep going, looking good", verdictContinue),
		criticReply("complete", verdictDone),
	}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	res, err := Duo(context.Background(), DuoConfig{Worker: wb, Critic: cb, Task: "TASKMARKER", Rounds: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rounds[0].Verdict != verdictContinue {
		t.Errorf("round 1 verdict = %q, want CONTINUE", res.Rounds[0].Verdict)
	}
	if worker.prompt(1) != duoContinueMsg {
		t.Errorf("worker turn 2 = %q, want the fixed proceed message %q", worker.prompt(1), duoContinueMsg)
	}
}

// TestDuoRoundsCapExhausted: the critic never says DONE → the loop stops at the --rounds cap
// with status rounds_exhausted, and Text is the last worker report.
func TestDuoRoundsCapExhausted(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("a"), rep("b"), rep("c"), rep("d")}}
	critic := &scriptSession{id: "c", replies: []Reply{
		criticReply("go", verdictContinue),
		criticReply("go", verdictContinue),
		criticReply("go", verdictContinue),
		criticReply("go", verdictContinue),
	}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	res, err := Duo(context.Background(), DuoConfig{Worker: wb, Critic: cb, Task: "T", Rounds: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != duoStatusExhausted {
		t.Errorf("status = %q, want %q", res.Status, duoStatusExhausted)
	}
	if len(res.Rounds) != 3 {
		t.Errorf("rounds = %d, want 3 (the cap)", len(res.Rounds))
	}
	if worker.turns() != 3 || critic.turns() != 3 {
		t.Errorf("turns worker=%d critic=%d, want 3/3", worker.turns(), critic.turns())
	}
	if res.Text != "c" {
		t.Errorf("text = %q, want the 3rd worker report", res.Text)
	}
}

// TestDuoCriticAlwaysConsult: even when the caller passes CriticOpts.Consult=false, Duo
// forces the critic's open to Consult=true — while leaving the worker's flags untouched.
func TestDuoCriticAlwaysConsult(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("x")}}
	critic := &scriptSession{id: "c", replies: []Reply{criticReply("ok", verdictDone)}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	_, err := Duo(context.Background(), DuoConfig{
		Worker: wb, Critic: cb, Task: "T", Rounds: 6,
		WorkerOpts: SessionOpts{Workdir: "/repo"},
		CriticOpts: SessionOpts{Workdir: "/repo", Consult: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cb.openOpts(0).Consult {
		t.Error("critic must ALWAYS be opened with Consult=true")
	}
	if wb.openOpts(0).Consult {
		t.Error("worker must keep the caller's flags — Consult not forced on the worker")
	}
	if cb.openOpts(0).Workdir != "/repo" {
		t.Errorf("critic should share the worker's --dir, got %q", cb.openOpts(0).Workdir)
	}
}

// TestDuoCriticDefaultsToWorker: a caller that leaves Critic nil and CriticOpts.Model empty
// gets the worker's backend + model for the critic seat. One backend serves both seats:
// sessions[0] the worker, sessions[1] the critic.
func TestDuoCriticDefaultsToWorker(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("x")}}
	critic := &scriptSession{id: "c", replies: []Reply{criticReply("ok", verdictDone)}}
	bk := &scriptBackend{name: "solo", sessions: []*scriptSession{worker, critic}}

	res, err := Duo(context.Background(), DuoConfig{
		Worker: bk, Critic: nil, Task: "T", Rounds: 6,
		WorkerOpts: SessionOpts{Model: "sonnet"},
		CriticOpts: SessionOpts{}, // Model empty → defaults to the worker's "sonnet"
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CriticSession != "c" {
		t.Errorf("critic session = %q, want c (opened on the worker's backend)", res.CriticSession)
	}
	if bk.openOpts(1).Model != "sonnet" {
		t.Errorf("critic model = %q, want sonnet (defaulted from the worker)", bk.openOpts(1).Model)
	}
	if !bk.openOpts(1).Consult {
		t.Error("critic still forced read-only when defaulted from the worker")
	}
}

// TestDuoBuildCompleteStillCriticd: the worker declares BUILD COMPLETE on turn 1, but the
// critic isn't convinced (CONTINUE). BUILD COMPLETE must NOT end the loop by itself — only
// the critic's DONE does.
func TestDuoBuildCompleteStillCriticd(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{
		rep("BUILD COMPLETE\nshipped it"),
		rep("addressed the gap"),
	}}
	critic := &scriptSession{id: "c", replies: []Reply{
		criticReply("you skipped the test — not done", verdictContinue),
		criticReply("now it's real", verdictDone),
	}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	res, err := Duo(context.Background(), DuoConfig{Worker: wb, Critic: cb, Task: "T", Rounds: 6})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rounds) != 2 || res.Status != duoStatusDone {
		t.Fatalf("BUILD COMPLETE should not short-circuit: rounds=%d status=%q", len(res.Rounds), res.Status)
	}
	// The critic WAS consulted on the BUILD COMPLETE report (round 1) and again on the fix.
	if critic.turns() != 2 {
		t.Errorf("critic turns = %d, want 2 (judged the BUILD COMPLETE report AND the fix)", critic.turns())
	}
}

// TestDuoFailsOpenOnGarbledVerdict: an unparseable critic reply is treated as CONTINUE (a
// warning fires, the loop keeps going, and the garbage is NOT routed as feedback).
func TestDuoFailsOpenOnGarbledVerdict(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("one"), rep("two")}}
	critic := &scriptSession{id: "c", replies: []Reply{
		rep("I forgot to write a verdict line"),
		criticReply("ok", verdictDone),
	}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	var warned int
	res, err := Duo(context.Background(), DuoConfig{
		Worker: wb, Critic: cb, Task: "T", Rounds: 6,
		Observer: DuoObserver{Warn: func(string) { warned++ }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if warned != 1 {
		t.Errorf("warn count = %d, want 1 (the garbled verdict)", warned)
	}
	if res.Rounds[0].Verdict != verdictContinue {
		t.Errorf("garbled verdict should record CONTINUE, got %q", res.Rounds[0].Verdict)
	}
	if worker.prompt(1) != duoContinueMsg {
		t.Errorf("fail-open should route the proceed message, not garbage: %q", worker.prompt(1))
	}
	if res.Status != duoStatusDone {
		t.Errorf("status = %q, want done (recovered on round 2)", res.Status)
	}
}

// TestDuoCostSummed: the result sums cost across both seats and every round. Values are
// exact in binary (0.25 + 0.125 = 0.375) so the assertion needs no float tolerance.
func TestDuoCostSummed(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{{Text: "x", CostUSD: 0.25}}}
	critic := &scriptSession{id: "c", replies: []Reply{{Text: "ok\nVERDICT: DONE", CostUSD: 0.125}}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	res, err := Duo(context.Background(), DuoConfig{Worker: wb, Critic: cb, Task: "T", Rounds: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.CostUSD != 0.375 {
		t.Errorf("cost = %v, want 0.375 (0.25 worker + 0.125 critic)", res.CostUSD)
	}
}

// TestDuoEventsTagged: with the event hooks set, each seat's tool events reach the observer,
// so the CLI can tag them [worker]/[critic]. (nil hooks take the plain final-text path.)
func TestDuoEventsTagged(t *testing.T) {
	worker := &scriptSession{id: "w", replies: []Reply{rep("x")}, events: []Event{{Kind: EvToolCall, Tool: "Bash"}}}
	critic := &scriptSession{id: "c", replies: []Reply{criticReply("ok", verdictDone)}, events: []Event{{Kind: EvToolCall, Tool: "Read"}}}
	wb := &scriptBackend{name: "wb", sessions: []*scriptSession{worker}}
	cb := &scriptBackend{name: "cb", sessions: []*scriptSession{critic}}

	var wEv, cEv int
	_, err := Duo(context.Background(), DuoConfig{
		Worker: wb, Critic: cb, Task: "T", Rounds: 6,
		Observer: DuoObserver{
			WorkerEvent: func(Event) { wEv++ },
			CriticEvent: func(Event) { cEv++ },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wEv != 1 || cEv != 1 {
		t.Errorf("events worker=%d critic=%d, want 1/1", wEv, cEv)
	}
}
