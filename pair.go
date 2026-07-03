package loom

// SAS (Short Authentication String) pairing — loom's zero-CA, no-pre-shared-secret way
// to establish mutual trust between two nodes, modeled on RFC 6189 (ZRTP) and the
// Bluetooth "numeric comparison" flow (Michael's phone-pairs-watch analogy). Two nodes
// exchange ephemeral ECDH public keys and their identity certs over a plain TCP conn,
// each derives the SAME six-digit PIN from a hash of the exchange, both humans confirm
// the PINs match on the two screens, and only then does each node PIN the other's SPKI
// fingerprint. A man-in-the-middle who substitutes its own keys produces a DIFFERENT
// transcript on each leg -> different PINs -> the humans see a mismatch -> abort. The
// human tap is the trust anchor; there is no CA and no shared token.
//
// ★ Commit-then-reveal (the ZRTP anti-grinding trick, RFC 6189 §4.4.1). A naive SAS is
// grindable: a fast MITM, sitting in the middle, could see a victim's ephemeral key and
// THEN choose its own ephemeral key adaptively, trying ~10^6 candidates to force the two
// six-digit PINs to collide (a six-digit SAS is only ~20 bits). WPS PIN fell to exactly
// this class of attack. The defense: each side first sends a COMMITMENT
// sha256(ephPub || nonce) BEFORE either reveals its ephemeral key. Having committed, a
// party can no longer choose its key after seeing the peer's — the one online guess is
// all it gets, and a blind guess collides a 6-digit SAS with probability 10^-6. We
// implement published, test-vectored crypto rather than inventing a scheme (the WPS
// cautionary tale from the standards research).

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// PairOutcome is what a completed ceremony hands the CLI: the PIN to show the human, and
// the peer fingerprint to pin IF they confirm. The CLI (not this package) does the human
// confirmation and the pinning — the ceremony computes trust; the human grants it.
type PairOutcome struct {
	SAS             string // six digits — display grouped (loom.GroupSAS)
	PeerFingerprint string // pin this on confirmation
}

// PairConnect dials a peer's `loom pair --listen` and runs the SAS ceremony as the
// initiator (dialer). It returns the derived PIN + the peer's fingerprint; nothing is
// pinned — the caller shows the PIN, confirms it matches the other screen, and only then
// pins the fingerprint under a chosen name.
func PairConnect(addr string, id *Identity) (*PairOutcome, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("pair: dial %s: %w", addr, err)
	}
	defer conn.Close()
	return finishPair(conn, id, true)
}

// PairListen binds addr, accepts ONE pairing connection, and runs the ceremony as the
// responder, then returns. `loom pair` is a one-shot ceremony (two humans, one moment),
// not a daemon — one pairing per invocation keeps the trust decision deliberate.
func PairListen(addr string, id *Identity) (*PairOutcome, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	conn, err := ln.Accept()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return finishPair(conn, id, false)
}

// GroupSAS renders the six-digit SAS as "123 456" for display (exported for the CLI).
func GroupSAS(sas string) string { return groupSAS(sas) }

// finishPair runs the handshake and maps the internal result to the exported outcome.
func finishPair(conn net.Conn, id *Identity, initiator bool) (*PairOutcome, error) {
	res, err := pairHandshake(conn, id, initiator)
	if err != nil {
		return nil, err
	}
	return &PairOutcome{SAS: res.SAS, PeerFingerprint: res.PeerFingerprint}, nil
}

// pairMsg is one framed message in the pairing ceremony. It is exchanged as
// newline-free JSON (a json.Encoder/Decoder pair frames it over the raw conn); []byte
// fields marshal as base64. commit carries only the commitment; reveal carries the
// ephemeral public key, its nonce, and the identity cert (DER).
type pairMsg struct {
	Type   string `json:"type"`             // "commit" | "reveal"
	Commit []byte `json:"commit,omitempty"` // sha256(ephPub || nonce)
	EphPub []byte `json:"eph_pub,omitempty"`
	Nonce  []byte `json:"nonce,omitempty"`
	Cert   []byte `json:"cert,omitempty"` // identity cert, DER
}

