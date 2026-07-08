package loom

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func msg(role, text string) openaiMessage {
	b, _ := json.Marshal(text)
	return openaiMessage{Role: role, Content: json.RawMessage(b)}
}

func TestStickyKeyFor(t *testing.T) {
	if k := stickyKeyFor("sonnet#companion", "sticky:companion"); k != "sonnet#companion|sticky:companion" {
		t.Fatalf("sticky key: %q", k)
	}
	if k := stickyKeyFor("sonnet#critic", "wi--abc123--critique"); k != "" {
		t.Fatalf("wi-- user must NOT be sticky (pipeline retry semantics), got %q", k)
	}
	if k := stickyKeyFor("sonnet", ""); k != "" {
		t.Fatalf("empty user must not be sticky, got %q", k)
	}
}

func TestFlattenDelta(t *testing.T) {
	// First turn: no assistant yet -> not a delta.
	if _, ok := flattenDelta([]openaiMessage{msg("system", "s"), msg("user", "hi")}); ok {
		t.Fatal("no-assistant history must not be a delta")
	}
	// Assistant last (nothing new) -> not a delta.
	if _, ok := flattenDelta([]openaiMessage{msg("user", "hi"), msg("assistant", "hey")}); ok {
		t.Fatal("assistant-last history must not be a delta")
	}
	// Normal turn: only what follows the last assistant message.
	d, ok := flattenDelta([]openaiMessage{
		msg("system", "persona"),
		msg("user", "hi"),
		msg("assistant", "hey"),
		msg("developer", "REMINDER DUE: stretch"),
		msg("user", "what number did I tell you?"),
	})
	if !ok {
		t.Fatal("expected a delta")
	}
	if strings.Contains(d, "persona") || strings.Contains(d, "hey") {
		t.Fatalf("delta leaked earlier turns: %q", d)
	}
	if !strings.Contains(d, "REMINDER DUE") || !strings.Contains(d, "what number") {
		t.Fatalf("delta missing new messages: %q", d)
	}
}

func TestStickyRegistryReap(t *testing.T) {
	stickyMu.Lock()
	stickyMap = map[string]*stickyEntry{}
	stickyMu.Unlock()

	e := stickyFor("m|sticky:x")
	e.sessionID = "s-1"
	stickyTouch(e)
	if e2 := stickyFor("m|sticky:x"); e2 != e {
		t.Fatal("same key must return the same entry")
	}
	// Age it past the idle boundary and access ANOTHER key -> reaped.
	stickyMu.Lock()
	e.lastUsed = time.Now().Add(-stickyIdle - time.Minute)
	stickyMu.Unlock()
	stickyFor("m|sticky:other")
	stickyMu.Lock()
	_, alive := stickyMap["m|sticky:x"]
	stickyMu.Unlock()
	if alive {
		t.Fatal("idle entry must be reaped (fresh mind per sitting)")
	}
}
