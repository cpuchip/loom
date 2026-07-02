package loom

// ConnectBackend is loom's fourth transport (direct | docker | ssh | ws): it drives
// a remote `loom serve` over a websocket. It implements Backend, and its session
// implements Session + Interruptible, so run/chat/review use it exactly like a local
// backend — the socket replaces the subprocess/ssh pipe under the same interface.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ConnectBackend dials a `loom serve` endpoint. Agent is the backend name the SERVER
// should open (claude/agy/local) — carried in the open frame so the server opens the
// right one; ConnectBackend.Name() itself is always "connect" (the transport).
//
// SessionName reattaches (or first-opens) a warm resident by a stable name: a second
// open of the same name reuses the live warm process instead of respawning. AttachOnly
// makes an open reattach-or-fail (never spawn) — used by `await`, which must not create
// a session just to poll one.
type ConnectBackend struct {
	URL         string // ws://host:port
	Token       string // auth token the server verifies
	Agent       string // backend to open on the server ("" → claude)
	SessionName string // stable name for a warm resident ("" → ephemeral, closed on disconnect)
	AttachOnly  bool   // reattach a live resident by name, else error — never spawn
}

// Compile-time proof that the ws transport honors the same interfaces as a local
// backend — so run/chat/review drive it identically — plus DetachSession for the
// warm-resident detach/await ergonomics.
var (
	_ Backend       = ConnectBackend{}
	_ Session       = (*connectSession)(nil)
	_ Interruptible = (*connectSession)(nil)
	_ DetachSession = (*connectSession)(nil)
)

// DetachSession is an optional capability of a session reached over `loom serve`: fire
// a long turn detached (return immediately with a turn-id) and fetch its buffered reply
// later with Await — so a minutes-long verify need not pin a synchronous client, and a
// dropped socket loses no verdict. Callers type-assert for it, like Interruptible.
type DetachSession interface {
	// SendDetached starts a turn and returns its server-assigned turn-id at once.
	SendDetached(ctx context.Context, text string) (turnID int64, err error)
	// Await fetches a turn's reply, blocking up to timeout. It reports running=true if
	// the turn is still in flight when timeout elapses (poll again). lastReply fetches
	// the most recent turn without a turn-id (recovery for a dropped non-detached turn).
	Await(ctx context.Context, turnID int64, lastReply bool, timeout time.Duration) (reply Reply, running bool, err error)
}

func (b ConnectBackend) Name() string { return "connect" }

// dialHello dials the endpoint and completes the authenticated handshake, returning the
// live socket. Shared by Open and the session-less ops (Sessions).
func (b ConnectBackend) dialHello() (*wsConn, error) {
	ws, err := wsDial(b.URL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("connect: dial %s: %w", b.URL, err)
	}
	if err := ws.WriteJSON(frame{Op: opHello, Token: b.Token, Client: "loom"}); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: hello: %w", err)
	}
	var hi frame
	if err := ws.ReadJSON(&hi); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: hello: %w", err)
	}
	if hi.Op == opError {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: %s", hi.Err)
	}
	if hi.Op != opHelloOK || !hi.OK {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: handshake refused by server")
	}
	return ws, nil
}

// Open dials, authenticates (hello), and opens a remote session (open). With a
// SessionName it reattaches a live warm resident if one exists (no respawn, no
// cold-read); else it first-opens one. It returns a connectSession bound to the
// server-side handle; the socket stays open for the life of the session (every turn is
// a frame exchange on it).
func (b ConnectBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
	ws, err := b.dialHello()
	if err != nil {
		return nil, err
	}

	agent := b.Agent
	if agent == "" {
		agent = "claude"
	}
	o := opts
	if err := ws.WriteJSON(frame{Op: opOpen, Agent: agent, Opts: &o, SessionName: b.SessionName, AttachOnly: b.AttachOnly}); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: open: %w", err)
	}
	var op frame
	if err := ws.ReadJSON(&op); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: open: %w", err)
	}
	if op.Op == opError {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: open: %s", op.Err)
	}
	if op.Op != opOpened || op.SessionID == "" {
		_ = ws.Close()
		return nil, fmt.Errorf("connect: open: unexpected reply %q", op.Op)
	}
	return &connectSession{ws: ws, handle: op.SessionID, named: b.SessionName != ""}, nil
}

// Sessions lists the residents held by the server (session-less: it only needs the
// hello handshake). Feeds the `loom sessions` command + reattach UX.
func (b ConnectBackend) Sessions(ctx context.Context) ([]SessionInfo, error) {
	ws, err := b.dialHello()
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	if err := ws.WriteJSON(frame{Op: opSessions}); err != nil {
		return nil, fmt.Errorf("connect: sessions: %w", err)
	}
	var f frame
	if err := ws.ReadJSON(&f); err != nil {
		return nil, fmt.Errorf("connect: sessions: %w", err)
	}
	if f.Op == opError {
		return nil, fmt.Errorf("connect: %s", f.Err)
	}
	return f.Sessions, nil
}

