package loom

import (
	"bytes"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newEnrollTestServer stands up an httptest server whose /enroll route drives a real
// EnrollServer (fresh identity, given code + pin name). Returns the server, the host:port
// addr (scheme-stripped, the form EnrollConnect wants), and the server's identity + pins.
func newEnrollTestServer(t *testing.T, code, pinName string) (*httptest.Server, string, *Identity, *PinStore) {
	t.Helper()
	srvID := testIdentity(t)
	srvPins := testPins(t, nil)
	es := &EnrollServer{Identity: srvID, Pins: srvPins, Code: code, PinName: pinName}
	mux := http.NewServeMux()
	mux.HandleFunc(enrollPath, func(w http.ResponseWriter, r *http.Request) { _, _, _ = es.handleEnroll(w, r) })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, strings.TrimPrefix(ts.URL, "http://"), srvID, srvPins
}

// assertMTLSHandshakeOK proves the two identities + pins actually complete a pinned-mTLS
// handshake — the end-to-end oracle that ties enrollment to the transport it exists for.
func assertMTLSHandshakeOK(t *testing.T, srvID *Identity, srvPins *PinStore, cliID *Identity, cliPins *PinStore, peer string) {
	t.Helper()
	scfg, err := TLSServerConfig(srvID, srvPins)
	if err != nil {
		t.Fatalf("server cfg: %v", err)
	}
	ccfg, err := TLSClientConfig(cliID, cliPins, peer)
	if err != nil {
		t.Fatalf("client cfg: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tln := tls.NewListener(ln, scfg)
	errc := make(chan error, 1)
	go func() {
		c, err := tln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer c.Close()
		errc <- c.(*tls.Conn).Handshake()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), ccfg)
	if err != nil {
		t.Fatalf("client handshake failed after enroll: %v", err)
	}
	defer conn.Close()
	if err := <-errc; err != nil {
		t.Fatalf("server handshake failed after enroll: %v", err)
	}
}

// The happy path: a client enrolls with the right code, both sides end up mutually pinned
// (server pins the client under the operator name; client pins the server under its chosen
// name), AND those pins actually enable a pinned-mTLS handshake. This is the enrollment oracle.
func TestEnrollRoundTrip(t *testing.T) {
	code, err := NewEnrollCode()
	if err != nil {
		t.Fatal(err)
	}
	_, addr, srvID, srvPins := newEnrollTestServer(t, code, "phone")

	cliID := testIdentity(t)
	cliPins := testPins(t, nil)
	res, err := EnrollConnect(addr, code, "mybox", "michael-pixel", cliID, cliPins)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	if res.ServerFingerprint != srvID.Fingerprint() {
		t.Fatalf("result fp %s, want %s", res.ServerFingerprint, srvID.Fingerprint())
	}
	if got, ok := cliPins.Get("mybox"); !ok || got != srvID.Fingerprint() {
		t.Fatalf("client pin mybox=%q ok=%v, want %s", got, ok, srvID.Fingerprint())
	}
	if name, ok := srvPins.NameFor(cliID.Fingerprint()); !ok || name != "phone" {
		t.Fatalf("server pin for client = %q ok=%v, want phone", name, ok)
	}

	assertMTLSHandshakeOK(t, srvID, srvPins, cliID, cliPins, "mybox")
}

// INVERSE HYPOTHESIS: with the WRONG code the server refuses (the MAC will not verify), and
// NOTHING is pinned on either side. This is the proof the code is load-bearing.
func TestEnrollWrongCodeRefused(t *testing.T) {
	code, _ := NewEnrollCode()
	_, addr, _, srvPins := newEnrollTestServer(t, code, "phone")

	cliID := testIdentity(t)
	cliPins := testPins(t, nil)
	wrong, _ := NewEnrollCode()
	if _, err := EnrollConnect(addr, wrong, "mybox", "", cliID, cliPins); err == nil {
		t.Fatal("enroll with the WRONG code SUCCEEDED — the code is not enforced")
	}
	if _, ok := cliPins.Get("mybox"); ok {
		t.Fatal("client pinned the server despite a refused enrollment")
	}
	if _, ok := srvPins.NameFor(cliID.Fingerprint()); ok {
		t.Fatal("server pinned the client despite a bad code")
	}
}

// A rogue server that cannot prove the code (returns a bad server MAC) is REJECTED by the
// client — the check that stops an impostor box from being pinned. Nothing is pinned.
func TestEnrollServerMACForged(t *testing.T) {
	code, _ := NewEnrollCode()
	rogueID := testIdentity(t)
	mux := http.NewServeMux()
	mux.HandleFunc(enrollPath, func(w http.ResponseWriter, r *http.Request) {
		// Return a well-formed response but with a MAC the client cannot verify against the
		// code — exactly what a MITM/impostor who does not hold the code could produce.
		writeJSON(w, http.StatusOK, enrollResponse{Cert: rogueID.CertDER(), MAC: []byte("not-a-valid-mac"), Name: "x"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	cliID := testIdentity(t)
	cliPins := testPins(t, nil)
	if _, err := EnrollConnect(addr, code, "mybox", "", cliID, cliPins); err == nil {
		t.Fatal("client accepted a forged server MAC — impersonation not caught")
	}
	if _, ok := cliPins.Get("mybox"); ok {
		t.Fatal("client pinned a rogue server despite a bad server MAC")
	}
}

// The code is normalized (case + grouping) before it keys the MAC, so a human typing
// "k7q2-m9xa" matches "K7Q2M9XA"; and the two MAC domains never collide.
func TestEnrollCodeNormalizeAndDomains(t *testing.T) {
	der := []byte("some-cert-der-bytes")
	a := enrollMAC("K7Q2M9XA", enrollDomainClient, der)
	b := enrollMAC("k7q2-m9xa", enrollDomainClient, der)
	c := enrollMAC("  K7Q2 M9XA  ", enrollDomainClient, der)
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatal("code normalization is not applied to the MAC key (casing/grouping must not matter)")
	}
	if bytes.Equal(enrollMAC("X", enrollDomainClient, der), enrollMAC("X", enrollDomainServer, der)) {
		t.Fatal("client and server MAC domains collide — a client proof could be replayed as a server proof")
	}
	code, err := NewEnrollCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 8 {
		t.Fatalf("enroll code is %d chars, want 8: %q", len(code), code)
	}
	if GroupEnrollCode(code) != code[:4]+"-"+code[4:] {
		t.Fatalf("grouping malformed: %q", GroupEnrollCode(code))
	}
}

// The real one-shot listener path (ListenAndEnroll): bind, accept one valid enrollment,
// pin the client, and return its name + fingerprint. Uses a grabbed-then-released ephemeral
// port with a readiness poll so there is no rebind race.
func TestListenAndEnroll(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	code, _ := NewEnrollCode()
	srvID := testIdentity(t)
	srvPins := testPins(t, nil)
	es := &EnrollServer{Identity: srvID, Pins: srvPins, Code: code, PinName: "phone"}

	type out struct {
		name, fp string
		err      error
	}
	ch := make(chan out, 1)
	go func() {
		n, f, e := es.ListenAndEnroll(addr, 5*time.Second)
		ch <- out{n, f, e}
	}()
	waitDial(t, addr)

	cliID := testIdentity(t)
	cliPins := testPins(t, nil)
	if _, err := EnrollConnect(addr, code, "mybox", "", cliID, cliPins); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("ListenAndEnroll: %v", o.err)
		}
		if o.name != "phone" || o.fp != cliID.Fingerprint() {
			t.Fatalf("server recorded name=%q fp=%q, want phone / %s", o.name, o.fp, cliID.Fingerprint())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndEnroll did not return after a valid enrollment")
	}
	if _, ok := cliPins.Get("mybox"); !ok {
		t.Fatal("client pin missing after ListenAndEnroll")
	}
	if _, ok := srvPins.NameFor(cliID.Fingerprint()); !ok {
		t.Fatal("server pin missing after ListenAndEnroll")
	}
}

// waitDial blocks until addr accepts a TCP connection (listener readiness), so a test never
// races the goroutine that binds it.
func waitDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("enroll listener never came up on %s", addr)
}
