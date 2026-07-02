package loom

// The token store gates who may drive this box. It holds sha256 hashes of the
// tokens (never the tokens themselves in memory longer than a verify), reads a
// newline-delimited file (blanks and #-comments ignored), and compares in constant
// time. Modeled on llama-chip's hub store: mint (append), a set of hashes, verify.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
)

type tokenStore struct {
	mu     sync.RWMutex
	hashes map[[32]byte]struct{}
}

// loadTokenStore reads tokenFile into a set of sha256 hashes. A missing file yields
// an empty store (the caller decides whether that is fatal — Serve refuses to start
// with no tokens; --add-token creates the file).
func loadTokenStore(path string) (*tokenStore, error) {
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ts, nil
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
		ts.hashes[sha256.Sum256([]byte(line))] = struct{}{}
	}
	return ts, sc.Err()
}

// count is the number of loaded tokens.
func (ts *tokenStore) count() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.hashes)
}

// verify reports whether token is one of the loaded tokens. The comparison is
// constant-time and does not short-circuit on the first match, so timing does not
// reveal which token matched (it still reveals the token count, which is not secret).
func (ts *tokenStore) verify(token string) bool {
	if token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	matched := 0
	for h := range ts.hashes {
		h := h
		matched |= subtle.ConstantTimeCompare(h[:], sum[:])
	}
	return matched == 1
}

// AddToken mints a fresh random token, appends it to the token file (creating it
// with 0600 perms if absent), and returns the plaintext so the operator can hand it
// to a client. This is what `loom serve --add-token` calls.
func AddToken(path string) (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, token); err != nil {
		return "", err
	}
	return token, nil
}
