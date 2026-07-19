package main

// CLI-worker visibility: the whole-box overview must also show the loom workers a
// human (or a foreman) launched DIRECTLY on this box as `loom run …` processes —
// not through loom-mcp, not as serve residents. Those workers never touch the
// loom serve, so they are invisible to the serve overview; the fleet foreman's
// audition/walk workers are exactly this shape. loom-mcp scans the host process
// table for them (Win32_Process on Windows; a no-op elsewhere), parses the backend
// and model off each command line, and folds them into sessions_overview as
// kind="cli-worker" so the phone's Live view stops showing "Nothing running" while
// three workers grind.
//
// This file is the PURE, cross-platform half: the command-line tokenizer/parser and
// the overview mapping (unit-tested against captured Win32 command lines). The
// platform half — the actual process enumeration and the force-kill — lives in
// cliworkers_windows.go / cliworkers_other.go.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cpuchip/loom"
)

// cliWorker is one local `loom run` process discovered on the host — a worker
// launched directly (a foreman's audition/walk seat), not a loom-mcp commission
// and not a serve resident.
type cliWorker struct {
	PID       int
	Backend   string    // --agent (defaults to "claude", matching loom run's own default)
	Model     string    // --model ("" = the backend default)
	Dir       string    // --dir (the corpus/task dir; its basename is a legible name)
	Connect   string    // --connect ("" = a local spawn; set = it drives a remote serve)
	StartedAt time.Time // process creation time (zero if the platform didn't report it)
}

// cliWorkerKillWarning is the strong, show-BEFORE-you-tap consequence string for
// stopping a CLI worker. Unlike a commission (which loom-mcp drives and can tear
// down cleanly) this is someone else's live worker, killed mid-turn by force.
const cliWorkerKillWarning = "This force-kills a running worker (and its agent subprocess) mid-task. " +
	"It cannot be undone: the foreman that launched it will see it vanish, and any unsaved work in the turn is lost."

// psProc is one row of the host process query (see cliworkers_windows.go). It lives
// here so decodePSProcs — pure JSON handling — is compiled and tested on every OS.
type psProc struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
	Cmd     string `json:"cmd"`
}

// decodePSProcs tolerates ConvertTo-Json's three shapes: empty output (no matching
// processes), a lone object (PowerShell does NOT wrap a single item in an array),
// or a JSON array (two or more).
func decodePSProcs(out []byte) ([]psProc, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "[") {
		var rows []psProc
		if err := json.Unmarshal([]byte(s), &rows); err != nil {
			return nil, fmt.Errorf("decode process list: %w", err)
		}
		return rows, nil
	}
	var one psProc
	if err := json.Unmarshal([]byte(s), &one); err != nil {
		return nil, fmt.Errorf("decode process: %w", err)
	}
	return []psProc{one}, nil
}

// workersFromProcs parses every process row into a cliWorker, keeping only the ones
// whose subcommand is `run` (serve/enroll/pair/… are dropped). The PID + start time
// come from the row; the backend/model/dir/connect from the command line.
func workersFromProcs(rows []psProc) []cliWorker {
	var ws []cliWorker
	for _, r := range rows {
		w, ok := parseLoomRunCmdline(r.Cmd)
		if !ok {
			continue
		}
		w.PID = r.PID
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(r.Started)); err == nil {
			w.StartedAt = t
		}
		ws = append(ws, w)
	}
	return ws
}

// parseLoomRunCmdline extracts a cliWorker from a full process command line. It
// returns ok=false for anything that is not a `loom … run …` invocation (a serve,
// an enroll, a non-loom process), so serve/enroll are excluded structurally rather
// than by fragile substring matching on " run ". It understands every flag form Go's
// flag package accepts: --agent claude, --agent=claude, -agent claude, -agent=claude.
func parseLoomRunCmdline(cmdline string) (cliWorker, bool) {
	toks := tokenizeCmdline(cmdline)
	if len(toks) < 2 {
		return cliWorker{}, false
	}
	if !isLoomExe(toks[0]) {
		return cliWorker{}, false
	}
	if toks[1] != "run" { // the subcommand is os.Args[1]; only `run` is a worker
		return cliWorker{}, false
	}
	w := cliWorker{Backend: "claude"} // loom run defaults --agent to claude
	args := toks[2:]
	for i := 0; i < len(args); i++ {
		name, val, hasVal := splitFlag(args[i])
		switch name {
		case "agent", "model", "dir", "connect":
			if !hasVal && i+1 < len(args) {
				val = args[i+1]
				i++
			}
			switch name {
			case "agent":
				if strings.TrimSpace(val) != "" {
					w.Backend = val
				}
			case "model":
				w.Model = val
			case "dir":
				w.Dir = val
			case "connect":
				w.Connect = val
			}
		}
	}
	return w, true
}

// splitFlag classifies one command-line token. A flag token (starts with '-') yields
// its bare name (dashes stripped) and, if written --name=value, its inline value. A
// non-flag token (the prompt, a flag value) yields an empty name.
func splitFlag(arg string) (name, val string, hasVal bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", "", false
	}
	s := strings.TrimLeft(arg, "-")
	if s == "" { // a bare "-" or "--"
		return "", "", false
	}
	if eq := strings.IndexByte(s, '='); eq >= 0 {
		return s[:eq], s[eq+1:], true
	}
	return s, "", false
}

