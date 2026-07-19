// openai_sticky.go — sticky sessions for the OpenAI shim (companion wave).
//
// The shim's historical contract is one FRESH isolated session per completion:
// right for single-shot seats (critique, review, war-game), wasteful for a
// CONVERSATION, where every turn re-reads the whole flattened transcript
// uncached and the seat's working state (tool results, files, reasoning) dies
// with each turn's container.
//
// Sticky mode is OPT-IN per request: a caller that sets the standard `user`
// field to "sticky:<name>" gets one LIVING claude session per (model, user)
// key. Turn 1 runs fresh (full transcript) and records Reply.SessionID; later
// turns send ONLY the delta (messages after the last assistant message) into
// `--resume <id>`. The per-turn container stays disposable — the session STATE
// lives in the mounted role claude-home, which is exactly what makes
// resume+isolate work (proven: remember-61 → new container → 61).
//
// Self-healing by construction: OpenAI callers (Pipecat et al.) send the FULL
// message history every turn anyway, so when a resume fails — session file
// gone, home swapped, anything — the shim drops the mapping and silently
// replays the whole transcript into a fresh session. The caller never sees the
// stumble. Substrate wi--* dispatches and plain callers are untouched: their
// per-turn semantics (a retry must NOT resume the failed attempt) are
// load-bearing for pipelines.
//
// NOTE: claude --resume can mint a NEW session id that continues the old
// transcript (resume-as-fork). We always store the LATEST Reply.SessionID, so
// the chain follows wherever the ids lead.

package loom

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stickyPrefix = "sticky:"
	// stickyIdle is the conversation boundary: a key untouched this long is
	// forgotten (the transcript file survives in the claude-home; only the
	// shim's mapping is dropped). Next contact = a fresh mind, which is the
	// deliberate "new sitting" semantic — durable memory belongs to the
	// substrate, not the session.
	stickyIdle = 2 * time.Hour
)

// openaiWarm gates warm-resident sticky seats (`loom serve --openai-warm`, DEFAULT
// OFF). When off, a sticky turn behaves exactly as before — a fresh isolated
// session spawned per turn, --resume carrying the lineage — so deploying a
// warm-capable binary changes nothing until the flag is chosen. When on, a sticky
// seat's live claude process/container is kept alive between turns, so the next
// turn feeds straight into the running process instead of paying the ~2.5-3s
// cold spawn+--resume floor (measured E1 2026-07-15). Bare (non-sticky) models and
// wi-- pipeline dispatches are NEVER warm — they keep their per-turn semantics.
var openaiWarm bool

// SetOpenAIWarm enables/disables warm-resident sticky seats.
func SetOpenAIWarm(on bool) { openaiWarm = on }

// stickyWarmN counts how many sticky seats are currently WARM (a live process +
// container + the model's in-flight context). It is the enforcement variable for
// the warm ceiling below. Atomic so every warm/cold transition — under e.mu on the
// turn path, under no lock in the reaper — updates it consistently.
var stickyWarmN atomic.Int32

