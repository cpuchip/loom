package main

// Unit proofs for the CLI-worker parser + mapping — the pure half that turns a
// captured Win32 command line into a cli-worker overview entry. The command lines
// below are REAL shapes captured from the live box (a walk/audition worker and the
// serve), plus the flag-form variants Go's flag package accepts.

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A worker command line in the shape a foreman's `loom run` seat actually takes
// (modeled on a real captured Win32_Process CommandLine): UNQUOTED exe path,
// double-dash space-separated flags with a bool flag interspersed, --dir, and a
// quoted multi-word prompt.
const realWalkWorkerCmd = `C:\code\loom\loom.exe run --agent claude --model sonnet --skip-permissions --dir C:/tmp/aud/t2-frontend/claude "You are in a timed audition. Read brief.md and constitution.md first, then build ONLY the deliverable file(s)."`

// A serve command line: QUOTED exe path, single-dash flags. Must NOT be seen as a
// worker (the subcommand is `serve`, not `run`).
const realServeCmd = `"C:\code\loom\loom.exe" serve -listen host:7791 -token-file C:\tokens\loom-serve-tokens -openai-warm`

func TestParseRealWalkWorker(t *testing.T) {
	w, ok := parseLoomRunCmdline(realWalkWorkerCmd)
	if !ok {
		t.Fatal("the real walk worker command line must parse as a loom run worker")
	}
	if w.Backend != "claude" {
		t.Errorf("backend = %q, want claude", w.Backend)
	}
	if w.Model != "sonnet" {
		t.Errorf("model = %q, want sonnet", w.Model)
	}
	if w.Dir == "" || baseName(w.Dir) != "claude" {
		t.Errorf("dir = %q, want it captured (ending in the audition dir)", w.Dir)
	}
}

func TestParseRealServeIsNotAWorker(t *testing.T) {
	if _, ok := parseLoomRunCmdline(realServeCmd); ok {
		t.Fatal("a `loom serve` process must NOT be classified as a run worker")
	}
}

func TestParseFlagForms(t *testing.T) {
	cases := []struct {
		name         string
		cmd          string
		backend      string
		model        string
		wantIsWorker bool
	}{
		{"double-dash space", `loom.exe run --agent codex --model gpt-5.6 "p"`, "codex", "gpt-5.6", true},
		{"double-dash equals", `loom.exe run --agent=codex --model=gpt-5.6 "p"`, "codex", "gpt-5.6", true},
		{"single-dash space", `loom.exe run -agent agy -model flash "p"`, "agy", "flash", true},
		{"single-dash equals", `loom.exe run -agent=agy -model=flash "p"`, "agy", "flash", true},
		{"agent omitted defaults claude", `loom.exe run "just a prompt"`, "claude", "", true},
		{"model omitted", `loom.exe run --agent local "p"`, "local", "", true},
		{"quoted exe path", `"C:\p\loom.exe" run --agent claude "p"`, "claude", "", true},
		{"connect present still local shape", `loom.exe run --connect ws://h:7791 --agent claude --model sonnet "p"`, "claude", "sonnet", true},
		{"enroll is not a worker", `loom.exe enroll --serve --listen h:7779`, "", "", false},
		{"chat is not a run worker", `loom.exe chat --agent claude`, "", "", false},
		{"bare serve is not a worker", `loom.exe serve -listen h:7777`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, ok := parseLoomRunCmdline(c.cmd)
			if ok != c.wantIsWorker {
				t.Fatalf("isWorker = %v, want %v", ok, c.wantIsWorker)
			}
			if !ok {
				return
			}
			if w.Backend != c.backend {
				t.Errorf("backend = %q, want %q", w.Backend, c.backend)
			}
			if w.Model != c.model {
				t.Errorf("model = %q, want %q", w.Model, c.model)
			}
		})
	}
}

func TestParseConnectCaptured(t *testing.T) {
	w, ok := parseLoomRunCmdline(`loom.exe run --connect ws://host:7791 --agent claude "p"`)
	if !ok {
		t.Fatal("want a worker")
	}
	if w.Connect != "ws://host:7791" {
		t.Errorf("connect = %q, want ws://host:7791", w.Connect)
	}
}

