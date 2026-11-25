//go:build windows

package shellenv

import "golang.org/x/sys/windows"

func recordedMemberPIDs(members *ShellCommandMembers) map[int]bool {
	recorded := make(map[int]bool, len(members.processes))
	for _, process := range members.processes {
		if pid, err := windows.GetProcessId(process); err == nil {
			recorded[int(pid)] = true
		}
	}
	return recorded
}
