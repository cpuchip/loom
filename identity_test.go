package loom

import (
	"path/filepath"
	"testing"
)

// testIdentity mints a throwaway node identity in a fresh temp dir.
func testIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	return id
}

// A fingerprint is stable across reloads of the same stored key, and distinct across
// independently generated keys. This is the property the whole pin model rests on.
func TestSPKIFingerprintStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")

	id1, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fp1 := id1.Fingerprint()
	if len(fp1) != 64 {
		t.Fatalf("fingerprint is %d hex chars, want 64: %q", len(fp1), fp1)
	}

	// Reload from disk — must be byte-identical (same key => same envelope => same fp).
	id2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := id2.Fingerprint(); got != fp1 {
		t.Fatalf("reloaded fingerprint changed: %s -> %s", fp1, got)
	}
	if string(id1.CertDER()) != string(id2.CertDER()) {
		t.Fatalf("reloaded cert DER differs from stored")
	}
	// SPKIFingerprint on the reparsed cert agrees with the helper on the identity.
	if got := SPKIFingerprint(id2.Cert); got != fp1 {
		t.Fatalf("SPKIFingerprint(cert)=%s, Fingerprint()=%s", got, fp1)
	}

	// A separate identity is a separate node.
	other := testIdentity(t)
	if other.Fingerprint() == fp1 {
		t.Fatalf("two independently generated identities share a fingerprint: %s", fp1)
	}
}
