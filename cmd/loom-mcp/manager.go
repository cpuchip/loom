package main

// The commission manager: loom-mcp's process-wide registry of sessions it has
// commissioned on a loom serve, and the state machine that governs them —
// read-only advisory sessions open immediately; WRITABLE sessions wait behind a
// tap-to-approve gate; every session is cancelable (e-stopped) from here.
//
// The registry is the source of truth for "commissioned sessions": there is one
// manager per loom-mcp process, and only the companion's MCP config wires in
// loom-mcp, so session_close over ANY handle in this registry is the whole
// "cancel any commissioned session" surface.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cpuchip/loom"
)

// sessState is where a commission sits in its lifecycle.
type sessState string

const (
	stateAwaiting sessState = "awaiting_approval" // writable, gate enqueued, tap pending
	stateOpen     sessState = "open"              // a live loom session backs it
	stateDeclined sessState = "declined"          // the tap was declined or timed out — never opened
	stateClosed   sessState = "closed"            // e-stopped / withdrawn
	stateError    sessState = "error"             // open failed
)

// opener talks to a loom serve. Real impl wraps loom.ConnectBackend; a test
// injects a fake so the state machine + cap + gate logic run with no serve.
type opener interface {
	open(ctx context.Context, backend string, opts loom.SessionOpts) (loom.Session, error)
}

// gate is the substrate tool-confirm tap gate. enqueue creates the card row and
// returns its hinge id; status reports the row's lifecycle; withdraw declines a
// pending row (the safe direction — a decline never executes; only Michael's
// approve does, which is why loom-mcp NEVER calls approve).
type gate interface {
	enqueue(ctx context.Context, tool, agent, session string, args map[string]any) (hingeID int64, err error)
	status(ctx context.Context, hingeID int64) (string, error)
	withdraw(ctx context.Context, hingeID int64, reason string) error
}

// planner builds the per-session mount plan (the workspace-RO /work + writable
// islands) and cleans it up on close.
type planner interface {
	plan(handle string, req openReq) (loom.SessionOpts, mountInfo, error)
	cleanup(handle string) error
}

// openReq is one session_open request.
type openReq struct {
	purpose  string
	backend  string
	model    string
	writable bool
	workdir  string // caller-designated host build dir ("" = a default island under commissions/<h>/work)
}

// commission is one commissioned session and its live state.
type commission struct {
	handle    string
	req       openReq
	opts      loom.SessionOpts
	info      mountInfo
	createdAt time.Time

	mu         sync.Mutex
	state      sessState
	hingeID    int64
	sess       loom.Session
	note       string
	cancelPoll context.CancelFunc
}

func (c *commission) snapshot() (sessState, int64, loom.Session, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.hingeID, c.sess, c.note
}

func (c *commission) setState(s sessState, note string) {
	c.mu.Lock()
	c.state, c.note = s, note
	c.mu.Unlock()
}

func (c *commission) setOpen(sess loom.Session) {
	c.mu.Lock()
	c.state, c.sess, c.note = stateOpen, sess, "approved — open"
	c.mu.Unlock()
}

func (c *commission) setNote(note string) {
	c.mu.Lock()
	c.note = note
	c.mu.Unlock()
}

// manager owns the registry + policy knobs.
type manager struct {
	op          opener
	gt          gate
	pl          planner
	agent       string // proposer name on gate rows ("loom-mcp")
	maxSess     int
	pollEvery   time.Duration
	pollTimeout time.Duration
	sendTimeout time.Duration
	log         func(format string, args ...any)

	mu       sync.Mutex
	sessions map[string]*commission
}

func newManager(op opener, gt gate, pl planner, maxSess int, sendTimeout time.Duration, log func(string, ...any)) *manager {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &manager{
		op:          op,
		gt:          gt,
		pl:          pl,
		agent:       "loom-mcp",
		maxSess:     maxSess,
		pollEvery:   2 * time.Second,
		pollTimeout: 15 * time.Minute,
		sendTimeout: sendTimeout,
		log:         log,
		sessions:    map[string]*commission{},
	}
}

// activeCountLocked counts sessions holding (or about to hold) a real seat — the
// states that consume a serve slot. Declared/closed/errored ones don't count, so
// they never wedge the cap. Caller holds m.mu.
func (m *manager) activeCountLocked() int {
	n := 0
	for _, c := range m.sessions {
		st, _, _, _ := c.snapshot()
		if st == stateAwaiting || st == stateOpen {
			n++
		}
	}
	return n
}

func (m *manager) get(handle string) *commission {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[handle]
}