// stickyWarmMax caps concurrent warm seats. Beyond it a new conversation still
// works — it simply runs on the cold spawn+--resume path (the historical sticky
// behavior), never failing; it just forgoes the warm optimization until a seat
// frees. A warm claude seat holds a live process, a docker container, and the
// model's context (~hundreds of MB), so this is a MEMORY ceiling, not a correctness
// one. The dominant use — one voice companion — needs 1; 8 leaves headroom for a
// few roles/users without unbounded growth. Override with LOOM_OPENAI_WARM_MAX
// (0 disables warm entirely by making every seat "at cap").
var stickyWarmMax = func() int {
	if v := os.Getenv("LOOM_OPENAI_WARM_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 8
}()

type stickyEntry struct {
	mu        sync.Mutex // serializes turns within one conversation (held across a whole turn)
	sessionID string     // the evolved lineage id for a cold --resume ("" = never run yet)
	lastUsed  time.Time  // the 2h forget clock (advanced under stickyMu by stickyTouch)

	// Warm-seat state (guarded by mu). warm is the live process kept alive between
	// turns; cancel/seatCtx are its SEAT-lifetime context (background-derived, NOT
	// any request — so it survives the request end that would SIGKILL a
	// request-ctx process). lastTurn is the idle clock the warm reaper reads.
	warm     Session
	cancel   context.CancelFunc
	seatCtx  context.Context
	lastTurn time.Time
}

// canServeWarm reports whether this turn may run on a warm seat: an already-warm
// seat always may; a cold one only if the warm ceiling has room. Caller holds e.mu.
// The ceiling read races benignly with concurrent opens on OTHER keys — a soft cap
// that may transiently overshoot by a few is fine (documented on stickyWarmMax).
func (e *stickyEntry) canServeWarm() bool {
	return e.warm != nil || int(stickyWarmN.Load()) < stickyWarmMax
}

// runStickyWarm executes one turn on a WARM sticky seat and leaves the seat's live
// process alive for the next turn. It is the shim's answer to the cold spawn+--resume
// floor that dominates a simple conversational turn. It mirrors the ws resident
// semantics (serve.go's residentSession) as a parallel, self-contained warm layer
// keyed by the sticky key — deliberately NOT reusing the ws byName/remembered maps,
// so the shim's seats never cross-contaminate the ws protocol's reattach path.
//
//   - Cold seat (warm == nil): open ONE live session bound to a SEAT-lifetime
//     context, resuming `resume` when a lineage exists (post-downgrade / post-crash)
//     else fresh (turn 1), and keep it warm.
//   - Warm seat (warm != nil): feed the turn straight to the live process — no
//     spawn, no cold --resume read. This is the whole latency win.
//
// A per-turn watchdog bounds the turn by openaiTimeout by cancelling the seat
// context (SIGKILL → the backend's read unblocks → SendStream errors); on ANY error
// the seat is torn down (clearWarm) so serveOpenAI's cold fallback re-establishes
// the conversation — a warm crash degrades to cold, never fails the request. The
// warm process always DRAINS a turn to its result (the backend read loop ignores
// ctx), so a client disconnect cannot misalign the shared stdio: the turn finishes,
// its streamed bytes simply go nowhere.
//
// The caller MUST hold e.mu (turn serialization within one conversation) and MUST
// have gated on canServeWarm.
func (e *stickyEntry) runStickyWarm(be Backend, opts SessionOpts, resume, prompt string, onEvent func(Event)) (Reply, error) {
	if e.warm == nil {
		o := opts
		o.Resume = resume // "" = fresh (turn 1); a lineage = cold-resume INTO a warm seat
		seatCtx, cancel := context.WithCancel(context.Background())
		sess, err := be.Open(seatCtx, o)
		if err != nil {
			cancel()
			return Reply{}, err
		}
		e.warm, e.cancel, e.seatCtx = sess, cancel, seatCtx
		stickyWarmN.Add(1)
	}

	// Bound this turn: a watchdog cancels the seat context if the turn overruns
	// openaiTimeout; a normal completion stops the timer. Capturing cancel into a
	// local keeps the watchdog from touching e's fields concurrently.
	cancel := e.cancel
	done := make(chan struct{})
	var timedOut atomic.Bool
	timer := time.NewTimer(openaiTimeout)
	go func() {
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			timedOut.Store(true)
			cancel()
		}
	}()

	reply, err := e.warm.SendStream(e.seatCtx, prompt, onEvent)
	close(done)
	if err == nil && reply.Err != "" {
		err = fmt.Errorf("%s", reply.Err)
	}
	if err != nil || timedOut.Load() {
		if timedOut.Load() && err == nil {
			err = fmt.Errorf("warm turn exceeded %s", openaiTimeout)
		}
		e.clearWarm() // never reuse a crashed/killed process; caller cold-falls-back
		return reply, err
	}
	e.lastTurn = time.Now()
	return reply, nil
}

