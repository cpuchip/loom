package loom

// Coexistence — run ONE loom server on TWO listeners at once: the existing plain-ws
// listener (token-gated) AND a pinned-mTLS wss listener, sharing ONE resident set. This
// is the migration bridge: clients move from token to pin ONE AT A TIME, and because
// both listeners drive the same *server, a client keeps seeing the same warm residents
// across the move instead of losing them at a hard cutover.
//
// Security posture during coexistence: a token file is REQUIRED and gates BOTH listeners.
// The plain listener has no certificate wall, so the token is its only gate; the wss
// listener adds the pin ON TOP of the token (belt-and-suspenders) rather than relaxing it
// — coexistence is strictly the stricter of the two modes. Once every client has migrated,
// switch to pure `loom serve --tls` (pin-only; token optional), which is the end state.
//
// This lives in its own file and edits none of serve.go's existing functions (the
// concurrent sessions-visibility arc owns that surface). The one shared piece — the HTTP
// router — is factored into (*server).httpHandler so both listeners use identical routing;
// it mirrors the inline handler in (*server).serve.

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// httpHandler is the request router shared by every listener a server runs on: the
// OpenAI-compat shim on /v1/chat/completions (and its /chat/completions alias), the
// native hand-rolled websocket everywhere else. It mirrors the inline handler built in
// (*server).serve — kept identical so a request behaves the same on the plain and the
// mTLS listener (only the transport underneath differs).
func (s *server) httpHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions" {
			s.serveOpenAI(w, r)
			return
		}
		ws, err := wsUpgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.handle(ws)
	})
}

// ServeBoth runs loom on a plain listener AND a pinned-mTLS listener simultaneously,
// against one shared resident set. tokenFile is REQUIRED — it gates both listeners (the
// plain one has no cert wall). id + pins back the mTLS listener's server identity and the
// enrollment pin store (peers enrolled via `loom pair` or `loom enroll`). idleTTL reaps
// idle residents exactly as plain Serve does — the reaper runs ONCE for the shared server.
// The call blocks until either listener errors.
func ServeBoth(plainAddr, tlsAddr, tokenFile string, backends map[string]Backend, idleTTL time.Duration, id *Identity, pins *PinStore) error {
	if tokenFile == "" {
		return fmt.Errorf("serve: coexistence (--tls-listen) requires --token-file (it gates the plain listener; the pin is added on top of the tls one)")
	}
	if id == nil || pins == nil {
		return fmt.Errorf("serve: coexistence requires a loaded identity and pin store")
	}
	if plainAddr == tlsAddr {
		return fmt.Errorf("serve: --listen and --tls-listen must differ (got %q for both)", plainAddr)
	}
	ts, err := loadTokenStore(tokenFile)
	if err != nil {
		return fmt.Errorf("serve: load tokens: %w", err)
	}
	if ts.count() == 0 {
		return fmt.Errorf("serve: token file %q has no tokens — mint one:\n  loom serve --token-file %q --add-token", tokenFile, tokenFile)
	}
	cfg, err := TLSServerConfig(id, pins)
	if err != nil {
		return fmt.Errorf("serve: tls config: %w", err)
	}

	plainLn, err := net.Listen("tcp", plainAddr)
	if err != nil {
		return fmt.Errorf("serve: listen plain %s: %w", plainAddr, err)
	}
	tlsRaw, err := net.Listen("tcp", tlsAddr)
	if err != nil {
		_ = plainLn.Close()
		return fmt.Errorf("serve: listen tls %s: %w", tlsAddr, err)
	}
	tlsLn := tls.NewListener(tlsRaw, cfg)

	// One server, shared by both listeners. requireToken stays true (newServer's default),
	// so BOTH listeners demand a token; the tls listener's pin is the additional wall.
	srv := newServer(ts, backends, idleTTL)

	fmt.Fprintf(os.Stderr,
		"loom serve (coexistence) — plain %s (token) + pinned mTLS %s (pin+token) — pinned peers: %d, tokens: %d, backends: %v, idle-ttl: %s\n",
		plainLn.Addr(), tlsLn.Addr(), len(pins.List()), ts.count(), names(backends), idleTTL)

	return srv.serveBoth(plainLn, tlsLn)
}

// serveBoth runs one server on two already-bound listeners with a SINGLE reaper (split
// out from ServeBoth so a test can bind 127.0.0.1:0 for each and drive the shared server
// without a rebind race — the same shape as serveOn). The first listener to error ends it.
func (s *server) serveBoth(plainLn, tlsLn net.Listener) error {
	if s.idleTTL > 0 {
		go s.reapLoop() // the reaper runs ONCE for the shared resident set
	}
	handler := s.httpHandler()
	errCh := make(chan error, 2)
	go func() { errCh <- (&http.Server{Handler: handler}).Serve(plainLn) }()
	go func() { errCh <- (&http.Server{Handler: handler}).Serve(tlsLn) }()
	return <-errCh
}
