package loom

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubBackend is a hermetic Backend: no process, no network to a model, no money. It
// counts Open calls (the oracle for "reattach did NOT spawn"), records the opts of the
// most recent Open (to prove a post-downgrade reopen cold-resumes the remembered
// lineage), and holds every session it minted (so a test can inspect the interrupt flag
// or close a turn's gate).
type stubBackend struct {
	mu       sync.Mutex
	opens    int
	lastOpts SessionOpts
	made     []*stubSession
	gate     chan struct{} // if non-nil, injected into each session so its turn blocks until closed
}

func (b *stubBackend) Name() string { return "stub" }

func (b *stubBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	b.lastOpts = opts
	s := &stubSession{id: "stub-1", gate: b.gate, intCh: make(chan struct{}), started: make(chan struct{})}
	b.made = append(b.made, s)
	return s, nil
}

func (b *stubBackend) openCount() int     { b.mu.Lock(); defer b.mu.Unlock(); return b.opens }
func (b *stubBackend) resumeSeen() string { b.mu.Lock(); defer b.mu.Unlock(); return b.lastOpts.Resume }
func (b *stubBackend) session(i int) *stubSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= len(b.made) {
		return nil
	}
	return b.made[i]
}

// stubSession echoes the prompt and emits one tool-call event before the reply — the
// minimum to exercise the full event + reply codec. A non-nil gate makes SendStream
// block until the gate is closed OR the turn is interrupted, so a test can hold a turn
// "in flight" (for detach / await) and prove an interrupt reaches it mid-turn. started
// closes when the turn begins; Interrupt + Close set atomic flags a test can assert on.
type stubSession struct {
	id          string
	gate        chan struct{}
	intCh       chan struct{} // closed by Interrupt so a gated turn can end
	started     chan struct{} // closed when SendStream begins
	intOnce     sync.Once
	startOnce   sync.Once
	interrupted int32 // atomic
	closed      int32 // atomic
}

func (s *stubSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}
func (s *stubSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	emit(onEvent, Event{Kind: EvToolCall, Backend: "stub", Tool: "echo"})
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.gate != nil {
		select {
		case <-s.gate: // released by the test
		case <-s.intCh: // interrupted mid-turn
		}
	}
	return Reply{Backend: "stub", Text: "echo:" + prompt, SessionID: s.id}, nil
}
func (s *stubSession) SessionID() string { return s.id }
func (s *stubSession) Close() error      { atomic.StoreInt32(&s.closed, 1); return nil }
func (s *stubSession) isClosed() bool    { return atomic.LoadInt32(&s.closed) == 1 }
func (s *stubSession) Interrupt() error {
	atomic.StoreInt32(&s.interrupted, 1)
	if s.intCh != nil {
		s.intOnce.Do(func() { close(s.intCh) })
	}
	return nil
}

