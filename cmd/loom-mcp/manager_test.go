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
