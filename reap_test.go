package loom

import (
	"os/exec"
	"runtime"
	"sync"
	"testing"
)

// trivialCmd is a portable, fast-exiting child for exercising StartChild's contract.
func trivialCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit", "0")
	}
	return exec.Command("sh", "-c", "exit 0")
}

func TestStartChildFiresHookAndStarts(t *testing.T) {
	var mu sync.Mutex
	var got []int
	SetChildSpawnHook(func(pid int) { mu.Lock(); got = append(got, pid); mu.Unlock() })
	defer SetChildSpawnHook(nil)

	cmd := trivialCmd()
	if err := StartChild(cmd); err != nil {
		t.Fatalf("StartChild: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child wait: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != pid {
		t.Fatalf("spawn hook pids = %v, want exactly [%d]", got, pid)
	}
}

// A child that never starts must NOT fire the spawn hook (a supervisor would otherwise
// record a phantom pid).
func TestStartChildBadBinaryNoHook(t *testing.T) {
	SetChildSpawnHook(func(pid int) { t.Fatalf("hook fired for a child that never started (pid %d)", pid) })
	defer SetChildSpawnHook(nil)
	cmd := exec.Command("this-binary-does-not-exist-loom-test-xyz")
	if err := StartChild(cmd); err == nil {
		t.Fatal("StartChild should fail for a missing binary")
	}
}

func TestFireChildSpawnNilSafe(t *testing.T) {
	SetChildSpawnHook(nil)
	fireChildSpawn(123) // must not panic with no hook installed
}
