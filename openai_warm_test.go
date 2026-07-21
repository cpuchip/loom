package loom

// openai_warm_test.go — the warm-resident sticky-seat state machine (openai_sticky.go
// + serveOpenAI's warm branch). These drive the REAL serveOpenAI handler over
// httptest with a hermetic backend (no process, no docker, no model spend), so the
// oracle is the backend's open-count: a warm conversation must open ONCE and reuse
// the live process; a downgrade/crash/cap must fall back to a fresh cold open.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// warmStub is a hermetic Backend. openCount is the "did NOT respawn" oracle;
// lastResume proves a post-downgrade turn cold-resumes the remembered lineage;
// failOnCall injects a mid-conversation "crash" on a session's Nth turn.
type warmStub struct {
	mu         sync.Mutex
	opens      int
	lastResume string
	lastOpts   SessionOpts
	made       []*warmStubSession
	failOnCall int // >0: every session errors on that SendStream call number
}

func (b *warmStub) Name() string { return "stub" }

func (b *warmStub) Open(_ context.Context, opts SessionOpts) (Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	b.lastResume = opts.Resume
	b.lastOpts = opts
	s := &warmStubSession{id: fmt.Sprintf("warm-%d", b.opens), failOnCall: b.failOnCall}
	b.made = append(b.made, s)
	return s, nil
}

func (b *warmStub) openCount() int          { b.mu.Lock(); defer b.mu.Unlock(); return b.opens }
func (b *warmStub) lastResumeSeen() string  { b.mu.Lock(); defer b.mu.Unlock(); return b.lastResume }
func (b *warmStub) openedOpts() SessionOpts { b.mu.Lock(); defer b.mu.Unlock(); return b.lastOpts }
func (b *warmStub) sess(i int) *warmStubSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i < 0 || i >= len(b.made) {
		return nil
	}
	return b.made[i]
}

type warmStubSession struct {
	id         string
	failOnCall int
	calls      int32 // atomic
	closed     int32 // atomic
	mu         sync.Mutex
	lastPrompt string
}

func (s *warmStubSession) Send(ctx context.Context, p string) (Reply, error) {
	return s.SendStream(ctx, p, nil)
}

func (s *warmStubSession) SendStream(_ context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	n := atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	s.lastPrompt = prompt
	s.mu.Unlock()
	if s.failOnCall > 0 && int(n) == s.failOnCall {
		return Reply{Backend: "stub", Err: "boom"}, fmt.Errorf("boom (simulated warm crash)")
	}
	emit(onEvent, Event{Kind: EvAssistant, Backend: "stub", Text: "echo:" + prompt})
	return Reply{Backend: "stub", Text: "echo:" + prompt, SessionID: s.id}, nil
}

func (s *warmStubSession) SessionID() string { return s.id }
func (s *warmStubSession) Close() error      { atomic.StoreInt32(&s.closed, 1); return nil }
func (s *warmStubSession) isClosed() bool    { return atomic.LoadInt32(&s.closed) == 1 }
func (s *warmStubSession) prompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPrompt
}

// --- harness ---

func newWarmTestServer(be Backend, idleTTL time.Duration) *server {
	ts := &tokenStore{hashes: map[[32]byte]struct{}{}}
	return newServer(ts, map[string]Backend{"claude": be}, idleTTL)
}

func resetSticky() {
	stickyMu.Lock()
	stickyMap = map[string]*stickyEntry{}
	stickyMu.Unlock()
	stickyWarmN.Store(0)
}