// Open commissions a session. Read-only advisory sessions open immediately (no
// tap). Writable sessions enqueue a tap gate and open only once Michael approves
// (a background poller watches the gate). Refuses over the concurrency cap.
func (m *manager) Open(ctx context.Context, req openReq) (openResult, error) {
	req.backend = strings.TrimSpace(req.backend)
	if req.backend == "" {
		req.backend = "claude"
	}
	if strings.TrimSpace(req.purpose) == "" {
		return openResult{}, fmt.Errorf("purpose is required — say what this session is for")
	}

	m.mu.Lock()
	if n := m.activeCountLocked(); n >= m.maxSess {
		m.mu.Unlock()
		return openResult{}, fmt.Errorf("commission cap reached (%d of %d active) — close one with session_close before opening another", n, m.maxSess)
	}
	handle := newHandle()
	opts, info, err := m.pl.plan(handle, req)
	if err != nil {
		m.mu.Unlock()
		return openResult{}, fmt.Errorf("prepare session workspace: %w", err)
	}
	c := &commission{handle: handle, req: req, opts: opts, info: info, createdAt: time.Now(), state: stateAwaiting}
	m.sessions[handle] = c
	m.mu.Unlock()

	if !req.writable {
		// Advisory read-only seat: /work mounted read-only, framed answer-don't-act.
		// No tap — reading and reasoning are safe; the seat's only writes are to its
		// own claude home + the substrate hinge.
		sess, err := m.op.open(ctx, req.backend, opts)
		if err != nil {
			c.setState(stateError, "open failed: "+err.Error())
			return openResult{Handle: handle, State: string(stateError), Message: "failed to open session: " + err.Error()}, nil
		}
		c.setOpen(sess)
		return openResult{
			Handle: handle, State: string(stateOpen), Writable: false,
			Workspace: m.workspaceOf(info), Scratch: info.scratchHost,
			Message: "Read-only advisory session is open — send it a question with session_send.",
		}, nil
	}

	// Writable builder seat: enqueue the tap gate; the open is deferred to approval.
	args := map[string]any{
		"purpose":     req.purpose,
		"backend":     req.backend,
		"model":       req.model,
		"writable":    true,
		"work_dir":    info.workHost,
		"scratch_dir": info.scratchHost,
		"handle":      handle,
	}
	tool := "loom.session_open (writable): " + clip(req.purpose, 60)
	hingeID, err := m.gt.enqueue(ctx, tool, m.agent, handle, args)
	if err != nil {
		c.setState(stateError, "gate enqueue failed: "+err.Error())
		return openResult{}, fmt.Errorf("enqueue approval tap: %w", err)
	}
	c.mu.Lock()
	c.hingeID = hingeID
	c.state = stateAwaiting
	pctx, cancel := context.WithCancel(context.Background())
	c.cancelPoll = cancel
	c.mu.Unlock()
	go m.poll(pctx, c)

	return openResult{
		Handle: handle, State: string(stateAwaiting), Writable: true, HingeID: hingeID,
		Workdir: info.workHost, Scratch: info.scratchHost, Workspace: m.workspaceOf(info),
		Message: fmt.Sprintf(
			"This writable commission needs your tap — I've put it on your screen (approval card #%d). "+
				"It opens the moment you approve; poll session_list or just retry session_send. "+
				"Decline it and the session never opens.", hingeID),
	}, nil
}

// poll watches a writable commission's gate row until approve/decline/timeout.
func (m *manager) poll(ctx context.Context, c *commission) {
	t := time.NewTicker(m.pollEvery)
	defer t.Stop()
	deadline := time.Now().Add(m.pollTimeout)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Now().After(deadline) {
				c.setState(stateDeclined, "approval timed out — the tap was not answered in time")
				_ = m.gt.withdraw(context.Background(), c.hingeID, "loom-mcp approval window elapsed")
				return
			}
			st, err := m.gt.status(ctx, c.hingeID)
			if err != nil {
				m.log("poll gate #%d: %v", c.hingeID, err)
				continue
			}
			switch st {
			case "applied", "approved":
				// Michael approved. Open the real seat now (spawn is lazy — the
				// container starts on the first session_send, not here).
				octx, cancel := context.WithTimeout(context.Background(), time.Minute)
				sess, err := m.op.open(octx, c.req.backend, c.opts)
				cancel()
				if err != nil {
					c.setState(stateError, "approved but open failed: "+err.Error())
					return
				}
				c.setOpen(sess)
				m.log("commission %s approved (gate #%d) → open", c.handle, c.hingeID)
				return
			case "declined", "revise":
				c.setState(stateDeclined, "you declined the tap")
				m.log("commission %s declined (gate #%d)", c.handle, c.hingeID)
				return
			default: // pending, escalated → keep waiting
			}
		}
	}
}

