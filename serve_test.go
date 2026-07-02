package loom

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// stubBackend is a hermetic Backend: no process, no network to a model, no money.
// It returns one shared stubSession so a test can inspect its interrupt flag.
type stubBackend struct{ sess *stubSession }

func (b *stubBackend) Name() string { return "stub" }
func (b *stubBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	return b.sess, nil
}

// stubSession echoes the prompt and emits one tool-call event before the reply — the
// minimum to exercise the full event + reply codec. Interrupt sets a flag so a test
// can prove the interrupt frame reached the server-held session.
type stubSession struct {
	interrupted int32 // atomic
}

func (s *stubSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}
func (s *stubSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	emit(onEvent, Event{Kind: EvToolCall, Backend: "stub", Tool: "echo"})
	return Reply{Backend: "stub", Text: "echo:" + prompt, SessionID: "stub-1"}, nil
}
func (s *stubSession) SessionID() string { return "stub-1" }
func (s *stubSession) Close() error      { return nil }
func (s *stubSession) Interrupt() error {
	atomic.StoreInt32(&s.interrupted, 1)
	return nil
}

// startTestServer binds 127.0.0.1:0 and serves the given backends with a token file
// holding one known token. It returns the ws:// URL and the token.
func startTestServer(t *testing.T, backends map[string]Backend) (url, token string) {
	t.Helper()
	token = "secret-token"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = serveOn(ln, ts, backends) }()
	return "ws://" + ln.Addr().String(), token
}

// TestServeRoundTrip drives real ConnectBackend client code against a real serveOn
// server over the hand-rolled websocket codec: the whole handshake + frame protocol.
func TestServeRoundTrip(t *testing.T) {
	stub := &stubSession{}
	backends := map[string]Backend{"stub": &stubBackend{sess: stub}}
	url, token := startTestServer(t, backends)

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
	url, token := startTestServer(t, map[string]Backend{"stub": &stubBackend{sess: &stubSession{}}})
	_, err := (ConnectBackend{URL: url, Token: token, Agent: "nope"}).Open(context.Background(), SessionOpts{})
	if err == nil {
		t.Fatal("expected an unknown backend to be rejected")
	}
}

// TestWSAcceptKey locks the RFC 6455 accept computation against the spec's own
// worked example (the canonical key → accept pair).
func TestWSAcceptKey(t *testing.T) {
	// From RFC 6455 §1.3.
	if got := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("wsAcceptKey = %q, want s3pPLMBiTxaQ9kYGzzhZRbK+xOo=", got)
	}
}

// TestTokenStore locks the file parsing (comments/blanks ignored) and constant-time
// verify.
func TestTokenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("# header\n\nalpha\nbravo\n# trailing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts, err := loadTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if ts.count() != 2 {
		t.Fatalf("count = %d, want 2", ts.count())
	}
	if !ts.verify("alpha") || !ts.verify("bravo") {
		t.Error("valid tokens should verify")
	}
	if ts.verify("charlie") || ts.verify("") {
		t.Error("unknown / empty tokens must not verify")
	}

	// AddToken appends a working token to the same file.
	tok, err := AddToken(path)
	if err != nil {
		t.Fatal(err)
	}
	ts2, err := loadTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ts2.verify(tok) || ts2.count() != 3 {
		t.Errorf("AddToken did not persist a usable token (count=%d)", ts2.count())
	}
}
