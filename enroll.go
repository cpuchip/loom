package loom

// Enrollment — the code-driven, ASYMMETRIC alternative to the SAS pair ceremony
// (pair.go), for clients that cannot run the two-screen PIN compare: an Android app,
// an unattended machine, an agent process. The ratified plan names it directly (the
// "bootstrap-token → client-cert pattern, kubeadm join / Tailscale auth keys" — see
// docs/proposals/loom-mesh-and-mtls.md §"The synthesized auth model"), specialized to
// loom's pinned-SPKI, keys-never-leave model.
//
// The shape: the SERVER mints a short-lived, single-use enrollment CODE and displays it
// (Spin can speak it). The CLIENT generates its OWN identity locally, then submits its
// identity cert plus a MAC proving it knows the code; the server pins the client's SPKI
// and returns its OWN cert (also MAC'd) for the client to pin. End state is the SAME
// mutual pinning as pair.go — but driven by one code the operator reads off the server,
// not a mutual compare.
//
// ★ Why it is safe over a plain (un-TLS'd) channel. The code authenticates BOTH certs:
// each side MACs its cert (HMAC-SHA256 keyed by the code, domain-separated). A man in
// the middle who does not know the code cannot forge either MAC, so it cannot SUBSTITUTE
// either cert — it can only relay the true ones, which is harmless because the later
// mTLS is end-to-end between the real endpoints (the relay learns nothing it can use).
// The code is high-entropy (40 bits) and single-use, so it resists offline grinding
// within its short life. NO private key ever crosses the wire — each side keeps its own
// key and pins only the other's public envelope, exactly as pairing does.
//
// This file is stdlib-only (crypto/hmac, crypto/sha256, encoding/base32, net/http), in
// keeping with loom's zero-dependency go.mod, and contains NO hardcoded host — the
// address is always supplied by the caller (the generic-hub / BYO-endpoint constraint).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// enrollCodeEntropyBytes is the raw entropy behind an enrollment code. 5 bytes = 40 bits,
// rendered as 8 base32 chars. High enough that offline grinding within the code's short,
// single-use life is infeasible; short enough to read aloud or type on a phone.
const enrollCodeEntropyBytes = 5

// enrollPath is the single HTTP route the enrollment listener serves.
const enrollPath = "/enroll"

// enroll MAC domain separators — distinct so a client's MAC can never be replayed as a
// server's (the two directions carry different, non-interchangeable proofs).
const (
	enrollDomainClient = "loom-enroll-client-v1\x00"
	enrollDomainServer = "loom-enroll-server-v1\x00"
)

// base32NoPad is the RFC 4648 base32 alphabet (A–Z, 2–7 — no 0/1/8/9, no lowercase) with
// padding stripped: legible, unambiguous, and safe to speak or type.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewEnrollCode mints a fresh, single-use enrollment code (8 base32 chars, 40 bits). The
// server displays it; the enrolling client submits it once. Grouping for display is the
// CLI's job (GroupEnrollCode).
func NewEnrollCode() (string, error) {
	var b [enrollCodeEntropyBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("enroll: mint code: %w", err)
	}
	return base32NoPad.EncodeToString(b[:]), nil
}

// normalizeEnrollCode canonicalizes a human-entered code: strip spaces/hyphens, uppercase.
// So "k7q2-m9xa", "K7Q2 M9XA", and "K7Q2M9XA" all key the same MAC.
func normalizeEnrollCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	return s
}

// GroupEnrollCode renders an 8-char code as "K7Q2-M9XA" for display (exported for the CLI).
func GroupEnrollCode(code string) string {
	c := normalizeEnrollCode(code)
	if len(c) != 8 {
		return c
	}
	return c[:4] + "-" + c[4:]
}

// enrollMAC is HMAC-SHA256(normalize(code), domain || certDER) — the proof that a party
// holds the code, bound to the specific cert it is presenting. Binding the cert is what
// stops a MITM from substituting a different key: a valid MAC only exists for the cert
// the code-holder actually chose.
func enrollMAC(code, domain string, certDER []byte) []byte {
	m := hmac.New(sha256.New, []byte(normalizeEnrollCode(code)))
	m.Write([]byte(domain))
	m.Write(certDER)
	return m.Sum(nil)
}

