//go:build unix

package shellenv

import (
	"bufio"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestParseGroupMemberStarts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		want    map[int]string
		wantErr bool
	}{
		{name: "empty", out: "", want: map[int]string{}},
		{
			name: "keeps live members of the group but the leader",
			out: " 100   100 Ss   Wed Nov 25 12:00:00 2026\n" +
				" 100   101 S    Wed Nov 25 12:00:01 2026\n" +
				" 100   102 Z    Wed Nov 25 12:00:02 2026\n" +
				" 200   201 S    Wed Nov 25 12:00:03 2026\n",
			want: map[int]string{101: "Wed Nov 25 12:00:01 2026"},
		},
		{name: "a line without a start time", out: "100 101 S\n", wantErr: true},
		{name: "a group that is not a number", out: "x 101 S Wed Nov 25 12:00:01 2026\n", wantErr: true},
		{name: "a pid that is not a number", out: "100 x S Wed Nov 25 12:00:01 2026\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGroupMemberStarts([]byte(tc.out), 100)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGroupMemberStarts() = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGroupMemberStarts() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseGroupMemberStarts() = %v, want %v", got, tc.want)
			}
			for pid, start := range tc.want {
				if got[pid] != start {
					t.Fatalf("parseGroupMemberStarts() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Kill stops a recorded member and never a process whose start time differs
// from the recorded one, which is how a reused process id looks.
func TestShellCommandMembersKillStopsOnlyTheRecordedProcesses(t *testing.T) {
	// Arrange
	leader := exec.Command("sh", "-c", "sleep 30 & echo $!; sleep 30 & echo $!; wait")
	ConfigureShellCommand(leader)
	stdout, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := StartShellCommand(leader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		TerminateShellCommandGroup(leader)
		_ = leader.Wait()
	})
	lines := bufio.NewScanner(stdout)
	pids := make([]int, 0, 2)
	for len(pids) < 2 && lines.Scan() {
		pid, err := strconv.Atoi(lines.Text())
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	if len(pids) != 2 {
		t.Fatalf("read pids %v, want two", pids)
	}
	members, err := RecordShellCommandMembers(leader)
	if err != nil {
		t.Fatalf("RecordShellCommandMembers() error = %v", err)
	}
	stopped, reused := pids[0], pids[1]
	if members.started[stopped] == "" || members.started[reused] == "" {
		t.Fatalf("recorded %v, want both of %v", members.started, pids)
	}
	members.started[reused] = "a start time the process never had"

	// Act
	members.Kill()

	// Assert
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := listGroupMemberStarts(leader.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if current[reused] == "" {
			t.Fatalf("Kill stopped pid %d whose start time differs from the recorded one", reused)
		}
		if current[stopped] == "" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("recorded pid %d still runs after Kill", stopped)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
