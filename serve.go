package loom

// loom serve — loom as a websocket service. A long-running server exposes loom's
// existing Backend/Session interface over a socket, so a client (another loom, or a
// browser) drives sessions with a token instead of spawning subprocesses/ssh. The
// server drives REAL backends through the same Backend/Session/Interruptible
// interfaces, so run/chat/review work unchanged over the wire. Design + rationale:
// docs/proposals/loom-serve.md.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
)

// serverName is announced in the hello reply.
const serverName = "loom"

// The wire protocol: JSON frames, one per websocket message. A single flat envelope
// carries every op's fields (omitempty keeps each frame small). op is the
// discriminator.
//
//	c→s: hello, open, send, interrupt, close
//	s→c: hello_ok, opened, event, reply, interrupt_ack, closed, error
const (
	opHello        = "hello"
	opHelloOK      = "hello_ok"
	opOpen         = "open"
	opOpened       = "opened"
	opSend         = "send"
	opEvent        = "event"
	opReply        = "reply"
	opInterrupt    = "interrupt"
	opInterruptAck = "interrupt_ack" // its own op so a mid-turn ack never looks like a send terminal
	opClose        = "close"
	opClosed       = "closed"
	opError        = "error"
)

// frame is one protocol message in either direction.
type frame struct {
	Op string `json:"op"`

	// hello (c→s) / hello_ok (s→c)
	Token    string   `json:"token,omitempty"`
	Client   string   `json:"client,omitempty"`
	OK       bool     `json:"ok,omitempty"`
	Server   string   `json:"server,omitempty"`
	Backends []string `json:"backends,omitempty"`

	// open (c→s): which backend + the session opts
	Agent string       `json:"agent,omitempty"`
	Opts  *SessionOpts `json:"opts,omitempty"`

	// session addressing (opened/send/interrupt/close/…): the server-side handle
	SessionID string `json:"session_id,omitempty"`

	// send (c→s)
	Text string `json:"text,omitempty"`

	// close (c→s): leave the process resident vs. drop it now
	KeepAlive bool `json:"keep_alive,omitempty"`

	// event/reply (s→c)
	Event *Event `json:"event,omitempty"`
	Reply *Reply `json:"reply,omitempty"`

	// error / interrupt_ack (s→c)
	Err string `json:"error,omitempty"`
}

// residentSession is a live server-held Session plus the socket-drop policy for it.
type residentSession struct {
	sess      Session
	keepAlive bool // set by an explicit close{keep_alive:true}; then a socket drop leaves it alive
}

type server struct {
	backends map[string]Backend
	tokens   *tokenStore

	mu       sync.Mutex
	sessions map[string]*residentSession // keyed by server handle; survives socket drops
}

// Serve binds addr and serves loom over websockets. tokenFile is REQUIRED (an empty
// or token-less file is refused — a daemon that runs agents with tool access must be
// gated). backends is injected so a caller passes Backends() and a test passes a stub.
func Serve(addr, tokenFile string, backends map[string]Backend) error {
	if tokenFile == "" {
		return fmt.Errorf("serve: --token-file is required (it gates who may drive this box)")
	}
	ts, err := loadTokenStore(tokenFile)
	if err != nil {
		return fmt.Errorf("serve: load tokens: %w", err)
	}
	if ts.count() == 0 {
		return fmt.Errorf("serve: token file %q has no tokens — mint one:\n  loom serve --token-file %q --add-token", tokenFile, tokenFile)
	}
	if host, _, e := net.SplitHostPort(addr); e == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		fmt.Fprintf(os.Stderr, "loom serve: WARNING binding to a wildcard address (%s) exposes this box to its whole network. A token gates access, but prefer a mesh IP (e.g. NetBird 100.x); there is no TLS yet — use only on an encrypted mesh.\n", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loom serve — listening on %s (%d token(s), backends: %v)\n", ln.Addr(), ts.count(), names(backends))
	return serveOn(ln, ts, backends)
}

// serveOn runs the accept loop on an already-bound listener. Split out from Serve so
// a test can bind 127.0.0.1:0 and dial the resulting address.
func serveOn(ln net.Listener, ts *tokenStore, backends map[string]Backend) error {
	s := &server{
		backends: backends,
		tokens:   ts,
		sessions: map[string]*residentSession{},
	}
	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := wsUpgrade(w, r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.handle(ws)
		}),
	}
	return httpSrv.Serve(ln)
}

func (s *server) backendNames() []string { return names(s.backends) }

