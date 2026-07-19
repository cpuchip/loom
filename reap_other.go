//go:build !windows && !linux

package loom

import "os/exec"

// macOS/BSD have no trivially-portable parent-death enforcement: no Job Objects and no
// PR_SET_PDEATHSIG. So on these platforms a dying wrapper CAN still orphan the agent
// child — we do not pretend otherwise. The run manifest's stale heartbeat with no
// finished_at still makes the death detectable, and `loom runs` surfaces it as
// heartbeat-stale, so the incident is legible even where the OS won't reap for us.
// (kqueue NOTE_EXIT watchers or a launchd-style supervisor could close this later.)
func superviseChild(cmd *exec.Cmd) {}
func adoptChild(pid int)           {}
