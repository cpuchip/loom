package loom

import (
	"net"
	"testing"
)

// The FROZEN SAS test vector. sasDigits is a pure function of the transcript bytes; this
// pins the derivation so a future refactor that changes the truncation (byte count,
// modulus, endianness) is caught immediately. The input is a fixed, arbitrary byte
// string; the expected output was computed once from the implementation and frozen here.
var (
	sasVectorInput = []byte("loom SAS test vector v1 — do not change")
	sasVectorWant  = "454584" // frozen 2026-07-03: sasDigits(sasVectorInput)
)

func TestSASVectorFrozen(t *testing.T) {
	got := sasDigits(sasVectorInput)
	if len(got) != 6 {
		t.Fatalf("SAS is %d chars, want 6: %q", len(got), got)
	}
	if got != sasVectorWant {
		t.Fatalf("SAS vector drifted: got %q, frozen %q — if this is an intentional derivation change, re-freeze deliberately", got, sasVectorWant)
	}
	if groupSAS(got) != got[:3]+" "+got[3:] {
		t.Fatalf("groupSAS malformed: %q", groupSAS(got))
	}
}

// The same transcript always yields the same SAS; a one-byte difference (overwhelmingly)
// yields a different one. Both nodes derive the SAME digits from the same exchange, which
// is what makes the human comparison meaningful.
func TestSASDeterministic(t *testing.T) {
	a := sasDigits([]byte("transcript-one"))
	if a != sasDigits([]byte("transcript-one")) {
		t.Fatalf("SAS not deterministic")
	}
	if a == sasDigits([]byte("transcript-two")) {
		t.Fatalf("distinct transcripts collided (astronomically unlikely — check the derivation)")
	}
}

// pairingTranscript is symmetric: the two nodes disagree on which key is "self" vs
// "peer", but the sorted transcript — and therefore the SAS — is identical.
func TestPairingTranscriptSymmetric(t *testing.T) {
	ephA, ephB := []byte{0x02, 0xaa}, []byte{0x03, 0xbb}
	spkiA, spkiB := []byte{0x11}, []byte{0x22}
	fromA := pairingTranscript(ephA, ephB, spkiA, spkiB)
	fromB := pairingTranscript(ephB, ephA, spkiB, spkiA)
	if string(fromA) != string(fromB) {
		t.Fatalf("transcript not symmetric across the two sides")
	}
}

// Commit-then-reveal rejects a tampered reveal: a commitment binds a specific ephemeral
// key + nonce, so a reveal with either field altered fails verification — the check that
// denies a MITM the ability to grind the SAS.
func TestVerifyCommitRejectsTamper(t *testing.T) {
	ephPub := []byte("ephemeral-public-key-bytes-xxxxx")
	nonce := []byte("nonce-bytes-yyyyyyyyyyyyyyyyyyyyy")
	commit := commitTo(ephPub, nonce)

	if !verifyCommit(commit, ephPub, nonce) {
		t.Fatalf("honest reveal rejected")
	}

	tampered := append([]byte(nil), ephPub...)
	tampered[0] ^= 0x01
	if verifyCommit(commit, tampered, nonce) {
		t.Fatalf("tampered ephemeral key accepted (commitment broken)")
	}
	badNonce := append([]byte(nil), nonce...)
	badNonce[5] ^= 0x80
	if verifyCommit(commit, ephPub, badNonce) {
		t.Fatalf("tampered nonce accepted (commitment broken)")
	}
	badCommit := append([]byte(nil), commit...)
	badCommit[0] ^= 0xff
	if verifyCommit(badCommit, ephPub, nonce) {
		t.Fatalf("wrong commitment accepted")
	}
	if verifyCommit([]byte{0x00}, ephPub, nonce) {
		t.Fatalf("short commitment accepted")
	}
}

// The full ceremony over an in-process pipe: two real identities pair, both derive the
// SAME six-digit PIN, and each learns the OTHER's true SPKI fingerprint — so a confirmed
// pairing pins the right keys. This is the integration oracle for pair.go with the human
// tap simulated as "yes" (we just read the result both sides would confirm).
func TestPairHandshakeOverPipe(t *testing.T) {
	idA := testIdentity(t)
	idB := testIdentity(t)

	ca, cb := net.Pipe()
	defer ca.Close()
	defer cb.Close()

	type res struct {
		r   *pairResult
		err error
	}
	ch := make(chan res, 2)
	go func() { r, err := pairHandshake(ca, idA, true); ch <- res{r, err} }()  // dialer/initiator
	go func() { r, err := pairHandshake(cb, idB, false); ch <- res{r, err} }() // listener/responder

	r1 := <-ch
	r2 := <-ch
	if r1.err != nil || r2.err != nil {
		t.Fatalf("handshake errored: %v / %v", r1.err, r2.err)
	}

	// Both screens show the same PIN — the whole point of SAS comparison.
	if r1.r.SAS != r2.r.SAS {
		t.Fatalf("the two sides derived different PINs: %s vs %s", r1.r.SAS, r2.r.SAS)
	}

	// Each side learned the other's real fingerprint (regardless of who was initiator).
	fps := map[string]bool{r1.r.PeerFingerprint: true, r2.r.PeerFingerprint: true}
	if !fps[idA.Fingerprint()] || !fps[idB.Fingerprint()] {
		t.Fatalf("peer fingerprints wrong: got %v, want {%s, %s}", fps, idA.Fingerprint(), idB.Fingerprint())
	}
	if r1.r.PeerFingerprint == r2.r.PeerFingerprint {
		t.Fatalf("both sides pinned the same fingerprint — one identity leaked into both")
	}
}
