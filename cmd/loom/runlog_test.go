package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cpuchip/loom"
)

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func mustReadManifest(t *testing.T, dir string) runManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m runManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestRecorderLifecycle(t *testing.T) {
	t.Setenv("LOOM_HOME", t.TempDir())

	rec, err := newRunRecorder([]string{"loom", "run", "hi"}, "/work", "claude", "sonnet")
	if err != nil {
		t.Fatalf("newRunRecorder: %v", err)
	}

	// artifacts created up front
	if _, err := os.Stat(filepath.Join(rec.dir, "manifest.json")); err != nil {
		t.Fatalf("manifest.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rec.dir, "output.log")); err != nil {
		t.Fatalf("output.log missing: %v", err)
	}

	m := mustReadManifest(t, rec.dir)
	if m.WrapperPID != os.Getpid() {
		t.Errorf("wrapper pid = %d, want %d", m.WrapperPID, os.Getpid())
	}
	if m.Backend != "claude" || m.Model != "sonnet" {
		t.Errorf("backend/model = %s/%s, want claude/sonnet", m.Backend, m.Model)
	}
	if len(m.Argv) != 3 {
		t.Errorf("argv = %v, want 3 tokens", m.Argv)
	}
	if m.FinishedAt != nil {
		t.Error("finished_at set before finish()")
	}

	// THE durability property: streamed output reaches the log BEFORE completion, so a
	// wrapper killed mid-run keeps everything already emitted.
	rec.logEvent(loom.Event{Kind: loom.EvAssistant, Text: "partial answer mid-run"})
	if !strings.Contains(readFileString(t, filepath.Join(rec.dir, "output.log")), "partial answer mid-run") {
		t.Error("assistant text not streamed to output.log before completion")
	}

	// child pid recorded (what a supervisor reads to confirm the reap)
	rec.addChildPID(4321)
	rec.addChildPID(4321) // idempotent
	if m = mustReadManifest(t, rec.dir); len(m.ChildPIDs) != 1 || m.ChildPIDs[0] != 4321 {
		t.Errorf("child pids = %v, want [4321]", m.ChildPIDs)
	}

	// heartbeat advances and is persisted
	first := mustReadManifest(t, rec.dir).HeartbeatAt
	time.Sleep(5 * time.Millisecond)
	rec.beat()
	if !mustReadManifest(t, rec.dir).HeartbeatAt.After(first) {
		t.Error("heartbeat did not advance on beat()")
	}

	// graceful finish → finished_at, usage, ok sentinel, final reply in log
	rec.finish(nil, loom.Reply{Text: "final answer text", CostUSD: 0.0123, Turns: 2, SessionID: "sess-1"})
	m = mustReadManifest(t, rec.dir)
	if m.FinishedAt == nil {
		t.Fatal("finished_at not set after finish()")
	}
	if m.Turns != 2 || m.SessionID != "sess-1" || m.CostUSD == 0 {
		t.Errorf("usage not recorded: %+v", m)
	}
	sent, err := loom.ReadSentinel(rec.dir)
	if err != nil || sent == nil {
		t.Fatalf("done sentinel: %v", err)
	}
	if sent.Status != "ok" {
		t.Errorf("sentinel status = %s, want ok", sent.Status)
	}
	if !strings.Contains(readFileString(t, filepath.Join(rec.dir, "output.log")), "final answer text") {
		t.Error("final reply not written to log")
	}
}

func TestRecorderFinishFailed(t *testing.T) {
	t.Setenv("LOOM_HOME", t.TempDir())
	rec, err := newRunRecorder([]string{"loom", "run"}, "", "claude", "")
	if err != nil {
		t.Fatalf("newRunRecorder: %v", err)
	}
	rec.finish(fakeErr("boom"), loom.Reply{})
	sent, err := loom.ReadSentinel(rec.dir)
	if err != nil || sent == nil || sent.Status != "failed" {
		t.Fatalf("want failed sentinel, got %+v (err %v)", sent, err)
	}
	if m := mustReadManifest(t, rec.dir); m.ExitError != "boom" {
		t.Errorf("exit_error = %q, want boom", m.ExitError)
	}
}

// A run whose recorder built its manifest but never finished (a killed wrapper) must NOT
// have a done sentinel — its absence is part of the death evidence.
func TestRecorderNoSentinelBeforeFinish(t *testing.T) {
	t.Setenv("LOOM_HOME", t.TempDir())
	rec, err := newRunRecorder([]string{"loom", "run"}, "", "claude", "")
	if err != nil {
		t.Fatalf("newRunRecorder: %v", err)
	}
	defer rec.finish(nil, loom.Reply{}) // clean up the heartbeat goroutine
	if _, err := os.Stat(filepath.Join(rec.dir, "done")); !os.IsNotExist(err) {
		t.Fatalf("done sentinel exists before finish() (err=%v)", err)
	}
}

func TestNewRunIDSortableAndUnique(t *testing.T) {
	base := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	a := newRunID(base)
	b := newRunID(base)
	if a == b {
		t.Errorf("run ids not unique for the same timestamp: %s", a)
	}
	if !strings.HasPrefix(a, "20260719-010203-") {
		t.Errorf("run id %q lacks the sortable timestamp prefix", a)
	}
}
