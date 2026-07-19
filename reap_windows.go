//go:build windows

package loom

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows child-reaping via a process-wide Job Object. We create ONE unnamed job with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and assign every spawned agent child to it, then
// keep the job handle open for the wrapper's whole life. We never close it explicitly:
// when the wrapper process terminates — cleanly, by panic, or by a hard TerminateProcess
// that skips deferred Go cleanup — the OS closes all of its handles, the job's last
// handle drops, and KILL_ON_JOB_CLOSE reaps every process in the job. This is the piece
// that turns "wrapper died → child orphaned 14h" into "wrapper died → child dead too."

var (
	jobOnce sync.Once
	jobH    syscall.Handle // the process-wide job; deliberately kept open, never closed
)

var (
	modkernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW      = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObj  = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj = modkernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess           = modkernel32.NewProc("OpenProcess")
	procCloseHandle           = modkernel32.NewProc("CloseHandle")
	procIsProcessInJob        = modkernel32.NewProc("IsProcessInJob")
)

const (
	_JobObjectExtendedLimitInformation  = 9
	_JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000
	_PROCESS_TERMINATE                  = 0x0001
	_PROCESS_SET_QUOTA                  = 0x0100
	_PROCESS_QUERY_INFORMATION          = 0x0400
)

// jobBasicLimitInformation mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION (winnt.h). The
// pointer-sized fields are uintptr so the struct layout matches on both 386 and amd64.
type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobExtendedLimitInformation mirrors JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// superviseChild is a no-op pre-start on Windows — the child joins the job AFTER Start
// (adoptChild), which is where the OS handle is available.
func superviseChild(cmd *exec.Cmd) {}

// ensureJob lazily creates the process-wide kill-on-close job. On any failure it leaves
// jobH == 0 and reaping degrades to a no-op (the run manifest still records the death) —
// StartChild never fails just because job setup did.
func ensureJob() syscall.Handle {
	jobOnce.Do(func() {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		job := syscall.Handle(h)
		var info jobExtendedLimitInformation
		info.BasicLimitInformation.LimitFlags = _JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		r, _, _ := procSetInformationJobObj.Call(
			uintptr(job),
			uintptr(_JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
		if r == 0 { // SetInformationJobObject failed — don't keep a job that won't reap
			procCloseHandle.Call(uintptr(job))
			return
		}
		jobH = job
	})
	return jobH
}

// adoptChild assigns the just-started child (by PID) to the kill-on-close job. Once the
// child is in the job, its own future children are auto-added too (unless they set
// JOB_OBJECT_LIMIT_BREAKAWAY, which the agent CLIs do not), so the whole agent tree is
// reaped together. There is a tiny race for grandchildren spawned in the window between
// Start and this assignment; we assign immediately after Start to keep it minimal.
func adoptChild(pid int) {
	job := ensureJob()
	if job == 0 {
		return
	}
	h, _, _ := procOpenProcess.Call(uintptr(_PROCESS_TERMINATE|_PROCESS_SET_QUOTA), 0, uintptr(pid))
	if h == 0 {
		return
	}
	defer procCloseHandle.Call(h)
	procAssignProcessToJobObj.Call(uintptr(job), h)
}

// processInJob reports whether pid is currently a member of loom's reaping job. It is a
// test/diagnostic helper — it proves the job-object wiring on the real path without
// having to kill the wrapper.
func processInJob(pid int) (bool, bool) {
	job := ensureJob()
	if job == 0 {
		return false, false
	}
	h, _, _ := procOpenProcess.Call(uintptr(_PROCESS_QUERY_INFORMATION), 0, uintptr(pid))
	if h == 0 {
		return false, false
	}
	defer procCloseHandle.Call(h)
	var in int32
	r, _, _ := procIsProcessInJob.Call(h, uintptr(job), uintptr(unsafe.Pointer(&in)))
	if r == 0 {
		return false, false
	}
	return in != 0, true
}
