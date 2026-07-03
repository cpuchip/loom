package loom

import (
	"crypto/tls"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// testPins builds a pin store in a temp file with the given name->fingerprint entries.
func testPins(t *testing.T, entries map[string]string) *PinStore {
	t.Helper()
	ps, err := LoadPinStore(filepath.Join(t.TempDir(), "pins"))
	if err != nil {
		t.Fatalf("LoadPinStore: %v", err)
	}
	for name, fp := range entries {
		if err := ps.Add(name, fp); err != nil {
			t.Fatalf("Add(%s): %v", name, err)
		}
	}
	return ps
}

// flipFP flips one hex nibble of a fingerprint, yielding a valid-shaped but WRONG pin.
func flipFP(fp string) string {
	b := []byte(fp)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

// Happy path: cross-pinned peers complete a TLS 1.3 handshake and round-trip data, and
// the server can name which pinned peer connected. This is the transport oracle.
func TestPinnedMTLSRoundTrip(t *testing.T) {
	idClient := testIdentity(t)
	idServer := testIdentity(t)
	serverPins := testPins(t, map[string]string{"alice": idClient.Fingerprint()})
	clientPins := testPins(t, map[string]string{"bob": idServer.Fingerprint()})

	serverCfg, err := TLSServerConfig(idServer, serverPins)
	if err != nil {
		t.Fatalf("TLSServerConfig: %v", err)
	}
	clientCfg, err := TLSClientConfig(idClient, clientPins, "bob")
	if err != nil {
		t.Fatalf("TLSClientConfig: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	tln := tls.NewListener(ln, serverCfg)

	type srvOut struct {
		peer string
		got  string
		err  error
	}
	srvCh := make(chan srvOut, 1)
	go func() {
		c, err := tln.Accept()
		if err != nil {
			srvCh <- srvOut{err: err}
			return
		}
		defer c.Close()
		tc := c.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			srvCh <- srvOut{err: err}
			return
		}
		name, _ := PeerNameFromState(tc.ConnectionState(), serverPins)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(tc, buf); err != nil {
			srvCh <- srvOut{err: err}
			return
		}
		_, _ = tc.Write([]byte("pong"))
		srvCh <- srvOut{peer: name, got: string(buf)}
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("client dial/handshake failed: %v", err)
	}
	defer conn.Close()
	if conn.ConnectionState().Version != tls.VersionTLS13 {
		t.Fatalf("not TLS 1.3: %x", conn.ConnectionState().Version)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(reply) != "pong" {
		t.Fatalf("round-trip garbled: %q", reply)
	}

	select {
	case out := <-srvCh:
		if out.err != nil {
			t.Fatalf("server side: %v", out.err)
		}
		if out.got != "ping" {
			t.Fatalf("server read %q, want ping", out.got)
		}
		if out.peer != "alice" {
			t.Fatalf("server named peer %q, want alice", out.peer)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server timed out")
	}
}

// INVERSE HYPOTHESIS: flip one byte of the pinned server fingerprint and the client must
// REFUSE the handshake. This is the proof the pin is load-bearing — with the correct pin
// (the happy path above) the handshake succeeds; with one byte wrong it fails.
func TestPinnedMTLSWrongPinFails(t *testing.T) {
	idClient := testIdentity(t)
	idServer := testIdentity(t)
	serverPins := testPins(t, map[string]string{"alice": idClient.Fingerprint()})
	clientPins := testPins(t, map[string]string{"bob": flipFP(idServer.Fingerprint())})

	serverCfg, err := TLSServerConfig(idServer, serverPins)
	if err != nil {
		t.Fatalf("server cfg: %v", err)
	}
	clientCfg, err := TLSClientConfig(idClient, clientPins, "bob")
	if err != nil {
		t.Fatalf("client cfg: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	tln := tls.NewListener(ln, serverCfg)
	go func() {
		if c, err := tln.Accept(); err == nil {
			_ = c.(*tls.Conn).Handshake() // will fail; ignore
			_ = c.Close()
		}
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err == nil {
		conn.Close()
		t.Fatalf("handshake SUCCEEDED with a wrong pin — the pin is not enforced")
	}
}

// The server rejects a client whose cert is not pinned at all (fail-closed). Note the
// TLS 1.3 timing: the server verifies the CLIENT cert only after the client has finished,
// so tls.Dial may return before the rejection — the refusal surfaces as a failed
// round-trip (the server aborts the handshake and closes). We assert no data flows.
func TestPinnedMTLSUnpinnedClientFails(t *testing.T) {
	idClient := testIdentity(t)
	idServer := testIdentity(t)
	serverPins := testPins(t, map[string]string{}) // trusts NO one
	clientPins := testPins(t, map[string]string{"bob": idServer.Fingerprint()})

	serverCfg, _ := TLSServerConfig(idServer, serverPins)
	clientCfg, _ := TLSClientConfig(idClient, clientPins, "bob")

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	tln := tls.NewListener(ln, serverCfg)
	go func() {
		// The server would echo IF it (wrongly) accepted — so a wrongful accept is caught.
		if c, err := tln.Accept(); err == nil {
			tc := c.(*tls.Conn)
			if tc.Handshake() == nil {
				buf := make([]byte, 4)
				if _, err := io.ReadFull(tc, buf); err == nil {
					_, _ = tc.Write([]byte("pong"))
				}
			}
			_ = c.Close()
		}
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		return // rejected at handshake — fail-closed, good
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, werr := conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	_, rerr := io.ReadFull(conn, buf)
	if werr == nil && rerr == nil {
		t.Fatalf("unpinned client completed a full round-trip — not fail-closed")
	}
}

// Config builders reject nonsense inputs (fail-closed at construction, not at handshake).
func TestTLSConfigGuards(t *testing.T) {
	id := testIdentity(t)
	pins := testPins(t, map[string]string{})
	if _, err := TLSClientConfig(id, pins, "nobody"); err == nil {
		t.Fatalf("expected error for unpinned peer name")
	}
	if _, err := TLSClientConfig(id, pins, ""); err == nil {
		t.Fatalf("expected error for empty peer name")
	}
	if _, err := TLSServerConfig(nil, pins); err == nil {
		t.Fatalf("expected error for nil identity")
	}
	if _, err := TLSServerConfig(id, nil); err == nil {
		t.Fatalf("expected error for nil pins")
	}
}
