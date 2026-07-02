package loom

// loom serve — loom as a websocket service. A long-running server exposes loom's
// existing Backend/Session interface over a socket, so a client (another loom, or a
// browser) drives sessions with a token instead of spawning subprocesses/ssh. The
// server drives REAL backends through the same Backend/Session/Interruptible
// interfaces, so run/chat/review work unchanged over the wire. Design + rationale:
// docs/proposals/loom-serve.md and docs/proposals/loom-serve-warm-resident.md.
//
// Warm-resident upgrade: a session opened under a client-chosen NAME stays resident
// and warm across socket drops; a later open of the same name REATTACHES to the live
// process instead of respawning + cold-reading the whole history. Long turns can run
// DETACHED (return a turn-id immediately, fetch the reply later with await). A per-turn
// reply ring makes a mid-turn socket drop recoverable. An idle TTL downgrades a stale
// resident to cold-resumable (closed, its lineage id remembered) — one cold-read, never
// lost data.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// serverName is announced in the hello reply.
const serverName = "loom"

// replyRingSize is how many recent turn replies a resident buffers by turn-id, so a
// client whose socket dropped mid-turn can reconnect and fetch the verdict.
const replyRingSize = 8

// awaitDefault / awaitMax bound how long a single await blocks server-side before it
// returns status:"running" so the client polls again (a client can pass a shorter one).
const (
	awaitDefault = 30 * time.Second
	awaitMax     = 120 * time.Second
)

// The wire protocol: JSON frames, one per websocket message. A single flat envelope
// carries every op's fields (omitempty keeps each frame small). op is the
// discriminator.
//
//	c→s: hello, open, send, await, interrupt, close, sessions
//	s→c: hello_ok, opened, event, reply, sent, interrupt_ack, closed, sessions_ok, error
const (
	opHello        = "hello"
	opHelloOK      = "hello_ok"
	opOpen         = "open"
	opOpened       = "opened"
	opSend         = "send"
	opSent         = "sent" // detach ack / "still running" after an await timeout: carries turn_id + status
	opAwait        = "await"
	opEvent        = "event"
	opReply        = "reply"
	opInterrupt    = "interrupt"
	opInterruptAck = "interrupt_ack" // its own op so a mid-turn ack never looks like a send terminal
	opClose        = "close"
	opClosed       = "closed"
	opSessions     = "sessions"    // c→s: list residents
	opSessionsOK   = "sessions_ok" // s→c: the resident list
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

	// open (c→s): the stable client-chosen name for a warm resident. A second open of
	// the same name reattaches to the live process instead of spawning a new one.
	SessionName string `json:"session_name,omitempty"`
	// open (c→s): reattach if resident, else error — never spawn (used by await/attach).
	AttachOnly bool `json:"attach_only,omitempty"`

	// session addressing (opened/send/interrupt/close/…): the server-side handle
	SessionID string `json:"session_id,omitempty"`

	// opened (s→c): whether this open reattached a live resident, and any note (e.g.
	// requested opts ignored because they froze at first open).
	Reattached bool   `json:"reattached,omitempty"`
	Note       string `json:"note,omitempty"`

	// send (c→s)
	Text   string `json:"text,omitempty"`
	Detach bool   `json:"detach,omitempty"` // start the turn detached; reply goes to the ring, not the socket

	// sent (s→c) / await (c→s): the turn identity + status
	TurnID    int64  `json:"turn_id,omitempty"`
	Status    string `json:"status,omitempty"` // "running" (turn not yet complete)
	LastReply bool   `json:"last_reply,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`

	// close (c→s): leave the process resident vs. drop it now
	KeepAlive bool `json:"keep_alive,omitempty"`

	// event/reply (s→c)
	Event *Event `json:"event,omitempty"`
	Reply *Reply `json:"reply,omitempty"`

	// sessions_ok (s→c)
	Sessions []SessionInfo `json:"sessions,omitempty"`

	// error / interrupt_ack (s→c)
	Err string `json:"error,omitempty"`
}

