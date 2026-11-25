//go:build unix

package shellenv

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// memberListTimeout bounds one listing of a process group's members.
const memberListTimeout = time.Second

// ShellCommandMembers holds the processes a started command's process group
// held besides its leader when they were recorded, each by its process id and
// its start time as ps reports it, so Kill never stops a process that took a
// recorded id after the recorded one exited.
type ShellCommandMembers struct {
	pgid    int
	started map[int]string
}

// RecordShellCommandMembers records the live processes in cmd's process group
// other than its leader.
func RecordShellCommandMembers(cmd *exec.Cmd) (*ShellCommandMembers, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("command was not started")
	}
	pgid := cmd.Process.Pid
	started, err := listGroupMemberStarts(pgid)
	if err != nil {
		return nil, err
	}
	return &ShellCommandMembers{pgid: pgid, started: started}, nil
}

// Kill stops every recorded process that is still in the group with the start
// time it was recorded with. When the group cannot be listed it stops nothing,
// and what is left stays for the owned-children probe to judge.
func (m *ShellCommandMembers) Kill() {
	if len(m.started) == 0 {
		return
	}
	current, err := listGroupMemberStarts(m.pgid)
	if err != nil {
		return
	}
	for pid, start := range m.started {
		if current[pid] == start {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// Release forgets the recorded processes. Kill does nothing after it.
func (m *ShellCommandMembers) Release() {
	m.started = nil
}

func listGroupMemberStarts(pgid int) (map[int]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), memberListTimeout)
	defer cancel()
	probe := exec.CommandContext(ctx, "ps", "-eo", "pgid=,pid=,stat=,lstart=")
	ConfigureShellCommand(probe)
	out, err := OutputShellCommand(probe)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("list process group members: %w", err)
	}
	return parseGroupMemberStarts(out, pgid)
}

// parseGroupMemberStarts reads ps output of pgid, pid, stat and lstart and
// returns the start time of each live member of pgid other than its leader.
func parseGroupMemberStarts(out []byte, pgid int) (map[int]string, error) {
	started := make(map[int]string)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("invalid process group member: %q", line)
		}
		memberPGID, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parse process group: %w", err)
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("parse process id: %w", err)
		}
		if memberPGID != pgid || pid == pgid || strings.HasPrefix(fields[2], "Z") {
			continue
		}
		started[pid] = strings.Join(fields[3:], " ")
	}
	return started, nil
}
