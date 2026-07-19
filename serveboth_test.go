package loom

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// startTestServeBoth binds two ephemeral listeners (plain + pinned mTLS) on ONE shared
// server with the given backend, mirroring startTestServer. The server pins the returned
// client identity; the returned client pins the server under "box". Returns the plain
// ws:// URL, the wss:// URL, the token, and the cross-pinned client identity + pins.
func startTestServeBoth(t *testing.T, sb Backend) (plainURL, tlsURL, token string, cliID *Identity, cliPins *PinStore) {
	t.Helper()
	token = "co-token"
	tokenPath := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts, err := loadTokenStore(tokenPath)
	if err != nil {
		t.Fatal(err)
	}

	srvID := testIdentity(t)
	cliID = testIdentity(t)
	srvPins := testPins(t, map[string]string{"client": cliID.Fingerprint()})
	cliPins = testPins(t, map[string]string{"box": srvID.Fingerprint()})

	cfg, err := TLSServerConfig(srvID, srvPins)
	if err != nil {
		t.Fatal(err)
	}
	plainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsRaw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(tlsRaw, cfg)
	t.Cleanup(func() { _ = plainLn.Close(); _ = tlsRaw.Close() })

	srv := newServer(ts, map[string]Backend{"stub": sb}, 0)
	go func() { _ = srv.serveBoth(plainLn, tlsLn) }()
	return "ws://" + plainLn.Addr().String(), "wss://" + tlsRaw.Addr().String(), token, cliID, cliPins
}

// The migration bridge: a plain token client AND a pinned mTLS client both drive the SAME
// coexistence server. Both get their turn — this is the whole point of running both at once.
func TestServeBothPlainAndTLS(t *testing.T) {
	sb := &stubBackend{}
	plainURL, tlsURL, token, cliID, cliPins := startTestServeBoth(t, sb)

	// 1. plain client over ws with the token.
	pb := ConnectBackend{URL: plainURL, Token: token, Agent: "stub"}
	ps, err := pb.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("plain open: %v", err)
	}
	defer ps.Close()
	if r, err := ps.Send(context.Background(), "hi"); err != nil || r.Text != "echo:hi" {
		t.Fatalf("plain send: err=%v text=%q", err, r.Text)
	}

	// 2. pinned client over wss (coexistence requires the token TOO — belt and suspenders).
	wb := ConnectBackend{URL: tlsURL, Token: token, Agent: "stub", Identity: cliID, Pins: cliPins, Peer: "box"}
	ws, err := wb.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("wss open: %v", err)
	}
	defer ws.Close()
	if r, err := ws.Send(context.Background(), "yo"); err != nil || r.Text != "echo:yo" {
		t.Fatalf("wss send: err=%v text=%q", err, r.Text)
	}
}

// Coexistence keeps the token wall on BOTH listeners: a pinned client with a BAD token is
// still refused (the pin is added on top of the token here, never in place of it).
func TestServeBothTLSStillNeedsToken(t *testing.T) {
	sb := &stubBackend{}
	_, tlsURL, _, cliID, cliPins := startTestServeBoth(t, sb)
	wb := ConnectBackend{URL: tlsURL, Token: "wrong", Agent: "stub", Identity: cliID, Pins: cliPins, Peer: "box"}
	if _, err := wb.Open(context.Background(), SessionOpts{}); err == nil {
		t.Fatal("wss client with a bad token was admitted — coexistence must keep the token wall on the tls listener")
	}
}

// An UNPINNED client is refused at the TLS layer of the coexistence tls listener (the pin
// is real, not decorative — a stranger cert never reaches the token check).
func TestServeBothUnpinnedTLSRefused(t *testing.T) {
	sb := &stubBackend{}
	_, tlsURL, token, _, cliPins := startTestServeBoth(t, sb)
	stranger := testIdentity(t) // the server never pinned this identity
	wb := ConnectBackend{URL: tlsURL, Token: token, Agent: "stub", Identity: stranger, Pins: cliPins, Peer: "box"}
	if _, err := wb.Open(context.Background(), SessionOpts{}); err == nil {
		t.Fatal("an unpinned client was admitted over the coexistence tls listener")
	}
}

// The two listeners share ONE resident set: a named resident opened over plain reattaches
// over wss without a second spawn (open-count stays 1). This is what lets a client migrate
// from token to pin without losing its warm session.
func TestServeBothSharedResidents(t *testing.T) {
	sb := &stubBackend{}
	plainURL, tlsURL, token, cliID, cliPins := startTestServeBoth(t, sb)

	pb := ConnectBackend{URL: plainURL, Token: token, Agent: "stub", SessionName: "warm"}
	ps, err := pb.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("plain open: %v", err)
	}
	defer ps.Close()
	if n := sb.openCount(); n != 1 {
		t.Fatalf("first open: open-count = %d, want 1", n)
	}

	wb := ConnectBackend{URL: tlsURL, Token: token, Agent: "stub", SessionName: "warm", Identity: cliID, Pins: cliPins, Peer: "box"}
	ws, err := wb.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("wss reattach: %v", err)
	}
	defer ws.Close()
	if n := sb.openCount(); n != 1 {
		t.Fatalf("wss reattach spawned a SECOND process (open-count %d), want 1 — the listeners are not sharing one server", n)
	}
}
