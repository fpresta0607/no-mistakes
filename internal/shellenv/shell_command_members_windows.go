//go:build windows

package shellenv

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ShellCommandMembers holds the processes a started command's job held besides
// its leader when they were recorded. Each is held by an open handle, so its
// process id cannot pass to another process before Kill.
type ShellCommandMembers struct {
	processes []windows.Handle
}

// RecordShellCommandMembers records the running processes in cmd's job other
// than its leader. A process that cannot be opened to be stopped is left out,
// so it stays for the owned-children probe to judge.
func RecordShellCommandMembers(cmd *exec.Cmd) (*ShellCommandMembers, error) {
	job, ok := shellCommandJob(cmd)
	if !ok || cmd.Process == nil {
		return nil, errNoAssignedShellCommandJob
	}
	return job.recordMembers(uint32(cmd.Process.Pid))
}

func (job *shellCommandJobState) recordMembers(leaderPID uint32) (*ShellCommandMembers, error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.isClosed || !job.isAssigned {
		return nil, errNoAssignedShellCommandJob
	}
	ids, err := jobProcessIDs(job.handle)
	if err != nil {
		return nil, err
	}
	members := &ShellCommandMembers{}
	for _, pid := range ids {
		if pid == leaderPID {
			continue
		}
		process, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		if !isRunningInJob(process, job.handle) {
			_ = windows.CloseHandle(process)
			continue
		}
		members.processes = append(members.processes, process)
	}
	return members, nil
}

// isRunningInJob reports whether process has not exited and is in job, which
// also rules out a listed id that already passed to a process outside it.
func isRunningInJob(process, job windows.Handle) bool {
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process, &exitCode); err != nil || exitCode != stillActiveExitCode {
		return false
	}
	var inJob int32
	ret, _, _ := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&inJob)))
	return ret != 0 && inJob != 0
}

// Kill stops every recorded process that is still running.
func (m *ShellCommandMembers) Kill() {
	for _, process := range m.processes {
		_ = windows.TerminateProcess(process, 1)
	}
}

// Release closes the recorded processes' handles. Kill does nothing after it.
func (m *ShellCommandMembers) Release() {
	for _, process := range m.processes {
		_ = windows.CloseHandle(process)
	}
	m.processes = nil
}