// names is the sorted list of backend names (announced in hello, logged on start).
func names(backends map[string]Backend) []string {
	out := make([]string, 0, len(backends))
	for n := range backends {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// conn is one client connection: the socket, plus the set of sessions this
// connection opened (so a socket drop can clean up exactly the right ones).
type conn struct {
	srv    *server
	ws     *wsConn
	opened []string       // handles opened on this connection
	wg     sync.WaitGroup // in-flight send goroutines
}

// handle runs the whole lifecycle of one connection: hello → frame loop → cleanup.
func (s *server) handle(ws *wsConn) {
	c := &conn{srv: s, ws: ws}
	defer ws.Close()

	// 1. hello — the first frame must authenticate.
	var hello frame
	if err := ws.ReadJSON(&hello); err != nil {
		return
	}
	if hello.Op != opHello || !s.tokens.verify(hello.Token) {
		_ = ws.WriteJSON(frame{Op: opError, Err: "unauthorized"})
		return
	}
	if err := ws.WriteJSON(frame{Op: opHelloOK, OK: true, Server: serverName, Backends: s.backendNames()}); err != nil {
		return
	}

	// 2. frame loop. send runs in a goroutine (it blocks a whole turn) so the
	// reader stays free to deliver an interrupt mid-turn; open/interrupt/close are
	// quick and run inline.
	for {
		var f frame
		if err := ws.ReadJSON(&f); err != nil {
			break // socket dropped or closed
		}
		switch f.Op {
		case opOpen:
			c.handleOpen(f)
		case opSend:
			c.wg.Add(1)
			go func(f frame) { defer c.wg.Done(); c.handleSend(f) }(f)
		case opInterrupt:
			c.handleInterrupt(f)
		case opClose:
			c.handleClose(f)
		default:
			_ = ws.WriteJSON(frame{Op: opError, Err: "unknown op: " + f.Op})
		}
	}

	// 3. socket dropped. Close ONLY the sessions this connection opened that were
	// never marked keep_alive — a keep_alive session stays resident (recover it with
	// a fresh open{Resume:<real id>}). Closing non-keepalive sessions first unblocks
	// any in-flight turn on them, so the wait returns promptly.
	c.cleanup()
	c.wg.Wait()
}

// handleOpen opens the requested backend and registers a resident session under a
// fresh server handle. The handle addresses the session over the socket; claude's
// real session id (needed for --resume) rides back in each reply.
func (c *conn) handleOpen(f frame) {
	b, ok := c.srv.backends[f.Agent]
	if !ok {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "unknown backend: " + f.Agent})
		return
	}
	opts := SessionOpts{}
	if f.Opts != nil {
		opts = *f.Opts
	}
	sess, err := b.Open(context.Background(), opts)
	if err != nil {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: err.Error()})
		return
	}
	handle := newHandle()
	c.srv.mu.Lock()
	c.srv.sessions[handle] = &residentSession{sess: sess}
	c.srv.mu.Unlock()
	c.opened = append(c.opened, handle)
	_ = c.ws.WriteJSON(frame{Op: opOpened, SessionID: handle})
}

// handleSend runs one turn, streaming each Event as an event frame, then the final
// Reply as a reply frame. Even a turn that errored is delivered as a reply (with Err
// set) so the client's read loop terminates cleanly; opError is reserved for a
// protocol failure (e.g. no such session).
func (c *conn) handleSend(f frame) {
	rs := c.srv.lookup(f.SessionID)
	if rs == nil {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "no such session: " + f.SessionID})
		return
	}
	onEvent := func(ev Event) {
		e := ev
		_ = c.ws.WriteJSON(frame{Op: opEvent, Event: &e})
	}
	r, err := rs.sess.SendStream(context.Background(), f.Text, onEvent)
	if err != nil && r.Err == "" {
		r.Err = err.Error()
	}
	rr := r
	_ = c.ws.WriteJSON(frame{Op: opReply, Reply: &rr})
}

// handleInterrupt stops the in-flight turn on a session (if it is Interruptible).
// The ack has its own op so the client's send read loop can ignore it — an interrupt
// is fire-and-forget on the client, mirroring the live subprocess path in claude.go.
func (c *conn) handleInterrupt(f frame) {
	rs := c.srv.lookup(f.SessionID)
	if rs == nil {
		_ = c.ws.WriteJSON(frame{Op: opInterruptAck, Err: "no such session: " + f.SessionID})
		return
	}
	it, ok := rs.sess.(Interruptible)
	if !ok {
		_ = c.ws.WriteJSON(frame{Op: opInterruptAck, Err: "session not interruptible"})
		return
	}
	if err := it.Interrupt(); err != nil {
		_ = c.ws.WriteJSON(frame{Op: opInterruptAck, Err: err.Error()})
		return
	}
	_ = c.ws.WriteJSON(frame{Op: opInterruptAck, OK: true})
}

// handleClose either drops the session's process now (keep_alive:false) or leaves it
// resident (keep_alive:true) so a dropped socket won't reap it.
func (c *conn) handleClose(f frame) {
	if f.KeepAlive {
		c.srv.mu.Lock()
		if rs := c.srv.sessions[f.SessionID]; rs != nil {
			rs.keepAlive = true
		}
		c.srv.mu.Unlock()
		_ = c.ws.WriteJSON(frame{Op: opClosed, OK: true, SessionID: f.SessionID})
		return
	}
	c.srv.mu.Lock()
	rs := c.srv.sessions[f.SessionID]
	delete(c.srv.sessions, f.SessionID)
	c.srv.mu.Unlock()
	if rs != nil {
		_ = rs.sess.Close()
	}
	_ = c.ws.WriteJSON(frame{Op: opClosed, OK: true, SessionID: f.SessionID})
}

// cleanup closes the non-keepalive sessions this connection opened, on socket drop.
func (c *conn) cleanup() {
	for _, handle := range c.opened {
		c.srv.mu.Lock()
		rs := c.srv.sessions[handle]
		if rs != nil && !rs.keepAlive {
			delete(c.srv.sessions, handle)
			c.srv.mu.Unlock()
			_ = rs.sess.Close()
		} else {
			c.srv.mu.Unlock()
		}
	}
}

func (s *server) lookup(handle string) *residentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[handle]
}

// newHandle mints a random server-side session handle. It is distinct from the
// backend's own session id (which may be empty until the first turn); the backend id
// travels in replies for --resume.
func newHandle() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ws-" + hex.EncodeToString(b[:])
}
