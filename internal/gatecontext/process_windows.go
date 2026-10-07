//go:build windows

package gatecontext

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func processInfo(pid int) (ProcessInfo, error) {
	if pid <= 1 {
		return ProcessInfo{}, nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ProcessInfo{}, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return ProcessInfo{}, err
	}
	for {
		if int(entry.ProcessID) == pid {
			return ProcessInfo{ParentPID: int(entry.ParentProcessID), Started: processStarted(pid)}, nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				return ProcessInfo{}, nil
			}
			return ProcessInfo{}, err
		}
	}
}

// processStarted returns the process creation time, or zero when the process
// cannot be opened (it exited, or access is denied).
func processStarted(pid int) time.Time {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}
	}
	return time.Unix(0, created.Nanoseconds())
}
