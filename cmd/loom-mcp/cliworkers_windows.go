//go:build windows

package main

// Windows process enumeration + force-kill for CLI workers. The walk/audition
// fleet runs on Windows, so this is the live path. We shell out to PowerShell's
// Win32_Process CIM query (no cgo, no extra Go dependency) and force-kill the whole
// process TREE — a `loom run` spawns its agent (claude/node) as a child, so killing
// loom alone would orphan the worker doing the actual work.

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// listWorkersPS enumerates every loom.exe process with its PID, creation time
// (round-trip ISO 8601, parseable as RFC3339Nano), and full command line. -Compress
// keeps the frame small; a lone match is emitted as an object, many as an array —
// decodePSProcs handles both.
const listWorkersPS = `Get-CimInstance Win32_Process -Filter "Name='loom.exe'" | ` +
	`ForEach-Object { [PSCustomObject]@{ pid = $_.ProcessId; started = $_.CreationDate.ToString('o'); cmd = $_.CommandLine } } | ` +
	`ConvertTo-Json -Depth 3 -Compress`

// listCLIWorkers queries the host process table and returns the live `loom run`
// workers (serve/enroll/etc. are filtered out by workersFromProcs).
func listCLIWorkers(ctx context.Context) ([]cliWorker, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", listWorkersPS).Output()
	if err != nil {
		return nil, fmt.Errorf("query loom.exe processes: %w", err)
	}
	rows, err := decodePSProcs(out)
	if err != nil {
		return nil, err
	}
	return workersFromProcs(rows), nil
}

// killCLIWorker force-stops a worker by PID, INCLUDING its agent subprocess tree.
// taskkill /T walks the child tree (the spawned claude/node), /F forces termination
// — the honest "kill the worker" that a plain TerminateProcess on loom would miss
// (it would leave the agent orphaned and still running).
func killCLIWorker(pid int) error {
	out, err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill /F /T /PID %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}