// enrollRequest is the client → server body (JSON; []byte fields base64 by encoding/json).
// Label is an OPTIONAL self-suggested pin name the server MAY use when the operator did
// not name the peer explicitly; it never overrides an operator-chosen name.
type enrollRequest struct {
	Label string `json:"label,omitempty"`
	Cert  []byte `json:"cert"` // the client's identity cert, DER
	MAC   []byte `json:"mac"`  // enrollMAC(code, client-domain, Cert)
}

// enrollResponse is the server → client body on success. Cert is the server's identity
// cert (the envelope the client pins); MAC proves the server also holds the code (so the
// client knows it reached the box whose operator read out the code, not an impostor).
type enrollResponse struct {
	Cert []byte `json:"cert"`
	MAC  []byte `json:"mac"` // enrollMAC(code, server-domain, Cert)
	Name string `json:"name,omitempty"` // the name the server pinned this client under (informational)
}

// EnrollServer holds one enrollment window: this box's identity + pin store, the minted
// code, and the name to pin the enrolling client under. It serves exactly ONE successful
// enrollment (the code is single-use), mirroring `loom pair`'s one-ceremony deliberateness.
type EnrollServer struct {
	Identity *Identity
	Pins     *PinStore
	Code     string // the minted code (any casing/grouping; normalized internally)
	PinName  string // operator-chosen name for the enrolling client ("" → fall back to the client's Label)
}

// ListenAndEnroll binds addr (PLAIN http — the client does not yet trust this box's cert,
// so the code+MAC is the wall, not TLS), accepts one VALID enrollment, pins the client's
// SPKI under the resolved name, and returns that name + the client's fingerprint. A bad
// MAC is refused (401) and does NOT consume the window — only a valid enrollment ends it.
// timeout 0 waits indefinitely; otherwise the wait is bounded and returns an error on
// expiry. The caller (the CLI) is responsible for displaying the code before calling this.
func (e *EnrollServer) ListenAndEnroll(addr string, timeout time.Duration) (pinnedName, fingerprint string, err error) {
	if e.Identity == nil || e.Pins == nil {
		return "", "", fmt.Errorf("enroll: identity and pin store are required")
	}
	if normalizeEnrollCode(e.Code) == "" {
		return "", "", fmt.Errorf("enroll: a code is required (mint one with NewEnrollCode)")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", "", fmt.Errorf("enroll: listen %s: %w", addr, err)
	}

	type done struct {
		name string
		fp   string
	}
	resCh := make(chan done, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(enrollPath, func(w http.ResponseWriter, r *http.Request) {
		name, fp, herr := e.handleEnroll(w, r)
		if herr == nil {
			// Success — hand the outcome to the waiter; the server shuts down after.
			select {
			case resCh <- done{name: name, fp: fp}:
			default:
			}
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timeoutCh = t.C
	}
	select {
	case d := <-resCh:
		_ = srv.Shutdown(context.Background())
		return d.name, d.fp, nil
	case <-timeoutCh:
		_ = srv.Shutdown(context.Background())
		return "", "", fmt.Errorf("enroll: timed out after %s with no valid enrollment", timeout)
	}
}

// handleEnroll validates one POST /enroll: decode, verify the client MAC against the code,
// pin the client's cert, and reply with this box's cert (MAC'd). It returns the pinned name
// + fingerprint on success, or an error (already written to w) otherwise. Kept a method so
// a test can drive it via httptest without binding a port.
func (e *EnrollServer) handleEnroll(w http.ResponseWriter, r *http.Request) (name, fingerprint string, err error) {
	if r.Method != http.MethodPost {
		http.Error(w, "enroll: POST required", http.StatusMethodNotAllowed)
		return "", "", fmt.Errorf("method %s", r.Method)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "enroll: read body", http.StatusBadRequest)
		return "", "", err
	}
	var req enrollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "enroll: bad json", http.StatusBadRequest)
		return "", "", err
	}
	if len(req.Cert) == 0 || len(req.MAC) == 0 {
		http.Error(w, "enroll: cert and mac required", http.StatusBadRequest)
		return "", "", fmt.Errorf("missing cert/mac")
	}
	// Verify the client holds the code, bound to the exact cert it presented. Constant-time.
	want := enrollMAC(e.Code, enrollDomainClient, req.Cert)
	if subtle.ConstantTimeCompare(want, req.MAC) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "enrollment code invalid or expired"})
		return "", "", fmt.Errorf("mac mismatch")
	}
	leaf, err := x509.ParseCertificate(req.Cert)
	if err != nil {
		http.Error(w, "enroll: parse cert", http.StatusBadRequest)
		return "", "", err
	}
	fp := SPKIFingerprint(leaf)

	pinName := strings.TrimSpace(e.PinName)
	if pinName == "" {
		pinName = strings.TrimSpace(req.Label)
	}
	if pinName == "" {
		http.Error(w, "enroll: no name to pin under (operator must pass a name, or the client a label)", http.StatusBadRequest)
		return "", "", fmt.Errorf("no pin name")
	}
	if err := e.Pins.Add(pinName, fp); err != nil {
		http.Error(w, "enroll: pin: "+err.Error(), http.StatusInternalServerError)
		return "", "", err
	}

	resp := enrollResponse{
		Cert: e.Identity.CertDER(),
		MAC:  enrollMAC(e.Code, enrollDomainServer, e.Identity.CertDER()),
		Name: pinName,
	}
	writeJSON(w, http.StatusOK, resp)
	return pinName, fp, nil
}

