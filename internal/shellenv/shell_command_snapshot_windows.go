//go:build windows

package shellenv

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processSnapshot uintptr
type processSnapshotMarker uintptr

// PSS_THREAD_ENTRY from ProcessSnapshot.h, with no captured thread context.
type processSnapshotThread struct {
	ExitStatus               uint32
	TebBaseAddress           uintptr
	ProcessID                uint32
	ThreadID                 uint32
	AffinityMask             uintptr
	Priority                 int32
	BasePriority             int32
	LastSyscallFirstArgument uintptr
	LastSyscallNumber        uint16
	CreateTime               windows.Filetime
	ExitTime                 windows.Filetime
	KernelTime               windows.Filetime
	UserTime                 windows.Filetime
	Win32StartAddress        uintptr
	CaptureTime              windows.Filetime
	Flags                    uint32
	SuspendCount             uint16
	SizeOfContextRecord      uint16
	ContextRecord            uintptr
}

var (
	processSnapshotKernel    = windows.NewLazySystemDLL("kernel32.dll")
	procPssCaptureSnapshot   = processSnapshotKernel.NewProc("PssCaptureSnapshot")
	procPssWalkMarkerCreate  = processSnapshotKernel.NewProc("PssWalkMarkerCreate")
	procPssWalkSnapshot      = processSnapshotKernel.NewProc("PssWalkSnapshot")
	procPssWalkMarkerFree    = processSnapshotKernel.NewProc("PssWalkMarkerFree")
	procPssFreeSnapshot      = processSnapshotKernel.NewProc("PssFreeSnapshot")
	procGetProcessIDOfThread = processSnapshotKernel.NewProc("GetProcessIdOfThread")
	shellCommandThreadCalls  = processThreadCalls{
		openProcess:  windows.OpenProcess,
		waitProcess:  windows.WaitForSingleObject,
		capture:      captureProcessThreads,
		createMarker: createProcessSnapshotMarker,
		walk:         walkProcessThreads,
		openThread:   windows.OpenThread,
		threadOwner:  processThreadOwner,
		resume:       windows.ResumeThread,
		closeHandle:  windows.CloseHandle,
		freeMarker:   freeProcessSnapshotMarker,
		freeSnapshot: freeProcessSnapshot,
	}
)

type processThreadCalls struct {
	openProcess  func(uint32, bool, uint32) (windows.Handle, error)
	waitProcess  func(windows.Handle, uint32) (uint32, error)
	capture      func(windows.Handle) (processSnapshot, error)
	createMarker func() (processSnapshotMarker, error)
	walk         func(processSnapshot, processSnapshotMarker, *processSnapshotThread) error
	openThread   func(uint32, bool, uint32) (windows.Handle, error)
	threadOwner  func(windows.Handle) (uint32, error)
	resume       func(windows.Handle) (uint32, error)
	closeHandle  func(windows.Handle) error
	freeMarker   func(processSnapshotMarker) error
	freeSnapshot func(processSnapshot) error
}

func resumeProcessThreads(process *os.Process) (result error) {
	// Actual callers wait only after startup returns. KeepAlive also excludes
	// Go 1.25's process cleanup while acquiring this independently owned handle.
	defer func() { runtime.KeepAlive(process) }()
	if (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") ||
		unsafe.Sizeof(processSnapshotThread{}) != 120 ||
		unsafe.Offsetof(processSnapshotThread{}.ProcessID) != 16 ||
		unsafe.Offsetof(processSnapshotThread{}.CreateTime) != 52 ||
		unsafe.Offsetof(processSnapshotThread{}.SuspendCount) != 108 ||
		unsafe.Offsetof(processSnapshotThread{}.ContextRecord) != 112 {
		return fmt.Errorf("unsupported Windows PSS thread layout for %s", runtime.GOARCH)
	}
	calls := shellCommandThreadCalls
	pid := uint32(process.Pid)
	handle, err := calls.openProcess(windows.PROCESS_QUERY_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return fmt.Errorf("open process for thread discovery: %w", err)
	}
	defer func() {
		if err := calls.closeHandle(handle); err != nil {
			result = errors.Join(result, fmt.Errorf("close thread discovery process: %w", err))
		}
	}()
	status, err := calls.waitProcess(handle, 0)
	if err != nil {
		return fmt.Errorf("check process before thread discovery: %w", err)
	}
	if status != uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("process exited before thread discovery: %w", os.ErrProcessDone)
	}
	snapshot, err := calls.capture(handle)
	if err != nil {
		return fmt.Errorf("capture process threads: %w", err)
	}
	defer func() {
		if err := calls.freeSnapshot(snapshot); err != nil {
			result = errors.Join(result, fmt.Errorf("free process thread snapshot: %w", err))
		}
	}()
	marker, err := calls.createMarker()
	if err != nil {
		return fmt.Errorf("create process thread walk marker: %w", err)
	}
	defer func() {
		if err := calls.freeMarker(marker); err != nil {
			result = errors.Join(result, fmt.Errorf("free process thread walk marker: %w", err))
		}
	}()
	isResumed := false
	for {
		var entry processSnapshotThread
		if err := calls.walk(snapshot, marker, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			return fmt.Errorf("walk process threads: %w", err)
		}
		if entry.ProcessID != pid {
			return errors.New("captured thread belongs to another process")
		}
		thread, err := calls.openThread(windows.THREAD_SUSPEND_RESUME|windows.THREAD_QUERY_LIMITED_INFORMATION, false, entry.ThreadID)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			continue // The captured thread exited before OpenThread.
		}
		if err != nil {
			return fmt.Errorf("open captured thread: %w", err)
		}
		resumeErr := resumeOwnedProcessThread(calls, thread, pid)
		closeErr := calls.closeHandle(thread)
		if closeErr != nil {
			closeErr = fmt.Errorf("close captured thread: %w", closeErr)
		}
		if err := errors.Join(resumeErr, closeErr); err != nil {
			return err
		}
		isResumed = true
	}
	if !isResumed {
		return errors.New("no process threads resumed during startup")
	}
	return nil
}