func TestParseNonLoomBinaryRejected(t *testing.T) {
	if _, ok := parseLoomRunCmdline(`C:\evil\notloom.exe run --agent claude "p"`); ok {
		t.Fatal("a non-loom binary named '…run…' must not be taken for a loom worker")
	}
}

func TestTokenizeCmdline(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`loom.exe run --agent claude`, []string{"loom.exe", "run", "--agent", "claude"}},
		{`"C:\p q\loom.exe" serve -x`, []string{`C:\p q\loom.exe`, "serve", "-x"}},
		{`loom.exe run "multi word prompt"`, []string{"loom.exe", "run", "multi word prompt"}},
		{`  spaced   out  `, []string{"spaced", "out"}},
	}
	for _, c := range cases {
		got := tokenizeCmdline(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// TestCLIWorkerEntriesUncorrelated proves the fallback path: with no correlation (nil
// hook), a worker maps to a running cli-worker card carrying NO transcript and the honest
// no-record note + kill consequence — byte-for-byte the pre-transcript behavior.
func TestCLIWorkerEntriesUncorrelated(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	ws := []cliWorker{
		{PID: 18652, Backend: "claude", Model: "sonnet", Dir: `C:/x/aud/t2-frontend/claude`, StartedAt: start},
		{PID: 4242, Backend: "local", Model: "", Dir: "", StartedAt: time.Time{}},
	}
	entries := cliWorkerEntries(ws, nil)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	e0 := entries[0]
	if e0.Kind != "cli-worker" {
		t.Errorf("kind = %q, want cli-worker", e0.Kind)
	}
	if e0.Handle != "pid:18652" {
		t.Errorf("handle = %q, want pid:18652", e0.Handle)
	}
	if e0.Name != "claude" { // basename of the --dir
		t.Errorf("name = %q, want the dir basename 'claude'", e0.Name)
	}
	if e0.Backend != "claude" || e0.Model != "sonnet" {
		t.Errorf("backend/model = %q/%q, want claude/sonnet", e0.Backend, e0.Model)
	}
	if e0.State != "running" {
		t.Errorf("state = %q, want running", e0.State)
	}
	if e0.AgeSeconds < 80 || e0.AgeSeconds > 100 {
		t.Errorf("age = %d, want ~90s", e0.AgeSeconds)
	}
	if e0.Tail != "" || e0.RunID != "" {
		t.Errorf("uncorrelated cli-worker must carry no tail/run_id; got tail=%q run_id=%q", e0.Tail, e0.RunID)
	}
	if !strings.Contains(e0.Note, "no transcript") || !strings.Contains(e0.Note, "force-kill") {
		t.Errorf("note must state no-transcript AND the kill consequence; got %q", e0.Note)
	}
	// A worker with no --dir falls back to a generic name and a zero age.
	if entries[1].Name != "loom run worker" {
		t.Errorf("fallback name = %q, want 'loom run worker'", entries[1].Name)
	}
	if entries[1].AgeSeconds != 0 {
		t.Errorf("unknown start time should give age 0; got %d", entries[1].AgeSeconds)
	}
}

// TestCLIWorkerEntriesCorrelated proves the transcript path: when the correlate hook
// returns a match, the card carries the run-id, the transcript tail (the app renders it
// like a commission's string tail), the derived status as State, usage, and a note that
// names the run + flags a wedged worker.
func TestCLIWorkerEntriesCorrelated(t *testing.T) {
	ws := []cliWorker{
		{PID: 18652, Backend: "claude", Model: "sonnet", Dir: `C:/x/aud/t2-frontend/claude`},
		{PID: 18777, Backend: "codex", Model: "gpt-5.6", Dir: `C:/x/aud/t1-backend/codex`},
	}
	correlate := func(w cliWorker) (runCorrelation, bool) {
		switch w.PID {
		case 18652:
			return runCorrelation{RunID: "20260719-010000-abc", Status: "running",
				Tail: "→ tool: Bash\nassistant: building the file"}, true
		case 18777:
			return runCorrelation{RunID: "20260719-011500-def", Status: "heartbeat-stale",
				Tail: "· thinking: hmm", CostUSD: 0.42}, true
		}
		return runCorrelation{}, false
	}
	entries := cliWorkerEntries(ws, correlate)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}

	live := entries[0]
	if live.RunID != "20260719-010000-abc" {
		t.Errorf("run_id = %q, want the correlated id", live.RunID)
	}
	if live.State != "running" {
		t.Errorf("state = %q, want running", live.State)
	}
	if live.Tail != "→ tool: Bash\nassistant: building the file" {
		t.Errorf("tail = %q, want the correlated transcript tail", live.Tail)
	}
	if !strings.Contains(live.Note, "20260719-010000-abc") || !strings.Contains(live.Note, "live") {
		t.Errorf("live note should name the run and say live; got %q", live.Note)
	}

	stale := entries[1]
	if stale.State != "heartbeat-stale" {
		t.Errorf("state = %q, want heartbeat-stale", stale.State)
	}
	if stale.CostUSD != 0.42 {
		t.Errorf("cost = %v, want 0.42 passed through", stale.CostUSD)
	}
	if !strings.Contains(stale.Note, "WEDGED") {
		t.Errorf("stale-heartbeat note should flag a possibly wedged worker; got %q", stale.Note)
	}
}

func TestParsePIDTarget(t *testing.T) {
	cases := []struct {
		in   string
		pid  int
		want bool
	}{
		{"pid:18652", 18652, true},
		{"18652", 18652, true},
		{" pid:42 ", 42, true},
		{"commission-abc123", 0, false},
		{"ws-aaa", 0, false},
		{"pid:0", 0, false},
		{"pid:-5", 0, false},
		{"", 0, false},
		{"companion", 0, false},
	}
	for _, c := range cases {
		pid, ok := parsePIDTarget(c.in)
		if ok != c.want || pid != c.pid {
			t.Errorf("parsePIDTarget(%q) = (%d,%v), want (%d,%v)", c.in, pid, ok, c.pid, c.want)
		}
	}
}

func TestDecodePSProcs(t *testing.T) {
	// empty output → no processes
	if rows, err := decodePSProcs([]byte("  \n")); err != nil || rows != nil {
		t.Errorf("empty output should give (nil,nil); got (%v,%v)", rows, err)
	}
	// a lone object (PowerShell does not wrap a single match in an array)
	single := `{"pid":18652,"started":"2026-07-19T00:29:18.7440250-05:00","cmd":"loom.exe run --agent claude --model sonnet \"p\""}`
	rows, err := decodePSProcs([]byte(single))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PID != 18652 {
		t.Fatalf("single-object decode = %+v", rows)
	}
	// an array of two
	arr := `[` + single + `,{"pid":2296,"started":"2026-07-19T00:31:59-05:00","cmd":"loom.exe serve -listen h"}]`
	rows, err = decodePSProcs([]byte(arr))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].PID != 2296 {
		t.Fatalf("array decode = %+v", rows)
	}
}

// TestWorkersFromProcs proves the end-to-end row→worker filter: real start-time
// parsing, run-only filtering, and PID attachment.
func TestWorkersFromProcs(t *testing.T) {
	rows := []psProc{
		{PID: 18652, Started: "2026-07-19T00:29:18.7440250-05:00", Cmd: realWalkWorkerCmd},
		{PID: 2296, Started: "2026-07-19T00:31:59-05:00", Cmd: realServeCmd},
	}
	ws := workersFromProcs(rows)
	if len(ws) != 1 {
		t.Fatalf("want only the run worker (serve filtered out); got %d", len(ws))
	}
	if ws[0].PID != 18652 || ws[0].Backend != "claude" || ws[0].Model != "sonnet" {
		t.Errorf("worker = %+v", ws[0])
	}
	if ws[0].StartedAt.IsZero() {
		t.Error("start time should have parsed from the ISO round-trip string")
	}
}
