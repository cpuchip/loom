//go:build !windows

package main

// CLI-worker enumeration is a Windows facility — the walk/audition fleet runs there
// via a Win32_Process query. On other platforms it is a no-op so the overview still
// builds cleanly (it simply lists no CLI workers), and a kill target that looks like
// a PID is refused rather than silently doing nothing.

import (
	"context"
	"fmt"
)

func listCLIWorkers(context.Context) ([]cliWorker, error) { return nil, nil }

func killCLIWorker(pid int) error {
	return fmt.Errorf("stopping a CLI worker (PID %d) is only supported on Windows", pid)
}