// postWarmTurn POSTs one non-streaming completion to the real serveOpenAI handler and
// returns the assistant text + HTTP status.
func postWarmTurn(t *testing.T, srv *server, model, user string, msgs []openaiMessage) (string, int) {
	t.Helper()
	body, err := json.Marshal(openaiChatReq{Model: model, Messages: msgs, User: user, Stream: false})
	if err != nil {
		t.Fatalf("marshal req: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.serveOpenAI(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return string(b), res.StatusCode
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if len(out.Choices) == 0 {
		return "", res.StatusCode
	}
	return out.Choices[0].Message.Content, res.StatusCode
}

func convo(parts ...openaiMessage) []openaiMessage { return parts }

// --- tests ---

// TestOpenAIWarmReusesLiveProcess is the headline: with --openai-warm, a second
// sticky turn feeds the SAME live process — the open-count stays 1 (no cold
// respawn), and the warm turn sends only the delta (the process holds the rest).
func TestOpenAIWarmReusesLiveProcess(t *testing.T) {
	resetSticky()
	defer func(p bool) { openaiWarm = p }(openaiWarm)
	openaiWarm = true

	sb := &warmStub{}
	srv := newWarmTestServer(sb, time.Hour)

	turn1 := convo(msg("system", "persona"), msg("user", "hi"))
	if txt, code := postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", turn1); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("turn1: code=%d txt=%q", code, txt)
	}
	if n := sb.openCount(); n != 1 {
		t.Fatalf("turn1 opens=%d want 1", n)
	}
	if stickyWarmN.Load() != 1 {
		t.Fatalf("turn1 warm count=%d want 1 (seat held warm)", stickyWarmN.Load())
	}

	turn2 := convo(msg("system", "persona"), msg("user", "hi"), msg("assistant", "hey"), msg("user", "again"))
	if txt, code := postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", turn2); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("turn2: code=%d txt=%q", code, txt)
	}
	if n := sb.openCount(); n != 1 {
		t.Fatalf("warm turn2 respawned: opens=%d want 1 (the whole point — no cold spawn+--resume)", n)
	}
	if got := sb.sess(0).prompt(); !strings.Contains(got, "again") || strings.Contains(got, "persona") {
		t.Fatalf("warm turn2 prompt=%q, want the delta only (live process holds the rest)", got)
	}
}

// TestOpenAIStickyColdUnchangedWhenWarmOff proves the flag default is inert: with
// warm OFF, sticky turns keep the historical per-turn spawn (2 opens) and NEVER hold
// a warm seat — deploying the binary changes nothing until --openai-warm is chosen.
func TestOpenAIStickyColdUnchangedWhenWarmOff(t *testing.T) {
	resetSticky()
	defer func(p bool) { openaiWarm = p }(openaiWarm)
	openaiWarm = false

	sb := &warmStub{}
	srv := newWarmTestServer(sb, time.Hour)

	postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", convo(msg("user", "hi")))
	postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", convo(msg("user", "hi"), msg("assistant", "hey"), msg("user", "again")))
	if n := sb.openCount(); n != 2 {
		t.Fatalf("warm OFF: sticky opens=%d want 2 (per-turn spawn unchanged)", n)
	}
	if stickyWarmN.Load() != 0 {
		t.Fatalf("warm OFF must never hold a warm seat (count=%d)", stickyWarmN.Load())
	}
}

// TestOpenAIWarmIdleDowngrade mirrors the ws resident janitor for warm shim seats:
// a seat idle past the TTL is downgraded (its live process CLOSED), the entry left
// cold-resumable; the next turn cold-opens (count→2) resuming the remembered lineage
// — one cold read, never lost context — then re-warms.
func TestOpenAIWarmIdleDowngrade(t *testing.T) {
	resetSticky()
	defer func(p bool) { openaiWarm = p }(openaiWarm)
	openaiWarm = true

	sb := &warmStub{}
	srv := newWarmTestServer(sb, time.Hour)

	postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", convo(msg("user", "hi")))
	if n := sb.openCount(); n != 1 {
		t.Fatalf("opens=%d want 1", n)
	}
	if stickyWarmN.Load() != 1 {
		t.Fatalf("warm count=%d want 1", stickyWarmN.Load())
	}

	// Force the reaper 2h into the future (past the 1h TTL) — deterministic, no sleep.
	reapStickyWarm(time.Now().Add(2*time.Hour), srv.idleTTL)
	if !sb.sess(0).isClosed() {
		t.Fatal("idle downgrade must CLOSE the warm process")
	}
	if stickyWarmN.Load() != 0 {
		t.Fatalf("warm count=%d want 0 after downgrade", stickyWarmN.Load())
	}

	turn2 := convo(msg("user", "hi"), msg("assistant", "hey"), msg("user", "again"))
	if txt, code := postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", turn2); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("post-downgrade turn: code=%d txt=%q", code, txt)
	}
	if n := sb.openCount(); n != 2 {
		t.Fatalf("post-downgrade opens=%d want 2 (a cold resume)", n)
	}
	if got := sb.lastResumeSeen(); got != "warm-1" {
		t.Fatalf("post-downgrade Resume=%q want warm-1 (the remembered lineage)", got)
	}
	if stickyWarmN.Load() != 1 {
		t.Fatalf("post-downgrade warm count=%d want 1 (re-warmed)", stickyWarmN.Load())
	}
}