// EnrollResult is what a client-side enrollment yields: the server's fingerprint and the
// local name it was pinned under (the --peer name the client then dials wss with).
type EnrollResult struct {
	ServerFingerprint string
	ServerName        string
}

// EnrollConnect runs the client side: POST this node's cert + code-proof to the server's
// enrollment listener at addr (host:port, plain http), verify the server's returned cert
// against the code, and pin it under serverName. selfLabel is an optional name suggestion
// the server MAY use for its own pin of this client. On success the client can immediately
// drive the server over mTLS: `loom run --connect wss://<addr> --peer <serverName>`.
func EnrollConnect(addr, code, serverName, selfLabel string, id *Identity, pins *PinStore) (*EnrollResult, error) {
	if id == nil || pins == nil {
		return nil, fmt.Errorf("enroll: identity and pin store are required")
	}
	if serverName == "" {
		return nil, fmt.Errorf("enroll: a name for the server is required (the --peer name you will dial)")
	}
	if normalizeEnrollCode(code) == "" {
		return nil, fmt.Errorf("enroll: a code is required (ask the box operator for it)")
	}
	reqBody, err := json.Marshal(enrollRequest{
		Label: selfLabel,
		Cert:  id.CertDER(),
		MAC:   enrollMAC(code, enrollDomainClient, id.CertDER()),
	})
	if err != nil {
		return nil, err
	}
	url := "http://" + addr + enrollPath
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(url, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("enroll: post %s: %w", url, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enroll: server refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var er enrollResponse
	if err := json.Unmarshal(rb, &er); err != nil {
		return nil, fmt.Errorf("enroll: decode response: %w", err)
	}
	if len(er.Cert) == 0 || len(er.MAC) == 0 {
		return nil, fmt.Errorf("enroll: server response missing cert/mac")
	}
	// Verify the SERVER also holds the code, bound to the cert it returned — proves this is
	// the box whose operator read out the code, not a MITM who forwarded our request.
	want := enrollMAC(code, enrollDomainServer, er.Cert)
	if subtle.ConstantTimeCompare(want, er.MAC) != 1 {
		return nil, fmt.Errorf("enroll: server MAC mismatch — the box did not prove the code (possible impersonation); nothing pinned")
	}
	leaf, err := x509.ParseCertificate(er.Cert)
	if err != nil {
		return nil, fmt.Errorf("enroll: parse server cert: %w", err)
	}
	fp := SPKIFingerprint(leaf)
	if err := pins.Add(serverName, fp); err != nil {
		return nil, fmt.Errorf("enroll: pin server: %w", err)
	}
	return &EnrollResult{ServerFingerprint: fp, ServerName: serverName}, nil
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