// Send delivers a turn to an open session and returns its reply. Non-open states
// return an informational (non-error) result so a caller can poll by retrying.
func (m *manager) Send(ctx context.Context, handle, message string) (sendResult, error) {
	c := m.get(handle)
	if c == nil {
		return sendResult{}, fmt.Errorf("no such session %q", handle)
	}
	st, hingeID, sess, note := c.snapshot()
	switch st {
	case stateAwaiting:
		return sendResult{Handle: handle, State: string(st),
			Message: fmt.Sprintf("Still awaiting your tap (approval card #%d) — approve it, then send again.", hingeID)}, nil
	case stateDeclined:
		return sendResult{Handle: handle, State: string(st),
			Message: "This commission was declined and never opened — open a new one if you still need it."}, nil
	case stateClosed:
		return sendResult{Handle: handle, State: string(st),
			Message: "This session was closed (e-stopped) — open a new one to continue."}, nil
	case stateError:
		return sendResult{Handle: handle, State: string(st), Message: "This session is in an error state: " + note}, nil
	}
	if sess == nil {
		return sendResult{Handle: handle, State: string(st), Message: "session not ready yet"}, nil
	}

	// Synchronous send, bounded by the send timeout. The ws read is not
	// context-aware, so a timed-out turn's goroutine finishes when the reply
	// eventually arrives (buffered channel → no leak of the result); we surface
	// "still running" and leave the session open for a session_close if wedged.
	// (A detach/await variant — connectSession already implements it — is P2.)
	sctx, cancel := context.WithTimeout(ctx, m.sendTimeout)
	defer cancel()
	type res struct {
		r   loom.Reply
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := sess.Send(sctx, message)
		ch <- res{r, err}
	}()
	select {
	case <-sctx.Done():
		c.setNote("last send exceeded the send timeout; the turn may still be running server-side")
		return sendResult{Handle: handle, State: string(stateOpen), TimedOut: true,
			Message: "The turn is taking longer than the send timeout and may still be running. Check back, or session_close if it's wedged."}, nil
	case rr := <-ch:
		if rr.err != nil {
			return sendResult{Handle: handle, State: string(stateOpen), Reply: rr.r.Text, Error: rr.err.Error()}, nil
		}
		return sendResult{Handle: handle, State: string(stateOpen), Reply: rr.r.Text, CostUSD: rr.r.CostUSD}, nil
	}
}

// List returns every commission this loom-mcp holds, plus the active/max count.
func (m *manager) List() listResult {
	m.mu.Lock()
	out := make([]sessionView, 0, len(m.sessions))
	for _, c := range m.sessions {
		st, hingeID, _, note := c.snapshot()
		out = append(out, sessionView{
			Handle: c.handle, Purpose: c.req.purpose, Backend: c.req.backend, Model: c.req.model,
			Writable: c.req.writable, State: string(st), HingeID: hingeID,
			Workdir: c.info.workHost, Scratch: c.info.scratchHost,
			AgeSeconds: int(time.Since(c.createdAt).Seconds()), Note: note,
		})
	}
	active := m.activeCountLocked()
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return listResult{Sessions: out, Active: active, Max: m.maxSess}
}

// Close is the e-stop: it kills the live seat (the ephemeral session's process is
// dropped by the serve), stops any pending poller, withdraws a not-yet-approved
// tap, and removes the session's scratch dirs. Works on ANY handle in the registry.
func (m *manager) Close(ctx context.Context, handle, reason string) (closeResult, error) {
	c := m.get(handle)
	if c == nil {
		return closeResult{}, fmt.Errorf("no such session %q", handle)
	}
	c.mu.Lock()
	prev := c.state
	sess := c.sess
	cancel := c.cancelPoll
	hingeID := c.hingeID
	c.state = stateClosed
	if reason != "" {
		c.note = "closed: " + reason
	} else {
		c.note = "closed (e-stop)"
	}
	c.sess = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel() // stop the approval poller if this was pending
	}
	killed := false
	if sess != nil {
		_ = sess.Close() // ephemeral session → serve drops the process (a true kill)
		killed = true
	}
	if prev == stateAwaiting && hingeID != 0 {
		// Withdraw the pending tap so the card clears (decline is the safe direction).
		_ = m.gt.withdraw(context.Background(), hingeID, "withdrawn by session_close")
	}
	_ = m.pl.cleanup(handle)

	msg := "Session closed."
	switch {
	case killed:
		msg = "Session e-stopped — the running seat was killed."
	case prev == stateAwaiting:
		msg = "Pending commission withdrawn before approval."
	}
	return closeResult{OK: true, Handle: handle, Killed: killed, PrevState: string(prev), Message: msg}, nil
}

// shutdown cancels every poller and closes every live seat (process exit / SIGINT).
func (m *manager) shutdown() {
	m.mu.Lock()
	all := make([]*commission, 0, len(m.sessions))
	for _, c := range m.sessions {
		all = append(all, c)
	}
	m.mu.Unlock()
	for _, c := range all {
		_, _, _, _ = c.snapshot()
		c.mu.Lock()
		sess := c.sess
		cancel := c.cancelPoll
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if sess != nil {
			_ = sess.Close()
		}
	}
}

func (m *manager) workspaceOf(info mountInfo) string { return info.workspaceHost }

// newHandle mints a short, human-legible commission handle.
func newHandle() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "commission-" + hex.EncodeToString(b[:])
}

// clip truncates s to n runes with an ellipsis.
func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