// clearWarm tears down the live warm process and drops the seat, decrementing the
// warm count. It preserves e.sessionID (the lineage) so the NEXT turn cold-resumes
// the conversation — one cold read, never lost context, mirroring serve.go's
// resident downgrade. Idempotent. Caller holds e.mu.
//
// Close is graceful (stdin EOF → claude flushes its session file → exits) UNLESS the
// seat context was already cancelled (a watchdog timeout SIGKILL, or a crash), in
// which case Close merely reaps the dead process; a cancelled-but-unflushed lineage
// is caught by serveOpenAI replaying the full transcript. Cancel is called last to
// release the watchdog goroutine's context; on a graceful close it is a no-op.
func (e *stickyEntry) clearWarm() {
	if e.warm == nil {
		return
	}
	sess, cancel := e.warm, e.cancel
	e.warm, e.cancel, e.seatCtx = nil, nil, nil
	stickyWarmN.Add(-1)
	_ = sess.Close()
	if cancel != nil {
		cancel()
	}
}

// reapStickyWarm downgrades every warm sticky seat idle longer than idleTTL — the
// same idle semantics serve.go's reaper applies to ws residents. Downgrade closes
// the live process gracefully and leaves the entry cold-resumable via its lineage.
// It never downgrades a seat with a turn in flight: a held e.mu (TryLock fails)
// means a turn is running, so that seat is skipped and reconsidered next tick.
// idleTTL <= 0 means "never downgrade on idle" (matching the ws reaper).
func reapStickyWarm(now time.Time, idleTTL time.Duration) {
	if idleTTL <= 0 {
		return
	}
	stickyMu.Lock()
	ents := make([]*stickyEntry, 0, len(stickyMap))
	for _, e := range stickyMap {
		ents = append(ents, e)
	}
	stickyMu.Unlock()
	for _, e := range ents {
		if !e.mu.TryLock() {
			continue // a turn is in flight — never downgrade mid-turn
		}
		if e.warm != nil && now.Sub(e.lastTurn) > idleTTL {
			e.clearWarm()
		}
		e.mu.Unlock()
	}
}

var (
	stickyMu  sync.Mutex
	stickyMap = map[string]*stickyEntry{}
)

// stickyFor returns the conversation entry for key, creating it if absent and
// lazily reaping idle entries. The caller must lock entry.mu around the turn.
//
// A forgotten entry may still hold a warm seat (only when the idle-TTL reaper is
// disabled — idleTTL 0 — since otherwise the seat is downgraded to cold long before
// the 2h forget). Such a seat's live process is released so forgetting it never
// leaks a process/container. The release runs OUTSIDE stickyMu (Close can block on
// the process exit, and the entry is already gone from the map — no turn can find
// it), and is skipped for an entry with a turn in flight (TryLock fails), which is
// simply forgotten on a later pass.
func stickyFor(key string) *stickyEntry {
	stickyMu.Lock()
	now := time.Now()
	var forgotten []*stickyEntry
	for k, e := range stickyMap {
		// Reap without blocking on lastUsed alone; lastUsed is only advanced under
		// stickyMu (see stickyTouch), so this read is consistent enough for a 2h TTL.
		if now.Sub(e.lastUsed) <= stickyIdle {
			continue
		}
		if e.warm != nil {
			if !e.mu.TryLock() {
				continue // a turn is in flight; forget it on a later pass
			}
			hadWarm := e.warm != nil
			e.mu.Unlock()
			if hadWarm {
				forgotten = append(forgotten, e)
			}
		}
		delete(stickyMap, k)
	}
	e, ok := stickyMap[key]
	if !ok {
		e = &stickyEntry{lastUsed: now}
		stickyMap[key] = e
	}
	stickyMu.Unlock()
	for _, fe := range forgotten {
		fe.mu.Lock()
		fe.clearWarm()
		fe.mu.Unlock()
	}
	return e
}

// stickyTouch advances the idle clock for key's entry.
func stickyTouch(e *stickyEntry) {
	stickyMu.Lock()
	e.lastUsed = time.Now()
	stickyMu.Unlock()
}

