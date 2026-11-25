//go:build unix

package shellenv

import (
	"testing"
	"time"
)

// Kill stops a recorded member and never a process whose start time differs
// from the recorded one, which is how a reused process id looks.
func TestShellCommandMembersKillStopsOnlyTheRecordedProcesses(t *testing.T) {
	// Arrange
	leader, pids := startMembersHelper(t, 2)
	time.Sleep(50 * time.Millisecond)
	members, err := RecordShellCommandMembers(leader, time.Now())
	if err != nil {
		t.Fatalf("RecordShellCommandMembers() error = %v", err)
	}
	stopped, reused := pids[0], pids[1]
	if _, ok := members.started[stopped]; !ok {
		t.Fatalf("recorded %v, want pid %d", members.started, stopped)
	}
	if _, ok := members.started[reused]; !ok {
		t.Fatalf("recorded %v, want pid %d", members.started, reused)
	}
	members.started[reused]++

	// Act
	members.Kill()

	// Assert
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := groupMemberStart(reused, leader.Process.Pid); !ok {
			t.Fatalf("Kill stopped pid %d whose start time differs from the recorded one", reused)
		}
		if _, ok := groupMemberStart(stopped, leader.Process.Pid); !ok {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("recorded pid %d still runs after Kill", stopped)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func recordedMemberPIDs(members *ShellCommandMembers) map[int]bool {
	recorded := make(map[int]bool, len(members.started))
	for pid := range members.started {
		recorded[pid] = true
	}
	return recorded
}
