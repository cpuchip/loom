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
	"strings"
	"sync"
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

type stickyEntry struct {
	mu        sync.Mutex // serializes turns within one conversation
	sessionID string
	lastUsed  time.Time
}

var (
	stickyMu  sync.Mutex
	stickyMap = map[string]*stickyEntry{}
)

// stickyFor returns the conversation entry for key, creating it if absent and
// lazily reaping idle entries. The caller must lock entry.mu around the turn.
func stickyFor(key string) *stickyEntry {
	stickyMu.Lock()
	defer stickyMu.Unlock()
	now := time.Now()
	for k, e := range stickyMap {
		// Reap without taking e.mu: lastUsed is only advanced under stickyMu
		// (see stickyTouch), so this read is consistent enough for a 2h TTL.
		if now.Sub(e.lastUsed) > stickyIdle {
			delete(stickyMap, k)
		}
	}
	e, ok := stickyMap[key]
	if !ok {
		e = &stickyEntry{lastUsed: now}
		stickyMap[key] = e
	}
	return e
}

// stickyTouch advances the idle clock for key's entry.
func stickyTouch(e *stickyEntry) {
	stickyMu.Lock()
	e.lastUsed = time.Now()
	stickyMu.Unlock()
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