// TestOpenAIWarmCrashFallsBackToCold proves constraint (4): a warm-seat crash degrades
// to the cold spawn path and NEVER fails the request. The 2nd turn hits the seat's
// injected crash → the seat is torn down → the cold fallback replays the FULL
// transcript into a fresh session, and the caller sees a normal reply.
func TestOpenAIWarmCrashFallsBackToCold(t *testing.T) {
	resetSticky()
	defer func(p bool) { openaiWarm = p }(openaiWarm)
	openaiWarm = true

	sb := &warmStub{failOnCall: 2} // each seat "crashes" on its 2nd turn
	srv := newWarmTestServer(sb, time.Hour)

	if txt, code := postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", convo(msg("user", "hi"))); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("turn1: code=%d txt=%q", code, txt)
	}
	if n := sb.openCount(); n != 1 {
		t.Fatalf("turn1 opens=%d want 1", n)
	}

	turn2 := convo(msg("user", "hi"), msg("assistant", "hey"), msg("user", "again"))
	txt, code := postWarmTurn(t, srv, "sonnet#warmtest", "sticky:wt", turn2)
	if code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("warm crash must degrade to a working cold turn: code=%d txt=%q", code, txt)
	}
	if n := sb.openCount(); n != 2 {
		t.Fatalf("crash fallback opens=%d want 2 (a fresh cold spawn)", n)
	}
	if !sb.sess(0).isClosed() {
		t.Fatal("crashed warm seat must be torn down (closed)")
	}
	if stickyWarmN.Load() != 0 {
		t.Fatalf("crash must not leak a warm seat: count=%d want 0", stickyWarmN.Load())
	}
	if got := sb.sess(1).prompt(); !strings.Contains(got, "hi") || !strings.Contains(got, "again") {
		t.Fatalf("cold fallback prompt=%q want the FULL transcript replay", got)
	}
}

// TestOpenAIWarmCapFallsBackToCold proves the concurrent-warm ceiling: once the cap is
// full, a new conversation still works — it just runs cold (per-turn spawn), never
// warming and never failing.
func TestOpenAIWarmCapFallsBackToCold(t *testing.T) {
	resetSticky()
	defer func(pw bool, pm int) { openaiWarm = pw; stickyWarmMax = pm }(openaiWarm, stickyWarmMax)
	openaiWarm = true
	stickyWarmMax = 1

	sb := &warmStub{}
	srv := newWarmTestServer(sb, time.Hour)

	// Key A claims the single warm slot.
	postWarmTurn(t, srv, "sonnet#warmtest", "sticky:A", convo(msg("user", "hi")))
	if stickyWarmN.Load() != 1 {
		t.Fatalf("key A warm count=%d want 1", stickyWarmN.Load())
	}
	afterA := sb.openCount()

	// Key B is over the cap → runs COLD, succeeds, never warms.
	if txt, code := postWarmTurn(t, srv, "sonnet#warmtest", "sticky:B", convo(msg("user", "yo"))); code != 200 || !strings.Contains(txt, "echo:") {
		t.Fatalf("key B over cap: code=%d txt=%q", code, txt)
	}
	if stickyWarmN.Load() != 1 {
		t.Fatalf("over-cap key must NOT warm: count=%d want 1", stickyWarmN.Load())
	}
	if sb.openCount() != afterA+1 {
		t.Fatalf("key B should open once (cold): opens=%d want %d", sb.openCount(), afterA+1)
	}

	// A second B turn spawns again — proof it stayed cold (per-turn spawn).
	postWarmTurn(t, srv, "sonnet#warmtest", "sticky:B", convo(msg("user", "yo"), msg("assistant", "sup"), msg("user", "more")))
	if sb.openCount() != afterA+2 {
		t.Fatalf("over-cap key B must spawn per turn: opens=%d want %d", sb.openCount(), afterA+2)
	}
}

// TestOpenAIBareModelNeverWarm proves bare (non-sticky) models are wholly untouched by
// the warm feature: each turn spawns fresh, no sticky entry, no warm seat — ever.
func TestOpenAIBareModelNeverWarm(t *testing.T) {
	resetSticky()
	defer func(p bool) { openaiWarm = p }(openaiWarm)
	openaiWarm = true

	sb := &warmStub{}
	srv := newWarmTestServer(sb, time.Hour)

	postWarmTurn(t, srv, "sonnet", "", convo(msg("user", "hi")))
	postWarmTurn(t, srv, "sonnet", "", convo(msg("user", "hi"), msg("assistant", "hey"), msg("user", "again")))
	if n := sb.openCount(); n != 2 {
		t.Fatalf("bare model opens=%d want 2 (warm must not touch non-sticky turns)", n)
	}
	if stickyWarmN.Load() != 0 {
		t.Fatalf("bare model held a warm seat (count=%d) — must never", stickyWarmN.Load())
	}
	if len(stickyMap) != 0 {
		t.Fatalf("bare model created %d sticky entries — want 0", len(stickyMap))
	}
}
