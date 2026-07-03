package loom

import (
	"path/filepath"
	"testing"
)

// The pin store round-trips through the file: add, persist, reload, and every lookup
// (by name and by fingerprint) still resolves; a removed pin is gone after reload.
func TestPinStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins")

	ps, err := LoadPinStore(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(ps.List()) != 0 {
		t.Fatalf("fresh store not empty: %v", ps.List())
	}

	const fpA = "aaaa000000000000000000000000000000000000000000000000000000000001"
	const fpB = "bbbb000000000000000000000000000000000000000000000000000000000002"
	if err := ps.Add("box-a", fpA); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if err := ps.Add("box-b", fpB); err != nil {
		t.Fatalf("add b: %v", err)
	}

	// Reload a brand-new store from the same file — the pins must survive.
	ps2, err := LoadPinStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	list := ps2.List()
	if len(list) != 2 || list[0].Name != "box-a" || list[1].Name != "box-b" {
		t.Fatalf("reloaded list wrong (want sorted a,b): %+v", list)
	}
	if got, ok := ps2.Get("box-a"); !ok || got != fpA {
		t.Fatalf("Get(box-a)=%q,%v want %q", got, ok, fpA)
	}
	if name, ok := ps2.NameFor(fpB); !ok || name != "box-b" {
		t.Fatalf("NameFor(fpB)=%q,%v want box-b", name, ok)
	}
	if list[0].AddedAt.IsZero() {
		t.Fatalf("added-at not persisted/parsed")
	}

	// Removing a pin revokes it — after reload it is gone, the other remains.
	if err := ps2.Remove("box-a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ps3, err := LoadPinStore(path)
	if err != nil {
		t.Fatalf("reload after remove: %v", err)
	}
	if _, ok := ps3.Get("box-a"); ok {
		t.Fatalf("box-a still pinned after remove")
	}
	if _, ok := ps3.Get("box-b"); !ok {
		t.Fatalf("box-b lost when removing box-a")
	}
	if _, ok := ps3.NameFor(""); ok {
		t.Fatalf("empty fingerprint matched a pin")
	}
}
