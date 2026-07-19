package main

// Unit proofs for the commission state machine, the concurrency cap, and the tap
// gate logic — all with fakes, no real serve or Postgres. These pin the behaviors
// the e2e then confirms on the real path: read-only opens without a tap, writable
// waits behind the gate and opens only on approve (refuses on decline), the cap
// refuses the over-cap open, and close e-stops (kills) a live seat.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cpuchip/loom"
)

// --- fakes ---

type fakeSession struct {
	mu     sync.Mutex
	closed bool
	sends  []string
}

func (f *fakeSession) Send(_ context.Context, p string) (loom.Reply, error) {
	f.mu.Lock()
	f.sends = append(f.sends, p)
	f.mu.Unlock()
	return loom.Reply{Backend: "fake", Text: "ack: " + p}, nil
}
func (f *fakeSession) SendStream(ctx context.Context, p string, _ func(loom.Event)) (loom.Reply, error) {
	return f.Send(ctx, p)
}
func (f *fakeSession) SessionID() string { return "fake-id" }
func (f *fakeSession) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeSession) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type fakeOpener struct {
	mu       sync.Mutex
	opened   []*fakeSession
	failNext bool

	// serve-wide overview/kill fakes (the surface a real connectOpener reaches over
	// the ws protocol). serveSessions is what overview returns; killed records the
	// kill targets; serveErr, if set, makes overview fail (to prove graceful degrade).
	serveSessions []loom.SessionOverview
	serveErr      error
	killed        []string
	killFound     bool
	killKind      string
}

func (o *fakeOpener) open(_ context.Context, _ string, _ loom.SessionOpts) (loom.Session, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failNext {
		o.failNext = false
		return nil, errors.New("boom")
	}
	s := &fakeSession{}
	o.opened = append(o.opened, s)
	return s, nil
}

func (o *fakeOpener) overview(_ context.Context) ([]loom.SessionOverview, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.serveErr != nil {
		return nil, o.serveErr
	}
	return o.serveSessions, nil
}

func (o *fakeOpener) kill(_ context.Context, target string) (string, string, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.killed = append(o.killed, target)
	if !o.killFound {
		return "", "", false, nil
	}
	return o.killKind, "stopped " + target, true, nil
}

// fakeGate models the substrate tap gate. status starts "pending"; a test flips
// it to approve/decline to drive the poller.
type fakeGate struct {
	mu        sync.Mutex
	next      int64
	statuses  map[int64]string
	withdrawn map[int64]bool
}

func newFakeGate() *fakeGate {
	return &fakeGate{statuses: map[int64]string{}, withdrawn: map[int64]bool{}}
}
func (g *fakeGate) enqueue(_ context.Context, _, _, _ string, _ map[string]any) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	g.statuses[g.next] = "pending"
	return g.next, nil
}
func (g *fakeGate) status(_ context.Context, id int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statuses[id], nil
}
func (g *fakeGate) withdraw(_ context.Context, id int64, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.withdrawn[id] = true
	g.statuses[id] = "declined"
	return nil
}
func (g *fakeGate) set(id int64, status string) {
	g.mu.Lock()
	g.statuses[id] = status
	g.mu.Unlock()
}
func (g *fakeGate) wasWithdrawn(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.withdrawn[id]
}

// fakePlanner returns a trivial opts + info and never touches the filesystem.
type fakePlanner struct{ writableSeen []bool }

func (p *fakePlanner) plan(_ string, req openReq) (loom.SessionOpts, mountInfo, error) {
	p.writableSeen = append(p.writableSeen, req.writable)
	opts := loom.SessionOpts{Isolate: true, WorkdirRO: true}
	if req.writable {
		opts.ExtraMounts = []string{"h/work:/commission", "h/scratch:/scratch"}
	} else {
		opts.Consult = true
	}
	return opts, mountInfo{workHost: "h/work", scratchHost: "h/scratch", workspaceHost: "ws"}, nil
}
func (p *fakePlanner) cleanup(string) error { return nil }

func newTestManager(op opener, gt gate) *manager {
	m := newManager(op, gt, &fakePlanner{}, 5, time.Minute, nil)
	m.pollEvery = 2 * time.Millisecond
	m.pollTimeout = 2 * time.Second
	// Default to an empty CLI-worker table so tests never shell out to the real host
	// process query; the cli-worker tests override these two hooks explicitly.
	m.listCLI = func(context.Context) ([]cliWorker, error) { return nil, nil }
	m.killCLI = func(int) error { return nil }
	return m
}

