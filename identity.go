package loom

// A loom node's cryptographic identity: a persistent ECDSA P-256 keypair and a
// self-signed certificate, generated lazily on first use and stored under the loom
// home dir. The certificate is only an ENVELOPE — loom does no CA validation, no
// chain building, no hostname check. Trust is the pinned SubjectPublicKeyInfo (SPKI)
// fingerprint: RFC 7250's "raw public key" model, the same trust shape as an SSH
// host key or a WireGuard peer key. Because Go's crypto/tls has no RFC 7250 wire
// format, the idiomatic equivalent is a self-signed cert plus a custom
// VerifyPeerCertificate that checks the SPKI fingerprint against a pin (see
// tlsconn.go). Zero dependencies: crypto/ecdsa + crypto/x509 + crypto/sha256, all
// stdlib.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// loomDir is the per-user loom home (keys, pins, tokens). LOOM_HOME overrides it —
// which is also what lets tests point identity + pins at a scratch dir instead of the
// real ~/.loom. A blank home dir (no HOME) falls back to ".loom" in the cwd rather
// than failing, so a headless box with no HOME still works.
func loomDir() string {
	if d := os.Getenv("LOOM_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".loom"
	}
	return filepath.Join(home, ".loom")
}

// identityDir is where key.pem + cert.pem live (the default for LoadOrCreateIdentity).
func identityDir() string { return filepath.Join(loomDir(), "identity") }

// Identity is this node's persistent keypair + self-signed cert. The private key
// never leaves the box; the cert (its public envelope) is what a peer pins.
type Identity struct {
	dir     string
	priv    *ecdsa.PrivateKey
	certDER []byte

	// Cert is the parsed self-signed certificate. Its SPKI fingerprint (Fingerprint)
	// is the node's stable identity — the string a peer pins and every later mTLS
	// handshake is checked against.
	Cert *x509.Certificate
}

// LoadOrCreateIdentity loads the node identity from dir, generating a fresh ECDSA
// P-256 keypair + self-signed cert on first use (dir "" resolves to ~/.loom/identity).
// key.pem is written 0600 (the private key); cert.pem is the public envelope. The
// operation is lazy and idempotent: a second call reuses the stored key, so a node's
// pinned identity is stable across restarts.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	if dir == "" {
		dir = identityDir()
	}
	keyPath := filepath.Join(dir, "key.pem")
	certPath := filepath.Join(dir, "cert.pem")

	if keyPEM, err := os.ReadFile(keyPath); err == nil {
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return nil, fmt.Errorf("identity: key present but cert missing (%s): %w", certPath, err)
		}
		return loadIdentity(dir, keyPEM, certPEM)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("identity: read key: %w", err)
	}

	return createIdentity(dir, keyPath, certPath)
}

// loadIdentity parses a stored key + cert pair back into an Identity.
func loadIdentity(dir string, keyPEM, certPEM []byte) (*Identity, error) {
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("identity: key.pem is not PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse key: %w", err)
	}
	priv, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: key is %T, want ECDSA", keyAny)
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("identity: cert.pem is not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse cert: %w", err)
	}
	return &Identity{dir: dir, priv: priv, certDER: cb.Bytes, Cert: cert}, nil
}

// createIdentity mints a new P-256 keypair + self-signed cert and persists both. The
// cert fields (CN, validity) barely matter — pinning ignores them — but sane values
// are set so the envelope is a well-formed X.509 that any TLS stack will parse. A long
// validity avoids an expiry ceremony loom has no CA to run; revocation is "delete the
// pin," not "wait for NotAfter."
func createIdentity(dir, keyPath, certPath string) (*Identity, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("identity: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "loom-node"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("identity: create cert: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("identity: reparse cert: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("identity: mkdir %s: %w", dir, err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("identity: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("identity: write key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("identity: write cert: %w", err)
	}
	return &Identity{dir: dir, priv: priv, certDER: certDER, Cert: cert}, nil
}

// CertDER is the node's certificate in DER form — what the pairing ceremony sends to
// the peer so it can compute (and, on confirmation, pin) this node's fingerprint.
func (id *Identity) CertDER() []byte { return id.certDER }

// Fingerprint is this node's SPKI fingerprint — its stable pinned identity.
func (id *Identity) Fingerprint() string { return SPKIFingerprint(id.Cert) }

// TLSCertificate packages the key + cert for a tls.Config's Certificates slice. Leaf
// is set so the TLS stack need not re-parse it per handshake.
func (id *Identity) TLSCertificate() tls.Certificate {
	return tls.Certificate{
		Certificate: [][]byte{id.certDER},
		PrivateKey:  id.priv,
		Leaf:        id.Cert,
	}
}

// SPKIFingerprint is the sha256 of a certificate's SubjectPublicKeyInfo, lowercase hex
// (64 chars, no separators). This is the pinned identity — NOT a fingerprint of the
// whole cert, because a node may legitimately re-issue its envelope (new serial, new
// validity) around the SAME keypair and stay the same peer. Two certs with the same
// public key share a fingerprint; a different keypair is a different node. Hex was
// chosen over base32 to match the hex idiom already used for tokens/handles and because
// it round-trips through a config file unambiguously.
func SPKIFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// spkiFingerprintRaw is the raw 32-byte SPKI digest (the pre-hex form), used to build
// the pairing transcript in a canonical binary order.
func spkiFingerprintRaw(cert *x509.Certificate) []byte {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return sum[:]
}
