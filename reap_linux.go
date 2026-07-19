//go:build linux

package loom

import (
	"os/exec"
	"syscall"
)

// superviseChild asks the kernel to SIGKILL the child if THIS (parent) process dies.
// Best-effort: PR_SET_PDEATHSIG fires when the OS THREAD that started the child exits,
// and the Go runtime may migrate goroutines across threads — so it is not as airtight as
// the Windows job object. For a long-lived wrapper that spawns one agent child it holds
// in practice; it is the trivially-portable Linux complement, not a full guarantee. The
// run manifest's stale heartbeat remains the backstop verdict either way.
func superviseChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// adoptChild is a no-op on Linux — reaping is armed pre-start via Pdeathsig.
func adoptChild(pid int) {}