// pairResult is what a completed (but not-yet-confirmed) handshake yields: the PIN both
// humans compare, plus the peer identity to pin if they confirm. Nothing is written to
// the pin store until the caller confirms — the ceremony computes trust, the human
// grants it.
type pairResult struct {
	SAS             string            // six digits, e.g. "472916" (display grouped: "472 916")
	PeerFingerprint string            // SPKI fingerprint to pin on confirmation
	PeerCert        *x509.Certificate // the peer's identity cert (envelope)
}

// pairHandshake runs the commit-then-reveal ceremony over conn and returns the derived
// SAS + peer identity. It performs NO human interaction and writes NO pins — the caller
// displays result.SAS, asks the human to confirm it matches the other screen, and only
// then pins result.PeerFingerprint. initiator must be true on exactly one side (the
// dialer) and false on the other (the listener); the flag fixes the send/recv order so
// the lock-step never deadlocks over the stream.
//
// The exchange:
//
//	initiator: send commit -> recv commit -> send reveal -> recv reveal
//	responder: recv commit -> send commit -> recv reveal -> send reveal
//
// On each reveal the receiver checks sha256(peerEphPub || peerNonce) == the peer's
// earlier commitment; a mismatch aborts (a tampered reveal — the MITM signature).
func pairHandshake(conn io.ReadWriter, id *Identity, initiator bool) (*pairResult, error) {
	curve := ecdh.P256()

	// Our ephemeral contribution: a fresh ECDH keypair + a random nonce. The keypair is
	// per-ceremony (never persisted) — the persistent identity is the CERT, exchanged in
	// the reveal; the ephemeral key exists only to be a committed, transcript-bound value
	// this side could not have chosen after seeing the peer's.
	ephPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pair: ephemeral key: %w", err)
	}
	ephPub := ephPriv.PublicKey().Bytes()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("pair: nonce: %w", err)
	}
	commit := commitTo(ephPub, nonce)

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	sendCommit := func() error { return enc.Encode(pairMsg{Type: "commit", Commit: commit}) }
	sendReveal := func() error {
		return enc.Encode(pairMsg{Type: "reveal", EphPub: ephPub, Nonce: nonce, Cert: id.CertDER()})
	}

	var peerCommit pairMsg
	var peerReveal pairMsg
	recvCommit := func() error { return readMsg(dec, "commit", &peerCommit) }
	recvReveal := func() error { return readMsg(dec, "reveal", &peerReveal) }

	// Strict lock-step, ordered by role, so a stream (net.Pipe / TCP) never deadlocks.
	var steps []func() error
	if initiator {
		steps = []func() error{sendCommit, recvCommit, sendReveal, recvReveal}
	} else {
		steps = []func() error{recvCommit, sendCommit, recvReveal, sendReveal}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}

	// Verify the peer honored ITS commitment: the revealed key+nonce must hash to the
	// commit it sent before seeing ours. This is the whole anti-grinding guarantee.
	if !verifyCommit(peerCommit.Commit, peerReveal.EphPub, peerReveal.Nonce) {
		return nil, fmt.Errorf("pair: commitment mismatch — the peer's reveal does not match its commit (tampered exchange, possible man-in-the-middle); aborting, nothing pinned")
	}

	// Validate the peer's ephemeral public key is a real point on P-256 (rejects a
	// malformed/off-curve value) — a genuine check even though the shared secret is not
	// folded into the SAS in v1 (see the transcript note below).
	if _, err := curve.NewPublicKey(peerReveal.EphPub); err != nil {
		return nil, fmt.Errorf("pair: peer ephemeral key invalid: %w", err)
	}

	peerCert, err := x509.ParseCertificate(peerReveal.Cert)
	if err != nil {
		return nil, fmt.Errorf("pair: parse peer cert: %w", err)
	}

	transcript := pairingTranscript(ephPub, peerReveal.EphPub, spkiFingerprintRaw(id.Cert), spkiFingerprintRaw(peerCert))
	return &pairResult{
		SAS:             sasDigits(transcript),
		PeerFingerprint: SPKIFingerprint(peerCert),
		PeerCert:        peerCert,
	}, nil
}