// stickyOverview reports the currently-WARM sticky seats for the serve-wide
// overview (admin.go). Only warm seats are surfaced: a cold sticky mapping is a
// paused conversation with no live process, nothing to show a stop button for
// (the next turn cold-resumes it). A warm seat carries no retained transcript —
// the shim flattens the caller's history per turn and keeps none — so Tail is
// intentionally empty; State is always "warm" and IdleSeconds counts from the
// last turn.
func stickyOverview(now time.Time) []SessionOverview {
	stickyMu.Lock()
	type kv struct {
		key string
		e   *stickyEntry
	}
	ents := make([]kv, 0, len(stickyMap))
	for k, e := range stickyMap {
		ents = append(ents, kv{k, e})
	}
	stickyMu.Unlock()

	out := make([]SessionOverview, 0, len(ents))
	for _, it := range ents {
		it.e.mu.Lock()
		warm := it.e.warm != nil
		last := it.e.lastTurn
		it.e.mu.Unlock()
		if !warm {
			continue
		}
		idle := 0
		if !last.IsZero() {
			idle = int(now.Sub(last).Seconds())
			if idle < 0 {
				idle = 0
			}
		}
		model, name := splitStickyKey(it.key)
		out = append(out, SessionOverview{
			Kind:        "warm-seat",
			Name:        name,
			Handle:      it.key,
			Backend:     "claude", // the shim's warm seats are always the claude backend
			Model:       model,
			State:       "warm",
			IdleSeconds: idle,
		})
	}
	return out
}

// stickyKill downgrades a warm sticky seat matched by its sticky key, its
// "sticky:<name>" user, or the bare <name>. A warm match is torn down to
// cold-resumable (its lineage is preserved, so the next turn continues the
// conversation); a matched-but-already-cold seat is a no-op that still reports
// hit=true (the target existed). hit=false means no sticky seat matched.
func stickyKill(target string) (note string, hit bool) {
	stickyMu.Lock()
	var match *stickyEntry
	var matchKey string
	for k, e := range stickyMap {
		_, name := splitStickyKey(k)
		if k == target || name == target || stickyPrefix+name == target {
			match, matchKey = e, k
			break
		}
	}
	stickyMu.Unlock()
	if match == nil {
		return "", false
	}
	match.mu.Lock()
	defer match.mu.Unlock()
	if match.warm != nil {
		match.clearWarm()
		return "warm seat " + matchKey + " downgraded to cold-resumable (live process torn down, lineage kept)", true
	}
	return "seat " + matchKey + " was already cold — nothing warm to stop (its lineage is intact)", true
}

// splitStickyKey parses a sticky map key ("model|sticky:name") into the model
// and the friendly conversation name. A malformed key degrades gracefully: the
// whole key as the model, an empty name.
func splitStickyKey(key string) (model, name string) {
	model, user, found := strings.Cut(key, "|")
	if !found {
		return key, ""
	}
	return model, strings.TrimPrefix(user, stickyPrefix)
}

// flattenDelta renders only the messages AFTER the last assistant message —
// the new user utterance plus any interleaved system/developer notes (e.g. a
// voice front's reminder injections). ok=false when there is no assistant
// message yet (first turn) or nothing follows it; callers then use the full
// flatten. The labeling matches flattenMessages so a resumed session reads
// turns in a consistent voice.
func flattenDelta(msgs []openaiMessage) (prompt string, ok bool) {
	last := -1
	for i, m := range msgs {
		if m.Role == "assistant" {
			last = i
		}
	}
	if last < 0 || last == len(msgs)-1 {
		return "", false
	}
	return flattenMessages(msgs[last+1:]), true
}

// stickyKeyFor derives the registry key, or "" when the request isn't sticky.
func stickyKeyFor(model, user string) string {
	if !strings.HasPrefix(user, stickyPrefix) {
		return ""
	}
	return model + "|" + user
}