// waitState polls a session's state until it matches or the deadline passes.
func waitState(t *testing.T, m *manager, handle string, want sessState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c := m.get(handle); c != nil {
			if st, _, _, _ := c.snapshot(); st == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	c := m.get(handle)
	got := sessState("<nil>")
	if c != nil {
		got, _, _, _ = c.snapshot()
	}
	t.Fatalf("session %s: state = %q, want %q", handle, got, want)
}

// --- tests ---

func TestReadOnlyOpensWithoutTap(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)

	res, err := m.Open(context.Background(), openReq{purpose: "read the covenant", writable: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != string(stateOpen) {
		t.Fatalf("read-only should open immediately; state=%q", res.State)
	}
	if gt.next != 0 {
		t.Fatalf("read-only must NOT enqueue a tap gate; gate calls=%d", gt.next)
	}
	if len(op.opened) != 1 {
		t.Fatalf("read-only should open one session; opened=%d", len(op.opened))
	}
}

func TestWritableWaitsForApproveThenOpens(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)

	res, err := m.Open(context.Background(), openReq{purpose: "build the thing", writable: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != string(stateAwaiting) {
		t.Fatalf("writable should await approval; state=%q", res.State)
	}
	if res.HingeID == 0 {
		t.Fatal("writable open should return a gate hinge id")
	}
	// A send before approval does not deliver.
	sr, _ := m.Send(context.Background(), res.Handle, "go")
	if sr.State != string(stateAwaiting) {
		t.Fatalf("send before approval should report awaiting; got %q", sr.State)
	}
	if len(op.opened) != 0 {
		t.Fatalf("no session should open before approval; opened=%d", len(op.opened))
	}
	// Approve → the poller opens the seat.
	gt.set(res.HingeID, "applied")
	waitState(t, m, res.Handle, stateOpen)
	if len(op.opened) != 1 {
		t.Fatalf("approval should open exactly one session; opened=%d", len(op.opened))
	}
	// Now a send delivers.
	sr, _ = m.Send(context.Background(), res.Handle, "go build")
	if sr.Reply == "" {
		t.Fatalf("send after approval should deliver; got %+v", sr)
	}
}

func TestWritableDeclineRefusesOpen(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)

	res, _ := m.Open(context.Background(), openReq{purpose: "push to prod", writable: true})
	gt.set(res.HingeID, "declined")
	waitState(t, m, res.Handle, stateDeclined)
	if len(op.opened) != 0 {
		t.Fatalf("a declined tap must NOT open a session; opened=%d", len(op.opened))
	}
	sr, _ := m.Send(context.Background(), res.Handle, "go")
	if sr.State != string(stateDeclined) {
		t.Fatalf("send on a declined commission should report declined; got %q", sr.State)
	}
}

func TestApprovalTimeoutDeclines(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)
	m.pollTimeout = 20 * time.Millisecond // never approved → times out

	res, _ := m.Open(context.Background(), openReq{purpose: "slow tap", writable: true})
	waitState(t, m, res.Handle, stateDeclined)
	if !gt.wasWithdrawn(res.HingeID) {
		t.Fatal("a timed-out approval should withdraw the gate row")
	}
	if len(op.opened) != 0 {
		t.Fatalf("timeout must not open; opened=%d", len(op.opened))
	}
}

func TestConcurrencyCapRefuses(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)
	m.maxSess = 5

	// 5 read-only sessions fill the cap.
	for i := 0; i < 5; i++ {
		if _, err := m.Open(context.Background(), openReq{purpose: "advise", writable: false}); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	// The 6th is refused.
	if _, err := m.Open(context.Background(), openReq{purpose: "one too many", writable: false}); err == nil {
		t.Fatal("6th open over a cap of 5 should be refused")
	}
	// Closing one frees a slot.
	lst := m.List()
	if lst.Active != 5 {
		t.Fatalf("active=%d, want 5", lst.Active)
	}
	if _, err := m.Close(context.Background(), lst.Sessions[0].Handle, "make room"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(context.Background(), openReq{purpose: "now there is room", writable: false}); err != nil {
		t.Fatalf("open after freeing a slot should succeed: %v", err)
	}
}

func TestAwaitingCountsTowardCap(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)
	m.maxSess = 2

	a, _ := m.Open(context.Background(), openReq{purpose: "w1", writable: true}) // awaiting
	_, _ = m.Open(context.Background(), openReq{purpose: "w2", writable: true})  // awaiting
	if _, err := m.Open(context.Background(), openReq{purpose: "w3", writable: true}); err == nil {
		t.Fatal("awaiting sessions should count toward the cap")
	}
	// Withdrawing one (close) frees the slot even though it never opened.
	if _, err := m.Close(context.Background(), a.Handle, "withdraw"); err != nil {
		t.Fatal(err)
	}
	if !gt.wasWithdrawn(a.HingeID) {
		t.Fatal("closing a pending commission should withdraw its gate row")
	}
	if _, err := m.Open(context.Background(), openReq{purpose: "w3 again", writable: true}); err != nil {
		t.Fatalf("open after withdrawing a pending one should succeed: %v", err)
	}
}

func TestCloseEStopsLiveSeat(t *testing.T) {
	op := &fakeOpener{}
	gt := newFakeGate()
	m := newTestManager(op, gt)

	res, _ := m.Open(context.Background(), openReq{purpose: "advise", writable: false})
	c := m.get(res.Handle)
	_, _, sess, _ := c.snapshot()
	fs := sess.(*fakeSession)

	cr, err := m.Close(context.Background(), res.Handle, "e-stop")
	if err != nil {
		t.Fatal(err)
	}
	if !cr.Killed {
		t.Fatal("closing an open session should report it killed")
	}
	if !fs.isClosed() {
		t.Fatal("close must actually Close() the underlying session (the kill)")
	}
	// It leaves the active count.
	if lst := m.List(); lst.Active != 0 {
		t.Fatalf("after e-stop active=%d, want 0", lst.Active)
	}
}

func TestOpenFailureIsReportedNotFatal(t *testing.T) {
	op := &fakeOpener{failNext: true}
	gt := newFakeGate()
	m := newTestManager(op, gt)

	res, err := m.Open(context.Background(), openReq{purpose: "advise", writable: false})
	if err != nil {
		t.Fatalf("an open failure should be a tool result, not a hard error: %v", err)
	}
	if res.State != string(stateError) {
		t.Fatalf("failed open should be state=error; got %q", res.State)
	}
}

func TestPurposeRequired(t *testing.T) {
	m := newTestManager(&fakeOpener{}, newFakeGate())
	if _, err := m.Open(context.Background(), openReq{purpose: "  ", writable: false}); err == nil {
		t.Fatal("empty purpose should be refused")
	}
}

// TestOverviewMergesAndDedups proves sessions_overview folds loom-mcp's own
// commissions together with the serve's named residents + warm seats, and SKIPS
// unnamed serve residents (those ARE the commissions, already shown richly).
func TestOverviewMergesAndDedups(t *testing.T) {
	op := &fakeOpener{serveSessions: []loom.SessionOverview{
		{Kind: "resident", Name: "worker-1", Handle: "ws-aaa", Named: true, State: "idle", Tail: "done"},
		{Kind: "warm-seat", Name: "companion", Handle: "sonnet#companion|sticky:companion", Model: "sonnet#companion", State: "warm"},
		{Kind: "resident", Name: "", Handle: "ws-bbb", Named: false, State: "idle"}, // an unnamed resident == a commission
	}}
	m := newTestManager(op, newFakeGate())

	res, err := m.Open(context.Background(), openReq{purpose: "advise on the covenant", writable: false})
	if err != nil {
		t.Fatal(err)
	}

	ov := m.Overview(context.Background())
	if ov.ServeError != "" {
		t.Fatalf("serve was healthy; unexpected serve_error %q", ov.ServeError)
	}
	byKind := map[string]int{}
	var sawUnnamedResident bool
	for _, e := range ov.Sessions {
		byKind[e.Kind]++
		if e.Kind == "resident" && e.Handle == "ws-bbb" {
			sawUnnamedResident = true
		}
	}
	if byKind["commission"] != 1 {
		t.Errorf("want 1 commission, got %d", byKind["commission"])
	}
	if byKind["resident"] != 1 {
		t.Errorf("want 1 (named) resident, got %d", byKind["resident"])
	}
	if byKind["warm-seat"] != 1 {
		t.Errorf("want 1 warm-seat, got %d", byKind["warm-seat"])
	}
	if sawUnnamedResident {
		t.Error("an unnamed serve resident must be skipped (it is a commission, shown richly)")
	}
	// The commission we opened is present with its purpose.
	var found bool
	for _, e := range ov.Sessions {
		if e.Kind == "commission" && e.Handle == res.Handle && e.Purpose == "advise on the covenant" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the opened commission is missing from the overview: %+v", ov.Sessions)
	}
}

// TestOverviewDegradesWhenServeDown proves a serve failure still lists the
// commissions and surfaces serve_error rather than failing the whole call.
func TestOverviewDegradesWhenServeDown(t *testing.T) {
	op := &fakeOpener{serveErr: errors.New("dial refused")}
	m := newTestManager(op, newFakeGate())
	if _, err := m.Open(context.Background(), openReq{purpose: "still here", writable: false}); err != nil {
		t.Fatal(err)
	}
	ov := m.Overview(context.Background())
	if ov.ServeError == "" {
		t.Fatal("a serve failure should set serve_error")
	}
	if len(ov.Sessions) != 1 || ov.Sessions[0].Kind != "commission" {
		t.Fatalf("commissions should still list when the serve is down; got %+v", ov.Sessions)
	}
}

// TestOverviewTailFromTurns proves a commission's recent replies become its tail.
func TestOverviewTailFromTurns(t *testing.T) {
	m := newTestManager(&fakeOpener{}, newFakeGate())
	res, _ := m.Open(context.Background(), openReq{purpose: "chat", writable: false})
	if _, err := m.Send(context.Background(), res.Handle, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(context.Background(), res.Handle, "second"); err != nil {
		t.Fatal(err)
	}
	ov := m.Overview(context.Background())
	var e *overviewEntry
	for i := range ov.Sessions {
		if ov.Sessions[i].Handle == res.Handle {
			e = &ov.Sessions[i]
		}
	}
	if e == nil {
		t.Fatal("commission missing from overview")
	}
	if e.Tail != "ack: second" {
		t.Errorf("tail = %q, want the last reply 'ack: second'", e.Tail)
	}
	if len(e.Turns) != 2 {
		t.Errorf("want 2 recorded turns, got %d", len(e.Turns))
	}
}

// TestKillRoutesCommissionToClose proves a commission handle is stopped through
// Close (the rich teardown), NOT the serve kill path.
func TestKillRoutesCommissionToClose(t *testing.T) {
	op := &fakeOpener{}
	m := newTestManager(op, newFakeGate())
	res, _ := m.Open(context.Background(), openReq{purpose: "advise", writable: false})
	c := m.get(res.Handle)
	_, _, sess, _ := c.snapshot()
	fs := sess.(*fakeSession)

	kr, err := m.Kill(context.Background(), res.Handle, "e-stop")
	if err != nil {
		t.Fatal(err)
	}
	if kr.Kind != "commission" || !kr.Killed {
		t.Fatalf("commission kill should report kind=commission, killed; got %+v", kr)
	}
	if !fs.isClosed() {
		t.Fatal("commission kill must Close the seat")
	}
	if len(op.killed) != 0 {
		t.Fatalf("a commission must NOT route to the serve kill path; serve kills=%v", op.killed)
	}
}

// TestKillRoutesToServe proves a non-commission target is stopped via the serve,
// reporting the kind the serve applied.
func TestKillRoutesToServe(t *testing.T) {
	op := &fakeOpener{killFound: true, killKind: "warm-seat"}
	m := newTestManager(op, newFakeGate())
	kr, err := m.Kill(context.Background(), "companion", "")
	if err != nil {
		t.Fatal(err)
	}
	if !kr.OK || kr.Kind != "warm-seat" {
		t.Fatalf("serve kill should report ok + the serve's kind; got %+v", kr)
	}
	if len(op.killed) != 1 || op.killed[0] != "companion" {
		t.Fatalf("kill should reach the serve with the target; got %v", op.killed)
	}
}

// TestKillMissReportsNotFound proves a target that matches nothing anywhere is a
// clean OK=false, not an error.
func TestKillMissReportsNotFound(t *testing.T) {
	op := &fakeOpener{killFound: false}
	m := newTestManager(op, newFakeGate())
	kr, err := m.Kill(context.Background(), "ghost", "")
	if err != nil {
		t.Fatalf("a kill miss should not error: %v", err)
	}
	if kr.OK {
		t.Fatal("a miss should report OK=false")
	}
}

// --- CLI-worker merge + kill routing (the direct `loom run` fleet) ---

// TestOverviewIncludesCLIWorkers proves the host's direct loom run workers are folded
// into the overview as cli-worker entries, alongside commissions.
func TestOverviewIncludesCLIWorkers(t *testing.T) {
	m := newTestManager(&fakeOpener{}, newFakeGate())
	m.listCLI = func(context.Context) ([]cliWorker, error) {
		return []cliWorker{
			{PID: 18652, Backend: "claude", Model: "sonnet", Dir: `C:/x/aud/t2-frontend/claude`},
			{PID: 18777, Backend: "claude", Model: "sonnet", Dir: `C:/x/aud/t1-backend/claude`},
		}, nil
	}
	if _, err := m.Open(context.Background(), openReq{purpose: "advise", writable: false}); err != nil {
		t.Fatal(err)
	}
	ov := m.Overview(context.Background())
	if ov.WorkersError != "" {
		t.Fatalf("healthy scan should leave workers_error empty; got %q", ov.WorkersError)
	}
	n, targets := 0, map[string]bool{}
	for _, e := range ov.Sessions {
		if e.Kind == "cli-worker" {
			n++
			targets[e.Handle] = true
			if e.Backend != "claude" || e.Model != "sonnet" {
				t.Errorf("cli-worker backend/model = %q/%q, want claude/sonnet", e.Backend, e.Model)
			}
		}
	}
	if n != 2 {
		t.Fatalf("want 2 cli-workers in the overview, got %d (%+v)", n, ov.Sessions)
	}
	if !targets["pid:18652"] || !targets["pid:18777"] {
		t.Fatalf("cli-worker handles should be pid:<n>; got %v", targets)
	}
	// CLI workers must NOT consume commission slots.
	if ov.Active != 1 {
		t.Errorf("active = %d, want 1 (only the commission counts)", ov.Active)
	}
}

// TestOverviewCLIWorkerScanErrorDegrades proves a failed process scan surfaces
// workers_error but still lists everything else.
func TestOverviewCLIWorkerScanErrorDegrades(t *testing.T) {
	m := newTestManager(&fakeOpener{}, newFakeGate())
	m.listCLI = func(context.Context) ([]cliWorker, error) { return nil, errors.New("powershell blew up") }
	if _, err := m.Open(context.Background(), openReq{purpose: "advise", writable: false}); err != nil {
		t.Fatal(err)
	}
	ov := m.Overview(context.Background())
	if ov.WorkersError == "" {
		t.Fatal("a failed scan should set workers_error")
	}
	if len(ov.Sessions) != 1 || ov.Sessions[0].Kind != "commission" {
		t.Fatalf("the commission should still list when the scan fails; got %+v", ov.Sessions)
	}
}

// TestKillRoutesCLIWorkerByPID proves a pid target force-kills the worker (and reports
// kind=cli-worker), and that the recycled-PID guard is honored: the kill only fires
// for a PID still present in the live scan.
func TestKillRoutesCLIWorkerByPID(t *testing.T) {
	m := newTestManager(&fakeOpener{}, newFakeGate())
	m.listCLI = func(context.Context) ([]cliWorker, error) {
		return []cliWorker{{PID: 4242, Backend: "local"}}, nil
	}
	var killedPID int
	m.killCLI = func(pid int) error { killedPID = pid; return nil }

	// Both "pid:<n>" and a bare integer route to the CLI kill.
	for _, target := range []string{"pid:4242", "4242"} {
		killedPID = 0
		kr, err := m.Kill(context.Background(), target, "")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if !kr.OK || kr.Kind != "cli-worker" || !kr.Killed {
			t.Fatalf("%s: kr = %+v, want ok cli-worker killed", target, kr)
		}
		if killedPID != 4242 {
			t.Fatalf("%s: killCLI got pid %d, want 4242", target, killedPID)
		}
	}
}

// TestKillCLIWorkerRecycledPIDGuard proves a PID not in the live scan is refused —
// never force-killing an innocent recycled PID.
func TestKillCLIWorkerRecycledPIDGuard(t *testing.T) {
	m := newTestManager(&fakeOpener{}, newFakeGate())
	m.listCLI = func(context.Context) ([]cliWorker, error) {
		return []cliWorker{{PID: 4242, Backend: "local"}}, nil // 9999 is NOT here
	}
	killed := false
	m.killCLI = func(int) error { killed = true; return nil }

	kr, err := m.Kill(context.Background(), "pid:9999", "")
	if err != nil {
		t.Fatalf("a guard miss should be a clean result, not an error: %v", err)
	}
	if kr.OK {
		t.Fatal("killing a PID absent from the live scan must report OK=false")
	}
	if killed {
		t.Fatal("the guard must prevent killCLI from firing on a recycled/absent PID")
	}
	if kr.Kind != "cli-worker" {
		t.Errorf("kind = %q, want cli-worker (so the app knows what was attempted)", kr.Kind)
	}
}
