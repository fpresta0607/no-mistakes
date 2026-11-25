//go:build unix

package shellenv

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ShellCommandMembers holds the processes a started command's process group
// held besides its leader when they were recorded, each by its process id and
// its start time, so Kill never stops a process that took a recorded id after
// the recorded one exited.
type ShellCommandMembers struct {
	pgid    int
	started map[int]int64
}

// RecordShellCommandMembers records the live processes in cmd's process group
// other than its leader that provably started before startedBefore. A member
// whose start cannot be placed before it is left out, so it stays for the
// owned-children probe to judge.
func RecordShellCommandMembers(cmd *exec.Cmd, startedBefore time.Time) (*ShellCommandMembers, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("command was not started")
	}
	pgid := cmd.Process.Pid
	started, err := listGroupMembersStartedBefore(pgid, startedBefore)
	if err != nil {
		return nil, err
	}
	return &ShellCommandMembers{pgid: pgid, started: started}, nil
}

// Kill stops every recorded process that is still in the group with the start
// time it was recorded with.
func (m *ShellCommandMembers) Kill() {
	for pid, start := range m.started {
		if current, ok := groupMemberStart(pid, m.pgid); ok && current == start {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// Release forgets the recorded processes. Kill does nothing after it.
func (m *ShellCommandMembers) Release() {
	m.started = nil
}
