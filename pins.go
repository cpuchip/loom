package loom

// The pin store is loom's trust anchor for pinned-SPKI mTLS: a flat file mapping a
// peer NAME to the SPKI fingerprint the human confirmed during pairing (see pair.go).
// A handshake is trusted iff the peer's cert fingerprint is in this store. There is no
// CA and no expiry — adding a pin is granting trust, removing a pin is REVOKING it.
// The file is line-oriented (name<TAB>fingerprint<TAB>added-at), so it is greppable
// and hand-editable, mirroring the token file's plain-text ethos.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// pinsPath is the default pin file (~/.loom/pins).
func pinsPath() string { return filepath.Join(loomDir(), "pins") }

// Pin is one trusted peer: a human-chosen name, its pinned SPKI fingerprint, and when
// it was added (bookkeeping only — pins do not expire).
type Pin struct {
	Name        string
	Fingerprint string
	AddedAt     time.Time
}

// PinStore is the in-memory set of pins backing a pin file. It is safe for concurrent
// reads (the TLS verify callbacks hit it on every handshake) and guarded writes.
type PinStore struct {
	path string

	mu   sync.RWMutex
	pins map[string]Pin // keyed by peer name
}

// LoadPinStore reads the pin file at path (path "" resolves to ~/.loom/pins). A missing
// file yields an empty, writable store — the common first-run case, where the first
// `loom pair` creates it. Malformed lines are skipped rather than fatal, so one bad
// hand-edit never locks a node out of every peer.
func LoadPinStore(path string) (*PinStore, error) {
	if path == "" {
		path = pinsPath()
	}
	ps := &PinStore{path: path, pins: map[string]Pin{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ps, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue // malformed — need at least name + fingerprint
		}
		p := Pin{Name: fields[0], Fingerprint: fields[1]}
		if len(fields) >= 3 {
			if t, err := time.Parse(time.RFC3339, fields[2]); err == nil {
				p.AddedAt = t
			}
		}
		ps.pins[p.Name] = p
	}
	return ps, sc.Err()
}

// Add records (or replaces) a pin for name and persists the whole store. Re-pinning an
// existing name overwrites it — the path a peer takes after re-keying (or after you
// re-pair to rotate a leaked key).
func (ps *PinStore) Add(name, fingerprint string) error {
	if name == "" || fingerprint == "" {
		return fmt.Errorf("pins: name and fingerprint are both required")
	}
	ps.mu.Lock()
	ps.pins[name] = Pin{Name: name, Fingerprint: fingerprint, AddedAt: time.Now().UTC()}
	ps.mu.Unlock()
	return ps.save()
}

// Remove deletes the pin for name (revoking that peer) and persists. Removing an absent
// name is a no-op success — revocation should be idempotent.
func (ps *PinStore) Remove(name string) error {
	ps.mu.Lock()
	delete(ps.pins, name)
	ps.mu.Unlock()
	return ps.save()
}

// Get returns the pinned fingerprint for name. A client verifying a server it dialed
// looks up the ONE peer it expects by name.
func (ps *PinStore) Get(name string) (fingerprint string, ok bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	p, ok := ps.pins[name]
	return p.Fingerprint, ok
}

// NameFor reverse-looks-up the peer name pinned to fingerprint. A server accepting an
// inbound handshake trusts ANY pinned peer, so it matches on the fingerprint and reports
// which name connected. Empty fingerprints never match (a defensive guard against a
// cert with no key material producing a hollow pin).
func (ps *PinStore) NameFor(fingerprint string) (name string, ok bool) {
	if fingerprint == "" {
		return "", false
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	for _, p := range ps.pins {
		if p.Fingerprint == fingerprint {
			return p.Name, true
		}
	}
	return "", false
}

// List returns all pins, sorted by name — for `loom pins` and for tests.
func (ps *PinStore) List() []Pin {
	ps.mu.RLock()
	out := make([]Pin, 0, len(ps.pins))
	for _, p := range ps.pins {
		out = append(out, p)
	}
	ps.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// save writes the whole store to a temp file and renames it over the target, so a
// crash mid-write never leaves a half-truncated pin file (an atomic replace). The file
// is 0600 — the pin set is who-may-drive-me, not world-readable trivia.
func (ps *PinStore) save() error {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	if dir := filepath.Dir(ps.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	var sb strings.Builder
	sb.WriteString("# loom pinned peers — name<TAB>SPKI-fingerprint<TAB>added-at (RFC3339)\n")
	sb.WriteString("# a pin grants trust; delete a line to revoke a peer.\n")
	for _, p := range sortedPins(ps.pins) {
		added := p.AddedAt.UTC().Format(time.RFC3339)
		fmt.Fprintf(&sb, "%s\t%s\t%s\n", p.Name, p.Fingerprint, added)
	}
	tmp := ps.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ps.path)
}

// sortedPins is a stable name-ordered slice of the map (deterministic file output, so a
// re-save produces a clean diff instead of reshuffling lines).
func sortedPins(m map[string]Pin) []Pin {
	out := make([]Pin, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