// tokenizeCmdline splits a Windows command line into arguments, honoring double
// quotes (so a quoted exe path or a quoted multi-word prompt stays one token) and
// treating backslashes as literal path characters. It does not decode Windows'
// backslash-before-quote escaping — the flag region we parse never uses it, and the
// quoted prompt (which might) is past the flags we read.
func tokenizeCmdline(s string) []string {
	var toks []string
	var b strings.Builder
	inQuote := false
	started := false
	flush := func() {
		if started {
			toks = append(toks, b.String())
			b.Reset()
			started = false
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			started = true // a quote opens a token even if it ends up empty
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inQuote:
			flush()
		default:
			b.WriteRune(r)
			started = true
		}
	}
	flush()
	return toks
}

// isLoomExe reports whether an argv[0] is the loom CLI (loom.exe / loom), so a
// same-named-but-different binary can't be mistaken for a worker.
func isLoomExe(path string) bool {
	base := strings.ToLower(baseName(path))
	return base == "loom.exe" || base == "loom"
}

// baseName returns the final path element, splitting on BOTH separators so it is
// correct for a Windows path even when this code is compiled on another OS (the
// tests run everywhere).
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// cliWorkerEntries maps discovered workers into unified overview entries. The kill
// target is "pid:<n>"; the name is the task/corpus dir's basename when known (so the
// phone shows "t2-frontend", not three identical "loom run" rows). When a correlate hook
// is supplied, each worker is matched to its `loom run` lifecycle record: on a confident
// match the card gains the run-id, a transcript tail (rendered by the app exactly like a
// commission's single-string tail), the derived status (running / heartbeat-stale), and
// usage if the manifest carries it. A worker with no confident match keeps the honest
// no-transcript note and the plain "running" state — the graceful fallback.
func cliWorkerEntries(ws []cliWorker, correlate func(cliWorker) (runCorrelation, bool)) []overviewEntry {
	out := make([]overviewEntry, 0, len(ws))
	for _, w := range ws {
		e := overviewEntry{
			Kind:       "cli-worker",
			Handle:     pidHandle(w.PID),
			Name:       cliWorkerName(w),
			Backend:    w.Backend,
			Model:      w.Model,
			State:      "running", // default: a loom run process is executing its single turn
			AgeSeconds: cliWorkerAge(w),
		}
		if correlate != nil {
			if c, ok := correlate(w); ok {
				e.RunID = c.RunID
				e.State = c.Status // running | heartbeat-stale (the wedged signal)
				e.Tail = c.Tail
				e.CostUSD = c.CostUSD
				e.Note = cliWorkerNoteCorrelated(w, c)
				out = append(out, e)
				continue
			}
		}
		e.Note = cliWorkerNoteUncorrelated(w)
		out = append(out, e)
	}
	return out
}

// cliWorkerAge is whole seconds since the worker process started, or 0 if the platform
// did not report a creation time.
func cliWorkerAge(w cliWorker) int {
	if w.StartedAt.IsZero() {
		return 0
	}
	if s := int(time.Since(w.StartedAt).Seconds()); s > 0 {
		return s
	}
	return 0
}

// pidHandle is the self-describing kill target for a CLI worker.
func pidHandle(pid int) string { return "pid:" + strconv.Itoa(pid) }

// cliWorkerName is the legible card title: the task dir's basename, else a generic label.
func cliWorkerName(w cliWorker) string {
	if d := strings.TrimRight(w.Dir, `/\`); d != "" {
		if b := baseName(d); b != "" {
			return b
		}
	}
	return "loom run worker"
}

// cliWorkerNoteCorrelated annotates a worker matched to its `loom run` record: it names
// the run-id (so a human can `loom runs tail <id>` for the full log), flags a stale
// heartbeat as the "may be wedged" signal, and still carries the force-kill consequence so
// the app can show it at confirm time.
func cliWorkerNoteCorrelated(w cliWorker, c runCorrelation) string {
	var n string
	if c.Status == "heartbeat-stale" {
		n = fmt.Sprintf("CLI worker (loom run, PID %d) — run %s. Its heartbeat is stale (no beat in >%ds): the worker may be WEDGED or its wrapper died. %s",
			w.PID, c.RunID, int(loom.HeartbeatStaleAfter.Seconds()), cliWorkerKillWarning)
	} else {
		n = fmt.Sprintf("CLI worker (loom run, PID %d) — run %s, live. %s",
			w.PID, c.RunID, cliWorkerKillWarning)
	}
	return appendConnect(n, w)
}

// cliWorkerNoteUncorrelated is the honest fallback when no `loom run` record matches the
// worker's PID — it predates lifecycle logging, its run dir is gone, or the only PID match
// failed the recycled-PID guard. There is no transcript to show; the kill consequence still
// applies.
func cliWorkerNoteUncorrelated(w cliWorker) string {
	n := fmt.Sprintf("CLI worker (loom run, PID %d) — no matching run record found (it predates lifecycle logging, or its run dir is unavailable), so there is no transcript to show. %s",
		w.PID, cliWorkerKillWarning)
	return appendConnect(n, w)
}

// appendConnect adds the remote-serve suffix when the worker drives one.
func appendConnect(n string, w cliWorker) string {
	if strings.TrimSpace(w.Connect) != "" {
		n += " (drives a remote serve at " + w.Connect + ")"
	}
	return n
}

// parsePIDTarget recognizes a CLI-worker kill target: "pid:<n>" (what the overview
// emits) or a bare positive integer (a human typing the PID).
func parsePIDTarget(target string) (int, bool) {
	t := strings.TrimSpace(target)
	t = strings.TrimPrefix(t, "pid:")
	if t == "" {
		return 0, false
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// backendModel renders a worker's identity as "backend/model" (or just "backend").
func backendModel(w cliWorker) string {
	if strings.TrimSpace(w.Model) != "" {
		return w.Backend + "/" + w.Model
	}
	return w.Backend
}
