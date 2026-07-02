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
)

// ConnectBackend dials a `loom serve` endpoint. Agent is the backend name the SERVER
// should open (claude/agy/local) — carried in the open frame so the server opens the
// right one; ConnectBackend.Name() itself is always "connect" (the transport).
type ConnectBackend struct {
	URL   string // ws://host:port
	Token string // auth token the server verifies
	Agent string // backend to open on the server ("" → claude)
}

// Compile-time proof that the ws transport honors the same interfaces as a local
// backend — so run/chat/review drive it identically, and interrupt works over the wire.
var (
	_ Backend       = ConnectBackend{}
	_ Session       = (*connectSession)(nil)
	_ Interruptible = (*connectSession)(nil)
)

func (b ConnectBackend) Name() string { return "connect" }

// Open dials, authenticates (hello), and opens a remote session (open). It returns a
// connectSession bound to the server-side handle; the socket stays open for the life
// of the session (every turn is a frame exchange on it).
func (b ConnectBackend) Open(ctx context.Context, opts SessionOpts) (Session, error) {
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

	agent := b.Agent
	if agent == "" {
		agent = "claude"
	}
	o := opts
	if err := ws.WriteJSON(frame{Op: opOpen, Agent: agent, Opts: &o}); err != nil {
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
	return &connectSession{ws: ws, handle: op.SessionID}, nil
}

// connectSession is a remote session reached over the socket. handle addresses it on
// the server; realID is the backend's own session id learned from replies (what
// SessionID reports, so a --resume hint uses the resumable id, not the handle).
type connectSession struct {
	ws     *wsConn
	handle string

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
			if r.SessionID != "" {
				s.mu.Lock()
				s.realID = r.SessionID
				s.mu.Unlock()
			}
			return r, nil
		case opError:
			return Reply{Backend: "connect", Err: f.Err}, fmt.Errorf("connect: %s", f.Err)
			// default: interrupt_ack / closed / anything else mid-turn → ignore
		}
	}
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

// Close asks the server to drop the remote process (keep_alive:false), then closes
// the socket. The backend session is persisted where it runs, so it is still
// --resume-able by its real id after this — closing frees the live process, not the
// history.
func (s *connectSession) Close() error {
	_ = s.ws.WriteJSON(frame{Op: opClose, SessionID: s.handle})
	return s.ws.Close()
}
