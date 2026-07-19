//go:build windows

package loom

import (
	"os/exec"
	"testing"
)

// On Windows the reaping guarantee is membership in loom's kill-on-close Job Object: when
// the wrapper dies, the OS closes the job handle and every process in the job dies. Prove
// the membership directly (via IsProcessInJob) so the mechanism is verified on the real
// path without having to kill the test process itself.
func TestStartChildAssignsToKillOnCloseJob(t *testing.T) {
	SetChildSpawnHook(nil)
	// a child that stays alive long enough to query
	cmd := exec.Command("cmd", "/c", "ping -n 4 127.0.0.1 >NUL")
	if err := StartChild(cmd); err != nil {
		t.Fatalf("StartChild: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	in, ok := processInJob(cmd.Process.Pid)
	if !ok {
		t.Skip("IsProcessInJob query unavailable in this environment")
	}
	if !in {
		t.Fatalf("child pid %d is NOT a member of loom's reaping job — a dying wrapper would orphan it", cmd.Process.Pid)
	}
}
