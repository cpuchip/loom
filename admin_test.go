package loom

import (
	"context"
	"testing"
	"time"
)

// TestOverviewListsResidentWithTail drives the real Overview client op against a
// real server: a NAMED resident that has taken a turn shows up with kind
// "resident", its model, an idle state, and the last reply as a tail.
func TestOverviewListsResidentWithTail(t *testing.T) {
	sb := &stubBackend{}
	_, url, token := startTestServer(t, map[string]Backend{"stub": sb}, 0)

	// A named resident (survives socket drops) that has said something.
	worker := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "worker-1"}
	sess, err := worker.Open(context.Background(), SessionOpts{Model: "stub-model"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()
	if _, err := sess.Send(context.Background(), "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}

	ov, err := (ConnectBackend{URL: url, Token: token}).Overview(context.Background())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	var got *SessionOverview
	for i := range ov {
		if ov[i].Handle != "" && ov[i].Name == "worker-1" {
			got = &ov[i]
		}
	}
	if got == nil {
		t.Fatalf("overview did not list the named resident; got %+v", ov)
	}
	if got.Kind != "resident" {
		t.Errorf("kind = %q, want resident", got.Kind)
	}
	if !got.Named {
		t.Error("a named resident should report Named=true")
	}
	if got.Model != "stub-model" {
		t.Errorf("model = %q, want stub-model", got.Model)
	}
	if got.State != "idle" {
		t.Errorf("state = %q, want idle (turn already finished)", got.State)
	}
	if got.Tail != "echo:hi" {
		t.Errorf("tail = %q, want the last reply echo:hi", got.Tail)
	}
}

// TestKillResidentByName proves Kill drops a resident by NAME (not just handle),
// reports kind "resident", closes the live process, and removes it from the
// serve-wide overview.
func TestKillResidentByName(t *testing.T) {
	sb := &stubBackend{}
	_, url, token := startTestServer(t, map[string]Backend{"stub": sb}, 0)

	worker := ConnectBackend{URL: url, Token: token, Agent: "stub", SessionName: "doomed"}
	sess, err := worker.Open(context.Background(), SessionOpts{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	kind, note, found, err := (ConnectBackend{URL: url, Token: token}).Kill(context.Background(), "doomed")
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !found {
		t.Fatal("kill should find the named resident")
	}
	if kind != "resident" {
		t.Errorf("kind = %q, want resident", kind)
	}
	if note == "" {
		t.Error("kill should report the semantics applied")
	}

	// The live process was dropped (killTarget calls Close synchronously).
	if stub := sb.session(0); stub == nil || !stub.isClosed() {
		t.Fatal("kill must Close the resident's live session")
	}

	// It is gone from the overview.
	ov, err := (ConnectBackend{URL: url, Token: token}).Overview(context.Background())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	for _, e := range ov {
		if e.Name == "doomed" {
			t.Fatalf("killed resident still in overview: %+v", e)
		}
	}
}

// TestKillMiss reports found=false (not an error) when nothing matches — the
// caller may own that handle in its own registry.
func TestKillMiss(t *testing.T) {
	_, url, token := startTestServer(t, map[string]Backend{"stub": &stubBackend{}}, 0)
	kind, _, found, err := (ConnectBackend{URL: url, Token: token}).Kill(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("kill miss should not error: %v", err)
	}
	if found {
		t.Fatal("kill of a nonexistent target should report found=false")
	}
	if kind != "" {
		t.Errorf("a miss should have no kind; got %q", kind)
	}
}

// TestStickyOverviewAndKill exercises the warm-sticky-seat surface directly (the
// OpenAI shim registry the ws `sessions` op never sees): a warm seat appears in
// the overview as kind "warm-seat", and a kill downgrades it to cold-resumable
// (its live process torn down) — never a hard destroy that would lose lineage.
func TestStickyOverviewAndKill(t *testing.T) {
	key := "sonnet#companion|sticky:companion"
	fs := &stubSession{id: "sticky-1", intCh: make(chan struct{}), started: make(chan struct{})}
	e := &stickyEntry{warm: fs, lastTurn: time.Now(), lastUsed: time.Now()}
	stickyMu.Lock()
	stickyMap[key] = e
	stickyMu.Unlock()
	stickyWarmN.Add(1)
	defer func() {
		stickyMu.Lock()
		delete(stickyMap, key)
		stickyMu.Unlock()
	}()

	ov := stickyOverview(time.Now())
	var got *SessionOverview
	for i := range ov {
		if ov[i].Handle == key {
			got = &ov[i]
		}
	}
	if got == nil {
		t.Fatalf("stickyOverview did not surface the warm seat; got %+v", ov)
	}
	if got.Kind != "warm-seat" {
		t.Errorf("kind = %q, want warm-seat", got.Kind)
	}
	if got.Model != "sonnet#companion" {
		t.Errorf("model = %q, want sonnet#companion", got.Model)
	}
	if got.Name != "companion" {
		t.Errorf("name = %q, want companion (parsed from the sticky key)", got.Name)
	}
	if got.State != "warm" {
		t.Errorf("state = %q, want warm", got.State)
	}

	// Kill by the friendly name downgrades to cold-resumable.
	note, hit := stickyKill("companion")
	if !hit {
		t.Fatal("stickyKill should match the seat by its friendly name")
	}
	if note == "" {
		t.Error("stickyKill should describe the downgrade")
	}
	e.mu.Lock()
	warm := e.warm
	e.mu.Unlock()
	if warm != nil {
		t.Fatal("kill of a warm seat must tear down the live process (downgrade to cold)")
	}
	if !fs.isClosed() {
		t.Fatal("downgrade must Close the warm session")
	}

	// A now-cold seat still reports hit=true (it exists; nothing warm to stop).
	if _, hit := stickyKill("companion"); !hit {
		t.Fatal("a cold-but-present seat should still report hit=true")
	}
}
