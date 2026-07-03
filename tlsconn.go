package loom

// Pinned-SPKI mTLS: the transport wall once two nodes have paired. Both sides present
// their identity cert; NEITHER side does CA validation, chain building, or hostname
// checks. Trust is exactly one thing — does the peer's cert SPKI fingerprint match a
// pin? This is RFC 7250's raw-public-key model expressed in stdlib crypto/tls: a
// self-signed cert as the envelope, a custom VerifyPeerCertificate as the trust check.
// TLS 1.3 minimum (no downgrade, modern AEAD only). Zero dependencies.
//
// A note on tls.Config asymmetry: InsecureSkipVerify is a CLIENT-side switch (it turns
// off the client's default verification of the server). It has no effect in a server
// config. The server's equivalent is ClientAuth: RequireAnyClientCert — "make the client
// present a cert, but do not validate it against ClientCAs; I will verify it myself."
// So the two configs look different but do the same job: request the peer cert, then run
// our pin check in VerifyPeerCertificate. (The proposal's "InsecureSkipVerify on both"
// is shorthand for this symmetric intent.)

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// TLSServerConfig builds the server side of pinned mTLS: present our identity cert,
// REQUIRE a client cert (RequireAnyClientCert), and accept the handshake iff the client
// cert's SPKI fingerprint is pinned — trusting ANY pinned peer (a server does not know
// in advance which of its paired peers is dialing). It fails CLOSED: an empty pin store
// trusts no one, so every handshake is rejected until a `loom pair` adds a peer.
//
// Which peer connected is recoverable post-handshake from the TLS state — see
// PeerNameFromState — because VerifyPeerCertificate can only return an error, not a name.
func TLSServerConfig(id *Identity, pins *PinStore) (*tls.Config, error) {
	if id == nil || pins == nil {
		return nil, fmt.Errorf("tls: identity and pin store are both required")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{id.TLSCertificate()},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		// No ClientCAs, no default verification — the pin check IS the verification.
		VerifyPeerCertificate: pinVerifier(pins, ""),
	}, nil
}

// TLSClientConfig builds the client side of pinned mTLS: present our identity cert, skip
// the default CA/hostname verification (InsecureSkipVerify — there is no CA), and accept
// the server iff its cert SPKI fingerprint matches the ONE peer we expect by name.
// expectPeer must be a pinned name; the client dialed a specific box and knows who should
// answer, so it pins to exactly that fingerprint (a stricter check than the server's
// any-pinned-peer). An unknown expectPeer fails closed.
func TLSClientConfig(id *Identity, pins *PinStore, expectPeer string) (*tls.Config, error) {
	if id == nil || pins == nil {
		return nil, fmt.Errorf("tls: identity and pin store are both required")
	}
	if expectPeer == "" {
		return nil, fmt.Errorf("tls: expectPeer is required (name the pinned peer you are dialing)")
	}
	if _, ok := pins.Get(expectPeer); !ok {
		return nil, fmt.Errorf("tls: no pinned peer named %q — pair first: loom pair --connect <host:port> --name %s", expectPeer, expectPeer)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{id.TLSCertificate()},
		// InsecureSkipVerify disables the stdlib's CA + hostname checks ONLY; our
		// VerifyPeerCertificate still runs and is the real gate. This is the documented
		// Go pattern for RFC-7250-style key pinning.
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: pinVerifier(pins, expectPeer),
	}, nil
}

// pinVerifier returns a VerifyPeerCertificate callback that computes the peer leaf's SPKI
// fingerprint and checks it against the pins. If expectPeer is "" (server side) it trusts
// any pinned fingerprint; otherwise (client side) it requires the fingerprint pinned to
// that exact name. The callback receives raw DER (verifiedChains is empty because we
// disabled chain building), so it parses the leaf itself.
func pinVerifier(pins *PinStore, expectPeer string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("tls: parse peer certificate: %w", err)
		}
		fp := SPKIFingerprint(leaf)

		if expectPeer != "" {
			want, ok := pins.Get(expectPeer)
			if !ok {
				return fmt.Errorf("tls: peer %q is no longer pinned (revoked)", expectPeer)
			}
			if fp != want {
				return fmt.Errorf("tls: peer cert fingerprint does not match the pin for %q (expected %s, got %s) — possible impersonation", expectPeer, short(want), short(fp))
			}
			return nil
		}

		if _, ok := pins.NameFor(fp); !ok {
			return fmt.Errorf("tls: peer cert fingerprint %s is not pinned — pair first, or this peer was revoked", short(fp))
		}
		return nil
	}
}

// PeerNameFromState reports which pinned peer completed a handshake, read from the TLS
// connection state after Handshake succeeds. The server uses it to know who it is talking
// to (VerifyPeerCertificate proved the peer is SOME pinned peer; this names it). ok is
// false if the state carries no peer cert or its fingerprint is unpinned (which the
// verifier would already have rejected — this is a belt-and-suspenders read).
func PeerNameFromState(state tls.ConnectionState, pins *PinStore) (name string, ok bool) {
	if len(state.PeerCertificates) == 0 {
		return "", false
	}
	return pins.NameFor(SPKIFingerprint(state.PeerCertificates[0]))
}

// short abbreviates a 64-char hex fingerprint for a human-facing error (first 8 + last 4).
func short(fp string) string {
	if len(fp) <= 16 {
		return fp
	}
	return fp[:8] + "…" + fp[len(fp)-4:]
}
