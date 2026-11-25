//go:build darwin

package shellenv

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// darwinZombie is SZOMB from <sys/proc.h>.
const darwinZombie = 5

// listGroupMembersStartedBefore returns each live member of pgid other than
// its leader whose start, in microseconds, is before startedBefore.
func listGroupMembersStartedBefore(pgid int, startedBefore time.Time) (map[int]int64, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, fmt.Errorf("list process group members: %w", err)
	}
	cutoff := startedBefore.UnixMicro()
	started := make(map[int]int64)
	for i := range procs {
		proc := &procs[i].Proc
		pid := int(proc.P_pid)
		start := darwinStartMicros(proc)
		if pid == pgid || proc.P_stat == darwinZombie || start >= cutoff {
			continue
		}
		started[pid] = start
	}
	return started, nil
}

// groupMemberStart returns pid's start, in microseconds, while it is a live
// member of pgid.
func groupMemberStart(pid, pgid int) (int64, bool) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(info.Proc.P_pid) != pid || int(info.Eproc.Pgid) != pgid || info.Proc.P_stat == darwinZombie {
		return 0, false
	}
	return darwinStartMicros(&info.Proc), true
}

func darwinStartMicros(proc *unix.ExternProc) int64 {
	return proc.P_starttime.Sec*1_000_000 + int64(proc.P_starttime.Usec)
}