// startTestServer binds 127.0.0.1:0 and serves the given backends with a token file
// holding one known token. It returns the *server (so a test can drive the reaper
// deterministically), the ws:// URL, and the token.
func startTestServer(t *testing.T, backends map[string]Backend, idleTTL time.Duration) (*server, string, string) {
	t.Helper()
	token := "secret-token"
	tokenPath := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenPath, []byte("# a comment\n\n"+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts, err := loadTokenStore(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if ts.count() != 1 {
		t.Fatalf("token store loaded %d tokens, want 1 (comments/blanks should be ignored)", ts.count())
	}
	srv := newServer(ts, backends, idleTTL)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.serve(ln) }()
	return srv, "ws://" + ln.Addr().String(), token
}

// TestServeRoundTrip drives real ConnectBackend client code against a real server over
// the hand-rolled websocket codec: the whole handshake + frame protocol.
func TestServeRoundTrip(t *testing.T) {
	sb := &stubBackend{}
	backends := map[string]Backend{"stub": sb}
	_, url, token := startTestServer(t, backends, 0)

	// A bad token is rejected at the handshake.
	if _, err := (ConnectBackend{URL: url, Token: "wrong", Agent: "stub"}).Open(context.Background(), SessionOpts{}); err == nil {
		t.Fatal("expected a bad token to be rejected")
	}

	// A good token opens a session.
	b := ConnectBackend{URL: url, Token: token, Agent: "stub"}
	sess, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	// SendStream: the event arrives, then the echo reply.
	var gotEvent bool
	r, err := sess.SendStream(context.Background(), "hi", func(ev Event) {
		if ev.Kind == EvToolCall && ev.Tool == "echo" {
			gotEvent = true
		}
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if r.Text != "echo:hi" {
		t.Errorf("reply text = %q, want %q", r.Text, "echo:hi")
	}
	if r.SessionID != "stub-1" {
		t.Errorf("reply session_id = %q, want stub-1", r.SessionID)
	}
	if !gotEvent {
		t.Error("expected the tool_call event to arrive over the wire")
	}

	// SessionID reports the backend's resumable id learned from the reply, not the
	// server handle.
	if got := sess.SessionID(); got != "stub-1" {
		t.Errorf("SessionID() = %q, want the reply's stub-1", got)
	}

	// Interrupt reaches the server-held session. It is fire-and-forget (no synchronous
	// ack to the client), so poll the flag with a timeout.
	it, ok := sess.(Interruptible)
	if !ok {
		t.Fatal("connectSession must implement Interruptible")
	}
	if err := it.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	stub := sb.session(0)
	if stub == nil {
		t.Fatal("stub backend never minted a session")
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&stub.interrupted) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("interrupt did not reach the stub session")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestServeUnknownBackend proves the server rejects an open for a backend it does not
// host (the client surfaces it as an error, not a panic).
func TestServeUnknownBackend(t *testing.T) {
	_, url, token := startTestServer(t, map[string]Backend{"stub": &stubBackend{}}, 0)
	_, err := (ConnectBackend{URL: url, Token: token, Agent: "nope"}).Open(context.Background(), SessionOpts{})
	if err == nil {
		t.Fatal("expected an unknown backend to be rejected")
	}
}

// TestInterruptDuringTurn proves the server reader stays free while a (non-detached)
// turn runs: an interrupt sent on the SAME socket reaches the in-flight session and
// ends the turn. If send were dispatched inline (blocking the frame reader for the
// whole turn), the interrupt would never be read and this would hang.
func TestInterruptDuringTurn(t *testing.T) {
	gate := make(chan struct{}) // never closed here — only an interrupt can end the turn
	sb := &stubBackend{gate: gate}
	_, url, token := startTestServer(t, map[string]Backend{"stub": sb}, 0)
	b := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "mid"}
	sess, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	done := make(chan Reply, 1)
	go func() {
		r, _ := sess.Send(context.Background(), "work")
		done <- r
	}()

	// Wait until the turn is actually in flight server-side.
	var st *stubSession
	deadline := time.Now().Add(2 * time.Second)
	for st == nil {
		if st = sb.session(0); st == nil {
			if time.Now().After(deadline) {
				t.Fatal("turn never reached the stub session")
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	select {
	case <-st.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never started on the session")
	}

	// Interrupt on the same socket — the server must read it while the turn blocks.
	it, ok := sess.(Interruptible)
	if !ok {
		t.Fatal("connectSession must implement Interruptible")
	}
	if err := it.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not reach the in-flight turn — the server reader was blocked (send must run in a goroutine)")
	}
	if atomic.LoadInt32(&st.interrupted) == 0 {
		t.Fatal("interrupt flag not set on the session")
	}
}

// TestReattachByName proves the warm-resident core + the two-writer fence: a second
// open of the same NAME reattaches to the live process instead of spawning a second one
// (the stub's open-count is the oracle — it must stay 1).
func TestReattachByName(t *testing.T) {
	sb := &stubBackend{}
	_, url, token := startTestServer(t, map[string]Backend{"stub": sb}, 0)
	b := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "loop"}

	s1, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer s1.Close()
	if n := sb.openCount(); n != 1 {
		t.Fatalf("first open: stub open-count = %d, want 1", n)
	}

	s2, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("second open (reattach): %v", err)
	}
	defer s2.Close()
	if n := sb.openCount(); n != 1 {
		t.Fatalf("reattach spawned a SECOND process: stub open-count = %d, want 1 (two writers on one lineage = silent divergence)", n)
	}

	// The reattached session drives the SAME resident.
	r, err := s2.Send(context.Background(), "ping")
	if err != nil {
		t.Fatalf("send on reattached session: %v", err)
	}
	if r.Text != "echo:ping" {
		t.Errorf("reattached send reply = %q, want echo:ping", r.Text)
	}
}

// TestReplyRingDetachAwait proves detach + the per-turn reply ring: a detached turn
// returns a turn-id immediately, await reports it "running" while the turn is in flight,
// then returns the buffered reply once done — AND a fresh connection (socket dropped,
// reattached by name) can still fetch that reply by turn-id from the ring.
func TestReplyRingDetachAwait(t *testing.T) {
	gate := make(chan struct{})
	sb := &stubBackend{gate: gate}
	_, url, token := startTestServer(t, map[string]Backend{"stub": sb}, 0)
	b := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "verify"}

	sess, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ds, ok := sess.(DetachSession)
	if !ok {
		t.Fatal("connectSession must implement DetachSession")
	}

	turnID, err := ds.SendDetached(context.Background(), "build")
	if err != nil {
		t.Fatalf("send --detach: %v", err)
	}
	if turnID <= 0 {
		t.Fatalf("detach returned turn-id %d, want > 0", turnID)
	}

	// The turn is blocked on the gate → await should report it still running.
	if _, running, err := ds.Await(context.Background(), turnID, false, 150*time.Millisecond); err != nil {
		t.Fatalf("await (running): %v", err)
	} else if !running {
		t.Fatal("await should report the turn still running while its gate is closed")
	}

	close(gate) // release the turn

	r, running, err := ds.Await(context.Background(), turnID, false, 2*time.Second)
	if err != nil {
		t.Fatalf("await (done): %v", err)
	}
	if running {
		t.Fatal("await should return the reply once the turn completed")
	}
	if r.Text != "echo:build" {
		t.Errorf("awaited reply = %q, want echo:build", r.Text)
	}

	// Drop the socket, reattach by name, and re-await the SAME turn-id: the ring still
	// holds the verdict (no lost result, no duplicate work).
	_ = sess.Close()
	sess2, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	defer sess2.Close()
	if n := sb.openCount(); n != 1 {
		t.Fatalf("reattach spawned a new process (open-count %d), want 1", n)
	}
	ds2 := sess2.(DetachSession)
	r2, running2, err := ds2.Await(context.Background(), turnID, false, 2*time.Second)
	if err != nil {
		t.Fatalf("await after reattach: %v", err)
	}
	if running2 || r2.Text != "echo:build" {
		t.Errorf("await after reattach = %q (running=%v), want echo:build", r2.Text, running2)
	}
}

// TestSessionsList proves `loom sessions` reports a resident with its name + last
// turn-id (needed for reattach UX and the janitor).
func TestSessionsList(t *testing.T) {
	sb := &stubBackend{}
	_, url, token := startTestServer(t, map[string]Backend{"stub": sb}, 0)
	b := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "alpha"}
	sess, err := b.Open(context.Background(), SessionOpts{Model: "claude-x"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	if _, err := sess.Send(context.Background(), "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}

	infos, err := (ConnectBackend{URL: url, Token: token}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	var got *SessionInfo
	for i := range infos {
		if infos[i].Name == "alpha" {
			got = &infos[i]
		}
	}
	if got == nil {
		t.Fatalf("sessions did not list the resident %q (got %+v)", "alpha", infos)
	}
	if got.Backend != "stub" {
		t.Errorf("listed backend = %q, want stub", got.Backend)
	}
	if got.Model != "claude-x" {
		t.Errorf("listed model = %q, want claude-x (frozen opts)", got.Model)
	}
	if got.LastTurnID != 1 {
		t.Errorf("listed last_turn_id = %d, want 1 (one turn ran)", got.LastTurnID)
	}
}

// TestIdleTTLDowngrade proves the janitor: a named resident idle past the TTL is
// downgraded (its live process closed), its lineage id remembered, and the next open of
// the name cold-opens (open-count increments) resuming that remembered id — one
// cold-read, never lost data. The reaper is driven with a far-future "now" so the test
// is deterministic (no sleeping on wall-clock).
func TestIdleTTLDowngrade(t *testing.T) {
	sb := &stubBackend{}
	srv, url, token := startTestServer(t, map[string]Backend{"stub": sb}, time.Hour)
	b := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "ttl"}

	sess, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if n := sb.openCount(); n != 1 {
		t.Fatalf("first open: open-count = %d, want 1", n)
	}
	_ = sess.Close() // named → the resident stays warm (keep_alive)

	// Force the janitor: pretend it is two hours later (well past the 1h TTL).
	srv.reapOnce(time.Now().Add(2 * time.Hour))

	first := sb.session(0)
	if first == nil || !first.isClosed() {
		t.Fatal("idle downgrade should have CLOSED the resident's live process")
	}

	// Reopen by name: no live resident remains, so it cold-opens (open-count → 2),
	// resuming the remembered lineage id.
	sess2, err := b.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("reopen after downgrade: %v", err)
	}
	defer sess2.Close()
	if n := sb.openCount(); n != 2 {
		t.Fatalf("reopen after downgrade: open-count = %d, want 2 (a fresh cold-open)", n)
	}
	if got := sb.resumeSeen(); got != "stub-1" {
		t.Fatalf("reopen should resume the remembered lineage id, got Resume=%q, want stub-1", got)
	}
}
