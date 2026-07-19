package loom

// admin.go — the serve-wide read+kill surface behind `loom serve`.
//
// The existing `sessions` op (serve.go) lists the ws RESIDENTS a client may
// reattach — enough for the warm-reattach UX, but it deliberately omits two
// things a supervising surface (the brain-app's Loom/Spin tabs) needs:
//
//  1. the OpenAI shim's WARM sticky seats (openai_sticky.go), which live in a
//     separate registry the ws `sessions` op never sees; and
//  2. a short recent-transcript TAIL, so a human can glance at what a session is
//     actually saying before deciding to stop it.
//
// `overview` adds both. `kill` generalizes the e-stop to name-OR-handle across
// BOTH registries, applying the right per-kind semantics: a ws resident is hard
// closed (process dropped, its remembered lineage cleared — the existing close
// semantics); a warm sticky seat is DOWNGRADED to cold-resumable (its live
// process torn down, its lineage kept) so stopping a voice seat mid-idle never
// throws away the conversation. Both ops ride the same authenticated ws
// connection as every other op, so the serve token already gates them — this is
// a privileged surface, and the mesh bind + token are its two walls.

import (
	"sort"
	"strings"
	"time"
)

// overviewTailMax caps the recent-reply tail carried per session, so a chatty
// resident can't bloat an overview frame. It is a glance, not a transcript —
// the full history lives where the session runs.
const overviewTailMax = 600

// SessionOverview is one live session as reported by `overview`: a ws resident
// or a warm sticky seat, with enough to render a card (kind, model, state,
// idle) and a short tail to glance at before stopping it.
type SessionOverview struct {
	Kind        string `json:"kind"`         // "resident" (ws) | "warm-seat" (OpenAI shim sticky)
	Name        string `json:"name,omitempty"`
	Handle      string `json:"handle"`       // ws handle (resident) OR sticky key (warm seat) — the kill target
	Backend     string `json:"backend,omitempty"`
	Model       string `json:"model,omitempty"`
	State       string `json:"state"`        // resident: "running"|"idle"; warm seat: "warm"
	IdleSeconds int    `json:"idle_seconds"`
	LastTurnID  int64  `json:"last_turn_id,omitempty"`
	Tail        string `json:"tail,omitempty"` // most recent reply text (residents); warm seats carry none
	Named       bool   `json:"named,omitempty"`
}

// overview snapshots every live session the serve holds: ws residents (from
// s.sessions) plus warm sticky seats (openai_sticky.go). Unnamed residents ARE
// included (a loom-mcp commission is an unnamed resident); the caller decides
// whether to fold those into its own richer view.
func (s *server) overview(now time.Time) []SessionOverview {
	s.mu.Lock()
	residents := make([]*residentSession, 0, len(s.sessions))
	for _, rs := range s.sessions {
		residents = append(residents, rs)
	}
	s.mu.Unlock()

	out := make([]SessionOverview, 0, len(residents)+1)
	for _, rs := range residents {
		out = append(out, rs.overview(now))
	}
	// Warm sticky seats live outside s.sessions (a parallel registry keyed by the
	// shim's sticky key); enumerate them here so the overview is truly serve-wide.
	out = append(out, stickyOverview(now)...)
	return out
}

// overview snapshots one resident for the overview: state (running vs idle),
// idle seconds, and the most recent completed reply as a tail.
func (rs *residentSession) overview(now time.Time) SessionOverview {
	rs.mu.Lock()
	idle := int(now.Sub(rs.lastActive).Seconds())
	if idle < 0 {
		idle = 0
	}
	state := "idle"
	if rs.running > 0 {
		state = "running"
	}
	tail := lastReplyText(rs.ring)
	last := rs.lastTurnID
	rs.mu.Unlock()
	return SessionOverview{
		Kind:        "resident",
		Name:        rs.name,
		Handle:      rs.handle,
		Backend:     rs.agent,
		Model:       rs.opts.Model,
		State:       state,
		IdleSeconds: idle,
		LastTurnID:  last,
		Tail:        tail,
		Named:       rs.name != "",
	}
}

// lastReplyText returns the newest COMPLETED turn's reply text, truncated. It
// walks the ring from newest to oldest so an in-flight turn (done==false, empty
// reply) is skipped in favor of the last thing the session actually said.
func lastReplyText(ring []*turnRecord) string {
	for i := len(ring) - 1; i >= 0; i-- {
		if ring[i].done {
			return clipRunes(ring[i].reply.Text, overviewTailMax)
		}
	}
	return ""
}

// killTarget kills a session by name OR handle across both registries and
// reports which kind it was + the semantics applied. A ws resident is hard
// closed (process dropped, name + remembered lineage cleared); a warm sticky
// seat is downgraded to cold-resumable (live process torn down, lineage kept).
// ok=false means nothing matched the target.
func (s *server) killTarget(target string) (kind, note string, ok bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", false
	}

	// 1. a ws resident, by handle then by name.
	s.mu.Lock()
	rs := s.sessions[target]
	if rs == nil {
		rs = s.byName[target]
	}
	if rs != nil {
		delete(s.sessions, rs.handle)
		if rs.name != "" {
			delete(s.byName, rs.name)
			delete(s.remembered, rs.name)
		}
	}
	s.mu.Unlock()
	if rs != nil {
		// Mark closed first so no new turn starts, then drop the live process. An
		// in-flight turn's backend read unblocks when Close kills the process, exactly
		// as the idle downgrade and the ephemeral socket-drop close already do.
		rs.mu.Lock()
		rs.closed = true
		rs.mu.Unlock()
		_ = rs.sess.Close()
		return "resident", "resident closed — live process dropped, remembered lineage cleared", true
	}

	// 2. a warm sticky seat (OpenAI shim), by sticky key or friendly name.
	if n, hit := stickyKill(target); hit {
		return "warm-seat", n, true
	}
	return "", "", false
}

// handleOverview answers a serve-wide overview request (read-only).
func (c *conn) handleOverview(f frame) {
	ov := c.srv.overview(time.Now())
	sortOverview(ov)
	_ = c.ws.WriteJSON(frame{Op: opOverviewOK, Overview: ov})
}

// handleKill answers a name-or-handle kill request, reporting the kind killed
// and the semantics applied (or ok:false + a note when nothing matched).
func (c *conn) handleKill(f frame) {
	kind, note, ok := c.srv.killTarget(f.Target)
	if !ok {
		_ = c.ws.WriteJSON(frame{Op: opKillOK, OK: false, Target: f.Target,
			Note: "no live resident or warm seat matched " + strconvName(f.Target)})
		return
	}
	_ = c.ws.WriteJSON(frame{Op: opKillOK, OK: true, Target: f.Target, Kind: kind, Note: note})
}

// sortOverview orders the overview stably: kind, then name, then handle — so a
// polling client sees a steady list rather than map-iteration jitter.
func sortOverview(ov []SessionOverview) {
	sort.Slice(ov, func(i, j int) bool {
		if ov[i].Kind != ov[j].Kind {
			return ov[i].Kind < ov[j].Kind
		}
		if ov[i].Name != ov[j].Name {
			return ov[i].Name < ov[j].Name
		}
		return ov[i].Handle < ov[j].Handle
	})
}

// clipRunes truncates s to n runes with an ellipsis, rune-safe (a reply tail may
// hold multibyte text). Kept local so loom's core stays dependency-free.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
