package loom

import (
	"os/exec"
	"sync"
)

// Child-reaping: a dying loom wrapper must NOT leave its agent child orphaned. This
// was the 2026-07-18 incident — a `loom run` wrapper died mid-run and its claude child
// idled, un-reaped, for 14h. The wrapper's deferred sess.Close() (which EOFs the child)
// only runs on a graceful return or a Go panic; a hard TerminateProcess, an os.Exit, or
// a fatal runtime throw all SKIP deferred cleanup — and that is exactly the class of
// death that stranded the child. So cleanup cannot live in a defer; it must be enforced
// by the OS. StartChild wires that OS-level enforcement at every persistent-agent spawn.

// childSpawnHook, if set, is invoked with each supervised child's PID the moment it
// starts. The single-run CLI (`loom run`) sets it to record spawned child PIDs into the
// run manifest, so a supervisor can later confirm a killed wrapper took its child with
// it. Process-global by design: a `loom run` process drives exactly one run. serve does
// not set it (its residents are tracked elsewhere), so this stays a no-op there.
var (
	hookMu         sync.Mutex
	childSpawnHook func(pid int)
)

// SetChildSpawnHook installs (fn) or clears (nil) the spawn callback. Safe to call
// concurrently; the CLI sets it before a turn and clears it after.
func SetChildSpawnHook(fn func(pid int)) {
	hookMu.Lock()
	childSpawnHook = fn
	hookMu.Unlock()
}

func fireChildSpawn(pid int) {
	hookMu.Lock()
	fn := childSpawnHook
	hookMu.Unlock()
	if fn != nil {
		fn(pid)
	}
}

// StartChild starts cmd with lifecycle supervision so a dying loom wrapper does not
// leave the agent child orphaned:
//
//   - Windows: the child is assigned to a process-wide Job Object created with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. When the wrapper exits by ANY means — clean,
//     panic, os.Exit, or a hard TerminateProcess that skips every deferred cleanup — the
//     OS closes the last handle to the job and reaps every process in it. This is the
//     fully-robust path (see reap_windows.go).
//   - Linux: the child gets PR_SET_PDEATHSIG=SIGKILL (best-effort; see reap_linux.go).
//   - macOS/BSD: no OS reaping is wired (no job objects, no pdeathsig) — documented, not
//     pretended-solved; the run manifest's stale heartbeat still makes the death visible
//     (see reap_other.go).
//
// It also fires the child-spawn hook so the run manifest can record the child PID.
//
// This replaces a bare cmd.Start() at every persistent-agent spawn site (claude, codex,
// copilot, opencode). It changes semantics DELIBERATELY: a dead wrapper is now a dead
// run, cleanly — not a zombie child idling indefinitely.
func StartChild(cmd *exec.Cmd) error {
	superviseChild(cmd) // pre-start: platform SysProcAttr (Linux pdeathsig); no-op elsewhere
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process != nil {
		adoptChild(cmd.Process.Pid) // post-start: platform job assignment (Windows); no-op elsewhere
		fireChildSpawn(cmd.Process.Pid)
	}
	return nil
}