func resumeOwnedProcessThread(calls processThreadCalls, thread windows.Handle, pid uint32) error {
	owner, err := calls.threadOwner(thread)
	if err != nil {
		return fmt.Errorf("query captured thread owner: %w", err)
	}
	if owner != pid {
		return errors.New("opened thread belongs to another process")
	}
	if _, err := calls.resume(thread); err != nil {
		return fmt.Errorf("resume captured thread: %w", err)
	}
	return nil
}

func captureProcessThreads(process windows.Handle) (processSnapshot, error) {
	if err := procPssCaptureSnapshot.Find(); err != nil {
		return 0, err
	}
	var snapshot processSnapshot
	const captureThreads = 0x80
	status, _, _ := procPssCaptureSnapshot.Call(uintptr(process), captureThreads, 0, uintptr(unsafe.Pointer(&snapshot)))
	if status := uint32(status); status != 0 {
		return 0, windows.Errno(status)
	}
	return snapshot, nil
}

func createProcessSnapshotMarker() (processSnapshotMarker, error) {
	if err := procPssWalkMarkerCreate.Find(); err != nil {
		return 0, err
	}
	var marker processSnapshotMarker
	status, _, _ := procPssWalkMarkerCreate.Call(0, uintptr(unsafe.Pointer(&marker)))
	if status := uint32(status); status != 0 {
		return 0, windows.Errno(status)
	}
	return marker, nil
}

func walkProcessThreads(snapshot processSnapshot, marker processSnapshotMarker, entry *processSnapshotThread) error {
	if err := procPssWalkSnapshot.Find(); err != nil {
		return err
	}
	const walkThreads = 3
	status, _, _ := procPssWalkSnapshot.Call(uintptr(snapshot), walkThreads, uintptr(marker), uintptr(unsafe.Pointer(entry)), unsafe.Sizeof(*entry))
	if status := uint32(status); status != 0 {
		return windows.Errno(status)
	}
	return nil
}

func processThreadOwner(thread windows.Handle) (uint32, error) {
	if err := procGetProcessIDOfThread.Find(); err != nil {
		return 0, err
	}
	pid, _, err := procGetProcessIDOfThread.Call(uintptr(thread))
	if uint32(pid) == 0 {
		return 0, err
	}
	return uint32(pid), nil
}

func freeProcessSnapshotMarker(marker processSnapshotMarker) error {
	if err := procPssWalkMarkerFree.Find(); err != nil {
		return err
	}
	status, _, _ := procPssWalkMarkerFree.Call(uintptr(marker))
	if status := uint32(status); status != 0 {
		return windows.Errno(status)
	}
	return nil
}

func freeProcessSnapshot(snapshot processSnapshot) error {
	if err := procPssFreeSnapshot.Find(); err != nil {
		return err
	}
	status, _, _ := procPssFreeSnapshot.Call(uintptr(windows.CurrentProcess()), uintptr(snapshot))
	if status := uint32(status); status != 0 {
		return windows.Errno(status)
	}
	return nil
}