// SessionInfo is one resident session as reported by `loom sessions` — enough to pick
// a name to reattach, to see what opts froze at open, and to feed the idle janitor.
type SessionInfo struct {
	Name           string `json:"name,omitempty"`
	Handle         string `json:"handle"`
	Backend        string `json:"backend"`
	Model          string `json:"model,omitempty"`
	Dir            string `json:"dir,omitempty"`
	AllowedTools   string `json:"allowed_tools,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	KeepAlive      bool   `json:"keep_alive,omitempty"`
	IdleSeconds    int    `json:"idle_seconds"`
	LastTurnID     int64  `json:"last_turn_id"`
}

// turnRecord is one turn's buffered result. doneCh closes when the turn completes, so
// an await can block on it; reply is filled (under the resident's mu) before the close.
type turnRecord struct {
	id     int64
	done   bool
	reply  Reply
	doneCh chan struct{}
}

// residentSession is a live server-held Session plus its warm-reattach identity, the
// per-turn reply ring, and the idle bookkeeping. The immutable identity fields (name,
// agent, opts, handle) are set once at open; the mutable fields are guarded by mu,
// EXCEPT keepAlive, which is set/read only under the server's mu.
type residentSession struct {
	sess   Session
	name   string      // "" = ephemeral (no reattach); stable client-chosen key otherwise
	agent  string      // backend name it was opened on (for sessions listing + resume memory)
	opts   SessionOpts // opts frozen at open (permission-mode/allowed-tools/model/dir…)
	handle string      // server handle addressing it over the socket

	keepAlive bool // set by close{keep_alive:true}; guarded by server.mu

	mu         sync.Mutex // guards the mutable state below
	nextTurnID int64
	lastTurnID int64
	ring       []*turnRecord // last replyRingSize turns, oldest first
	lastActive time.Time
	running    int  // in-flight turns (the reaper never downgrades a resident mid-turn)
	closed     bool // downgraded/destroyed; no new turn may start
}

// rememberedSession is a name whose live process was downgraded (idle TTL): the next
// open of that name cold-resumes its evolved lineage id, so no context is lost.
type rememberedSession struct {
	resumeID string
	agent    string
	opts     SessionOpts
}

type server struct {
	backends map[string]Backend
	tokens   *tokenStore

	idleTTL time.Duration // 0 = never downgrade on idle

	// openMu serializes opens so the reattach-or-spawn check-and-register is atomic
	// even across concurrent clients (the two-writer fence). Opens are rare and a
	// spawn is seconds; sends/awaits never take this lock.
	openMu sync.Mutex

	mu         sync.Mutex
	sessions   map[string]*residentSession  // keyed by server handle; survives socket drops
	byName     map[string]*residentSession  // keyed by client SessionName (named residents)
	remembered map[string]rememberedSession // name → lineage id remembered after a TTL downgrade
}

// Serve binds addr and serves loom over websockets. tokenFile is REQUIRED (an empty
// or token-less file is refused — a daemon that runs agents with tool access must be
// gated). backends is injected so a caller passes Backends() and a test passes a stub.
// idleTTL downgrades a resident idle longer than it to cold-resumable (0 = never).
func Serve(addr, tokenFile string, backends map[string]Backend, idleTTL time.Duration) error {
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
	fmt.Fprintf(os.Stderr, "loom serve — listening on %s (%d token(s), backends: %v, idle-ttl: %s)\n", ln.Addr(), ts.count(), names(backends), idleTTL)
	return serveOn(ln, ts, backends, idleTTL)
}

// newServer builds the server state. Split out so a test can hold the *server (to
// drive the reaper deterministically) instead of only its address.
func newServer(ts *tokenStore, backends map[string]Backend, idleTTL time.Duration) *server {
	return &server{
		backends:   backends,
		tokens:     ts,
		idleTTL:    idleTTL,
		sessions:   map[string]*residentSession{},
		byName:     map[string]*residentSession{},
		remembered: map[string]rememberedSession{},
	}
}

// serveOn runs the accept loop on an already-bound listener. Split out from Serve so
// a test can bind 127.0.0.1:0 and dial the resulting address.
func serveOn(ln net.Listener, ts *tokenStore, backends map[string]Backend, idleTTL time.Duration) error {
	return newServer(ts, backends, idleTTL).serve(ln)
}

func (s *server) serve(ln net.Listener) error {
	if s.idleTTL > 0 {
		go s.reapLoop()
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
	wg     sync.WaitGroup // in-flight (non-detached) send goroutines
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

	// 2. frame loop. send + await run in goroutines (each blocks — a whole turn, or up
	// to the await timeout) so the reader stays free to deliver an interrupt mid-turn;
	// open/interrupt/close/sessions are quick and run inline.
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
		case opAwait:
			c.wg.Add(1)
			go func(f frame) { defer c.wg.Done(); c.handleAwait(f) }(f)
		case opInterrupt:
			c.handleInterrupt(f)
		case opClose:
			c.handleClose(f)
		case opSessions:
			c.handleSessions(f)
		default:
			_ = ws.WriteJSON(frame{Op: opError, Err: "unknown op: " + f.Op})
		}
	}

	// 3. socket dropped. Close ONLY the ephemeral sessions this connection opened that
	// were never marked keep_alive — a keep_alive OR NAMED session stays resident
	// (a named resident is the warm-reattach target; recover it with a fresh open of
	// the name). Closing the ephemeral ones first unblocks any in-flight turn on them,
	// so the wait returns promptly.
	c.cleanup()
	c.wg.Wait()
}

// handleOpen reattaches to a live resident by name, else cold-opens the requested
// backend and registers a new resident. The whole method holds openMu so the
// check-and-register is atomic across concurrent clients (the two-writer fence: two
// processes appending one history would silently diverge).
func (c *conn) handleOpen(f frame) {
	c.srv.openMu.Lock()
	defer c.srv.openMu.Unlock()

	// 1. reattach a live resident by name — never spawn a second process.
	if f.SessionName != "" {
		c.srv.mu.Lock()
		rs := c.srv.byName[f.SessionName]
		c.srv.mu.Unlock()
		if rs != nil {
			rs.touch()
			c.opened = append(c.opened, rs.handle)
			_ = c.ws.WriteJSON(frame{Op: opOpened, SessionID: rs.handle, Reattached: true, Note: reattachNote(rs, f)})
			return
		}
	}

	// attach_only never spawns: if there was no live resident to reattach, it is an error.
	if f.AttachOnly {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "no resident session named " + strconvName(f.SessionName)})
		return
	}

	// 2. resolve the backend + opts for a cold open.
	agent := f.Agent
	opts := SessionOpts{}
	if f.Opts != nil {
		opts = *f.Opts
	}

	// 3. a name downgraded by the idle TTL cold-resumes its evolved lineage (opts + agent
	// froze at first open; the remembered id is newer than any client-supplied Resume).
	if f.SessionName != "" {
		c.srv.mu.Lock()
		if rem, ok := c.srv.remembered[f.SessionName]; ok {
			agent = rem.agent
			opts = rem.opts
			opts.Resume = rem.resumeID
			delete(c.srv.remembered, f.SessionName)
		}
		c.srv.mu.Unlock()
	}

	b, ok := c.srv.backends[agent]
	if !ok {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "unknown backend: " + agent})
		return
	}
	sess, err := b.Open(context.Background(), opts)
	if err != nil {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: err.Error()})
		return
	}
	handle := newHandle()
	rs := &residentSession{sess: sess, name: f.SessionName, agent: agent, opts: opts, handle: handle, lastActive: time.Now()}
	c.srv.mu.Lock()
	c.srv.sessions[handle] = rs
	if f.SessionName != "" {
		c.srv.byName[f.SessionName] = rs
	}
	c.srv.mu.Unlock()
	c.opened = append(c.opened, handle)
	_ = c.ws.WriteJSON(frame{Op: opOpened, SessionID: handle})
}

// handleSend runs one turn. A detached send returns a turn-id immediately and runs the
// turn in a resident-owned goroutine (its reply lands in the ring, recoverable by
// await even across a socket drop). A non-detached send keeps the streaming behavior
// (each Event as an event frame, then the Reply) AND buffers the reply in the ring —
// so a mid-turn drop still leaves the verdict fetchable with `await --last-reply`.
func (c *conn) handleSend(f frame) {
	rs := c.srv.lookup(f.SessionID)
	if rs == nil {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "no such session: " + f.SessionID})
		return
	}
	rec, ok := rs.beginTurn()
	if !ok {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "session downgraded — reopen by name: " + f.SessionID})
		return
	}
	if f.Detach {
		// The turn outlives this connection: it is NOT tracked by c.wg, so a socket
		// drop won't wait on (or cancel) a minutes-long verify. context.Background()
		// keeps it bound to the resident, not the request.
		go rs.runTurn(rec, f.Text, nil)
		_ = c.ws.WriteJSON(frame{Op: opSent, SessionID: f.SessionID, TurnID: rec.id, Status: "running"})
		return
	}
	// Non-detached: block this connection's caller, streaming the work. This runs in
	// the frame loop's send goroutine (tracked by c.wg), so cleanup waits for it and
	// the ring still captures the reply if the socket dies mid-stream.
	onEvent := func(ev Event) {
		e := ev
		_ = c.ws.WriteJSON(frame{Op: opEvent, Event: &e})
	}
	r := rs.runTurn(rec, f.Text, onEvent)
	rr := r
	_ = c.ws.WriteJSON(frame{Op: opReply, Reply: &rr, TurnID: rec.id})
}

// handleAwait fetches a detached (or dropped) turn's reply from the ring. It blocks
// server-side until the turn completes or the (bounded) timeout elapses; on timeout it
// returns sent{status:"running"} so the client polls again. --last-reply fetches the
// most recent turn without knowing its id (the recovery path for a dropped non-detached
// turn, whose id the client never learned).
func (c *conn) handleAwait(f frame) {
	rs := c.srv.lookup(f.SessionID)
	if rs == nil {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "no such session: " + f.SessionID})
		return
	}
	rec := rs.recordFor(f.TurnID, f.LastReply)
	if rec == nil {
		_ = c.ws.WriteJSON(frame{Op: opError, Err: "no such turn on this session"})
		return
	}
	timeout := awaitDefault
	if f.TimeoutMS > 0 {
		timeout = time.Duration(f.TimeoutMS) * time.Millisecond
	}
	if timeout > awaitMax {
		timeout = awaitMax
	}
	select {
	case <-rec.doneCh:
		rs.mu.Lock()
		reply := rec.reply
		rs.mu.Unlock()
		rr := reply
		_ = c.ws.WriteJSON(frame{Op: opReply, Reply: &rr, TurnID: rec.id})
	case <-time.After(timeout):
		_ = c.ws.WriteJSON(frame{Op: opSent, SessionID: f.SessionID, TurnID: rec.id, Status: "running"})
	}
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
// resident (keep_alive:true) so a dropped socket won't reap it. Destroying a named
// resident also clears its name index + any remembered lineage.
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
	if rs != nil && rs.name != "" {
		delete(c.srv.byName, rs.name)
		delete(c.srv.remembered, rs.name)
	}
	c.srv.mu.Unlock()
	if rs != nil {
		_ = rs.sess.Close()
	}
	_ = c.ws.WriteJSON(frame{Op: opClosed, OK: true, SessionID: f.SessionID})
}

// handleSessions lists the live residents (name, backend, frozen opts, idle, last turn).
func (c *conn) handleSessions(f frame) {
	now := time.Now()
	c.srv.mu.Lock()
	infos := make([]SessionInfo, 0, len(c.srv.sessions))
	for _, rs := range c.srv.sessions {
		infos = append(infos, rs.info(now))
	}
	c.srv.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Name != infos[j].Name {
			return infos[i].Name < infos[j].Name
		}
		return infos[i].Handle < infos[j].Handle
	})
	_ = c.ws.WriteJSON(frame{Op: opSessionsOK, Sessions: infos})
}

// cleanup closes the ephemeral, non-keepalive sessions this connection opened, on a
// socket drop. A named resident (warm-reattach target) or a keep_alive one is left
// resident for a later reattach.
func (c *conn) cleanup() {
	for _, handle := range c.opened {
		c.srv.mu.Lock()
		rs := c.srv.sessions[handle]
		if rs != nil && !rs.keepAlive && rs.name == "" {
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

// --- resident turn/ring/idle bookkeeping (all under rs.mu) ---

// beginTurn reserves a monotonic turn id + a pending ring record, or reports !ok if the
// resident has been downgraded/destroyed (so no new turn starts on a dead process).
func (rs *residentSession) beginTurn() (*turnRecord, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.closed {
		return nil, false
	}
	rs.nextTurnID++
	rec := &turnRecord{id: rs.nextTurnID, doneCh: make(chan struct{})}
	rs.ring = append(rs.ring, rec)
	if len(rs.ring) > replyRingSize {
		rs.ring = rs.ring[len(rs.ring)-replyRingSize:]
	}
	rs.running++
	rs.lastActive = time.Now()
	return rec, true
}

// runTurn executes one turn and records its reply. It holds no lock while the backend
// works (the backend's own turnMu serializes concurrent turns over the shared stdio);
// it locks rs.mu only to publish the result, then closes doneCh to wake any awaiter.
func (rs *residentSession) runTurn(rec *turnRecord, text string, onEvent func(Event)) Reply {
	r, err := rs.sess.SendStream(context.Background(), text, onEvent)
	if err != nil && r.Err == "" {
		r.Err = err.Error()
	}
	rs.mu.Lock()
	rec.reply = r
	rec.done = true
	rs.running--
	rs.lastActive = time.Now()
	rs.lastTurnID = rec.id
	rs.mu.Unlock()
	close(rec.doneCh) // wake awaiters; the reply is already published under mu above
	return r
}

// recordFor finds a buffered turn by id, or the most recent turn for --last-reply.
func (rs *residentSession) recordFor(id int64, last bool) *turnRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if last {
		if len(rs.ring) == 0 {
			return nil
		}
		return rs.ring[len(rs.ring)-1]
	}
	for _, rec := range rs.ring {
		if rec.id == id {
			return rec
		}
	}
	return nil
}

func (rs *residentSession) touch() {
	rs.mu.Lock()
	rs.lastActive = time.Now()
	rs.mu.Unlock()
}

// info snapshots the resident for a sessions listing. keepAlive is read without rs.mu
// because the caller (handleSessions) already holds server.mu, which guards it.
func (rs *residentSession) info(now time.Time) SessionInfo {
	rs.mu.Lock()
	idle := int(now.Sub(rs.lastActive).Seconds())
	last := rs.lastTurnID
	rs.mu.Unlock()
	if idle < 0 {
		idle = 0
	}
	return SessionInfo{
		Name:           rs.name,
		Handle:         rs.handle,
		Backend:        rs.agent,
		Model:          rs.opts.Model,
		Dir:            rs.opts.Workdir,
		AllowedTools:   rs.opts.AllowedTools,
		PermissionMode: rs.opts.PermissionMode,
		KeepAlive:      rs.keepAlive,
		IdleSeconds:    idle,
		LastTurnID:     last,
	}
}

// --- idle TTL reaper ---

// reapLoop runs the idle janitor on a ticker until process exit. It is only started
// when idleTTL > 0.
func (s *server) reapLoop() {
	iv := s.idleTTL
	if iv > time.Minute {
		iv = time.Minute
	}
	if iv < time.Second {
		iv = time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for range t.C {
		s.reapOnce(time.Now())
	}
}

// reapOnce downgrades every named resident idle longer than idleTTL. Downgrade closes
// the live process but remembers its evolved lineage id, so the next open of the name
// cold-resumes it — one cold-read, never lost data.
func (s *server) reapOnce(now time.Time) {
	s.mu.Lock()
	cands := make([]*residentSession, 0, len(s.byName))
	for _, rs := range s.byName {
		cands = append(cands, rs)
	}
	s.mu.Unlock()
	for _, rs := range cands {
		s.downgrade(rs, now)
	}
}

// downgrade closes an idle resident and remembers its lineage. It never downgrades a
// resident with an in-flight turn, and it takes rs.mu and server.mu one at a time
// (rs.mu released before server.mu) so it can't deadlock against the s.mu→rs.mu order
// used elsewhere.
func (s *server) downgrade(rs *residentSession, now time.Time) {
	rs.mu.Lock()
	if rs.closed || rs.running > 0 || now.Sub(rs.lastActive) <= s.idleTTL {
		rs.mu.Unlock()
		return
	}
	rs.closed = true
	rs.mu.Unlock()

	resumeID := rs.sess.SessionID() // the evolved, resumable lineage id (own mutex)

	s.mu.Lock()
	delete(s.sessions, rs.handle)
	if rs.name != "" {
		delete(s.byName, rs.name)
		s.remembered[rs.name] = rememberedSession{resumeID: resumeID, agent: rs.agent, opts: rs.opts}
	}
	s.mu.Unlock()

	_ = rs.sess.Close()
}

// reattachNote surfaces opts a reattaching client asked for that were IGNORED because
// they froze at first open (a mis-provisioned resident must be cold-killed to change).
func reattachNote(rs *residentSession, f frame) string {
	if f.Opts == nil {
		return ""
	}
	var diffs []string
	if f.Opts.Model != "" && f.Opts.Model != rs.opts.Model {
		diffs = append(diffs, "model")
	}
	if f.Opts.PermissionMode != "" && f.Opts.PermissionMode != rs.opts.PermissionMode {
		diffs = append(diffs, "permission-mode")
	}
	if f.Opts.AllowedTools != "" && f.Opts.AllowedTools != rs.opts.AllowedTools {
		diffs = append(diffs, "allowed-tools")
	}
	if f.Opts.Workdir != "" && f.Opts.Workdir != rs.opts.Workdir {
		diffs = append(diffs, "dir")
	}
	if len(diffs) == 0 {
		return ""
	}
	return "reattached to live resident; requested " + strings.Join(diffs, ",") + " ignored (opts froze at first open)"
}

// strconvName renders an empty name readably in an error.
func strconvName(name string) string {
	if name == "" {
		return "(unnamed)"
	}
	return name
}

// newHandle mints a random server-side session handle. It is distinct from the
// backend's own session id (which may be empty until the first turn); the backend id
// travels in replies for --resume.
func newHandle() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ws-" + hex.EncodeToString(b[:])
}