// connectSession is a remote session reached over the socket. handle addresses it on
// the server; realID is the backend's own session id learned from replies (what
// SessionID reports, so a --resume hint uses the resumable id, not the handle). named
// marks a warm resident (so Close leaves it resident instead of dropping the process).
type connectSession struct {
	ws     *wsConn
	handle string
	named  bool

	turnMu sync.Mutex // one turn at a time, mirroring claudeSession.turnMu

	mu     sync.Mutex // guards realID
	realID string
}

func (s *connectSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

// SendStream sends one turn and reads frames until the reply: each event frame is
// delivered to onEvent, the reply frame ends the turn. Any other frame arriving
// mid-turn (e.g. an interrupt_ack, or a stray closed) is ignored — only reply/error
// terminate the read, so a fire-and-forget interrupt never derails the stream.
func (s *connectSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	if err := s.ws.WriteJSON(frame{Op: opSend, SessionID: s.handle, Text: prompt}); err != nil {
		return Reply{Backend: "connect", Err: err.Error()}, err
	}
	for {
		var f frame
		if err := s.ws.ReadJSON(&f); err != nil {
			return Reply{Backend: "connect", Err: err.Error()}, err
		}
		switch f.Op {
		case opEvent:
			if f.Event != nil {
				emit(onEvent, *f.Event)
			}
		case opReply:
			r := Reply{Backend: "connect"}
			if f.Reply != nil {
				r = *f.Reply
			}
			s.rememberID(r.SessionID)
			return r, nil
		case opError:
			return Reply{Backend: "connect", Err: f.Err}, fmt.Errorf("connect: %s", f.Err)
			// default: interrupt_ack / closed / anything else mid-turn → ignore
		}
	}
}

// SendDetached starts a turn detached: it writes the send frame with detach set and
// returns as soon as the server acks with the turn-id (the turn keeps running server-
// side). Fetch the reply later with Await.
func (s *connectSession) SendDetached(ctx context.Context, text string) (int64, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if err := s.ws.WriteJSON(frame{Op: opSend, SessionID: s.handle, Text: text, Detach: true}); err != nil {
		return 0, err
	}
	for {
		var f frame
		if err := s.ws.ReadJSON(&f); err != nil {
			return 0, err
		}
		switch f.Op {
		case opSent:
			return f.TurnID, nil
		case opError:
			return 0, fmt.Errorf("connect: %s", f.Err)
			// default: ignore any stray frame
		}
	}
}

// Await fetches a turn's buffered reply, blocking up to timeout. running=true means the
// turn was still in flight when the (server-bounded) timeout elapsed — poll again.
func (s *connectSession) Await(ctx context.Context, turnID int64, lastReply bool, timeout time.Duration) (Reply, bool, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	f := frame{Op: opAwait, SessionID: s.handle, TurnID: turnID, LastReply: lastReply}
	if timeout > 0 {
		f.TimeoutMS = int(timeout / time.Millisecond)
	}
	if err := s.ws.WriteJSON(f); err != nil {
		return Reply{Backend: "connect", Err: err.Error()}, false, err
	}
	for {
		var fr frame
		if err := s.ws.ReadJSON(&fr); err != nil {
			return Reply{Backend: "connect", Err: err.Error()}, false, err
		}
		switch fr.Op {
		case opReply:
			r := Reply{Backend: "connect"}
			if fr.Reply != nil {
				r = *fr.Reply
			}
			s.rememberID(r.SessionID)
			return r, false, nil
		case opSent: // still running after the server-side timeout
			return Reply{Backend: "connect"}, true, nil
		case opError:
			return Reply{Backend: "connect", Err: fr.Err}, false, fmt.Errorf("connect: %s", fr.Err)
			// default: ignore any stray frame
		}
	}
}

func (s *connectSession) rememberID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.realID = id
	s.mu.Unlock()
}

// Interrupt writes an interrupt frame and returns — fire-and-forget, exactly like
// claudeSession.Interrupt. The write goes out under the wsConn write mutex, so it is
// safe to call concurrently with SendStream (whose read loop holds no write lock);
// the server's ack is swallowed by that read loop.
func (s *connectSession) Interrupt() error {
	return s.ws.WriteJSON(frame{Op: opInterrupt, SessionID: s.handle})
}

// SessionID returns the backend's resumable session id once a turn has revealed it,
// else the server handle. (claudeSession behaves the same: its id is empty until the
// first result event.)
func (s *connectSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.realID != "" {
		return s.realID
	}
	return s.handle
}

// Close releases the socket. For an ephemeral session it asks the server to drop the
// remote process (keep_alive:false); for a NAMED (warm-resident) session it asks the
// server to keep the process resident (keep_alive:true), so the next open of the name
// reattaches warm. Either way the backend session is persisted where it runs, so it is
// still --resume-able by its real id.
func (s *connectSession) Close() error {
	_ = s.ws.WriteJSON(frame{Op: opClose, SessionID: s.handle, KeepAlive: s.named})
	return s.ws.Close()
}
