//go:build linux

package shellenv

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// linuxClockTicksPerSecond is USER_HZ, the unit of /proc/<pid>/stat's
// starttime, which is 100 on every Linux architecture Go supports.
const linuxClockTicksPerSecond = 100

// procStat is the part of /proc/<pid>/stat a group member is judged by.
type procStat struct {
	state      byte
	pgid       int
	startTicks int64
}

// listGroupMembersStartedBefore returns each live member of pgid other than
// its leader whose start, in boot-clock ticks, is wholly before startedBefore.
// starttime counts whole ticks, so a member started in the tick that holds
// startedBefore is left out.
func listGroupMembersStartedBefore(pgid int, startedBefore time.Time) (map[int]int64, error) {
	var boot unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot); err != nil {
		return nil, fmt.Errorf("read the boot clock: %w", err)
	}
	cutoff := time.Duration(boot.Nano()) - time.Since(startedBefore)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	started := make(map[int]int64)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == pgid {
			continue
		}
		stat, ok := readProcStat(pid)
		if !ok || stat.pgid != pgid || stat.state == 'Z' {
			continue
		}
		if time.Duration(stat.startTicks+1)*time.Second/linuxClockTicksPerSecond >= cutoff {
			continue
		}
		started[pid] = stat.startTicks
	}
	return started, nil
}

// groupMemberStart returns pid's start, in boot-clock ticks, while it is a live
// member of pgid.
func groupMemberStart(pid, pgid int) (int64, bool) {
	stat, ok := readProcStat(pid)
	if !ok || stat.pgid != pgid || stat.state == 'Z' {
		return 0, false
	}
	return stat.startTicks, true
}

func readProcStat(pid int) (procStat, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	return parseProcStat(string(data))
}

// parseProcStat reads state, process group and starttime from one
// /proc/<pid>/stat line. The command name is parenthesized and may hold spaces
// or parentheses, so the fields after it are found from its last ')'.
func parseProcStat(line string) (procStat, bool) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return procStat{}, false
	}
	// Fields from state (field 3) on: state, ppid, pgrp, ..., starttime (22).
	fields := strings.Fields(line[end+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, false
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return procStat{}, false
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return procStat{}, false
	}
	return procStat{state: fields[0][0], pgid: pgid, startTicks: startTicks}, true
}