// commitTo is the commitment sha256(ephPub || nonce). A side sends this before revealing
// ephPub, binding itself to a key it chose blind.
func commitTo(ephPub, nonce []byte) []byte {
	h := sha256.New()
	h.Write(ephPub)
	h.Write(nonce)
	return h.Sum(nil)
}

// verifyCommit reports whether reveal (ephPub, nonce) matches the earlier commit. The
// compare is constant-time out of habit; the inputs here are not secret, but the
// codebase's crypto compares are all constant-time (see tokens.go) and consistency is
// its own defense against a future refactor leaking timing.
func verifyCommit(commit, ephPub, nonce []byte) bool {
	if len(commit) != sha256.Size {
		return false
	}
	want := commitTo(ephPub, nonce)
	return subtle.ConstantTimeCompare(commit, want) == 1
}

// pairingTranscript assembles the canonical byte string the SAS is derived from: both
// ephemeral public keys, then both SPKI fingerprints, each PAIR sorted by bytes.Compare
// so both nodes — which disagree on which key is "mine" vs "theirs" — hash identical
// input. A MITM that substitutes either its ephemeral key or its cert changes the set of
// values on one leg, so the two PINs diverge.
//
// ★ Transcript composition follows the ratified proposal EXACTLY: both ephemeral
// pubkeys + both SPKI fingerprints. The ECDH shared secret is deliberately NOT folded in
// for v1 — the commit-then-reveal is what defeats grinding, and the SAS over the
// committed public values detects substitution. See the report's design-notes for the
// recommended follow-up (binding the DH secret, ZRTP's full shape) and the honest
// limitation this leaves.
func pairingTranscript(ephSelf, ephPeer, spkiSelf, spkiPeer []byte) []byte {
	e1, e2 := sortPair(ephSelf, ephPeer)
	s1, s2 := sortPair(spkiSelf, spkiPeer)
	out := make([]byte, 0, len(e1)+len(e2)+len(s1)+len(s2))
	out = append(out, e1...)
	out = append(out, e2...)
	out = append(out, s1...)
	out = append(out, s2...)
	return out
}

// sortPair returns a, b in bytes.Compare order — the canonicalization that makes the
// transcript symmetric across the two sides.
func sortPair(a, b []byte) (lo, hi []byte) {
	if bytes.Compare(a, b) <= 0 {
		return a, b
	}
	return b, a
}

// sasDigits derives the six-digit Short Authentication String from a transcript:
// take the first 4 bytes of sha256(transcript) as a big-endian uint32, reduce mod 10^6,
// and zero-pad to six decimal digits. uint32 (~4.3e9) over a 10^6 range makes the
// modulo bias negligible. This is the single point a frozen test vector pins down, so
// the derivation is intentionally tiny and dependency-free.
func sasDigits(transcript []byte) string {
	sum := sha256.Sum256(transcript)
	n := binary.BigEndian.Uint32(sum[:4]) % 1_000_000
	return fmt.Sprintf("%06d", n)
}

// groupSAS renders the six digits as "123 456" for the confirmation prompt — the
// grouping the BLE-pairing UX uses so the eye compares two triples, not one six-run.
func groupSAS(sas string) string {
	if len(sas) != 6 {
		return sas
	}
	return sas[:3] + " " + sas[3:]
}

// readMsg decodes one pairing message and asserts its type, so an out-of-order or
// malformed frame fails loudly rather than being silently misread as the wrong step.
func readMsg(dec *json.Decoder, want string, into *pairMsg) error {
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("pair: read %s: %w", want, err)
	}
	if into.Type != want {
		return fmt.Errorf("pair: protocol error — expected %q, got %q", want, into.Type)
	}
	return nil
}
