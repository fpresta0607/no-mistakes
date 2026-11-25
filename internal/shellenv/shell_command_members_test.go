//go:build unix || windows

package shellenv

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// Issue #277: a member counts as the command's own only when it was created
// before the moment the record names, however long the record itself takes,
// so a process started after the agent reported it had started is never
// recorded.
func TestRecordShellCommandMembersKeepsOnlyMembersCreatedBeforeTheCutoff(t *testing.T) {
	// Arrange
	leader, next, pids := startMembersHelperOnCue(t)
	before := next()
	time.Sleep(50 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(50 * time.Millisecond)
	after := next()

	// Act
	members, err := RecordShellCommandMembers(leader, cutoff)
	if err != nil {
		t.Fatalf("RecordShellCommandMembers() error = %v", err)
	}
	t.Cleanup(members.Release)

	// Assert: on Windows the leader's console host is a member created before
	// the cutoff too.
	recorded := recordedMemberPIDs(members)
	if !recorded[before] || recorded[after] {
		t.Fatalf("recorded pids %v of children %v, want pid %d created before the cutoff and not pid %d created after it", recorded, *pids, before, after)
	}
}

// startMembersHelper starts a helper leader with count children and returns
// the leader and the children's pids.
func startMembersHelper(t *testing.T, count int) (*exec.Cmd, []int) {
	t.Helper()
	leader, next, _ := startMembersHelperOnCue(t)
	pids := make([]int, 0, count)
	for range count {
		pids = append(pids, next())
	}
	return leader, pids
}

// startMembersHelperOnCue starts a helper leader that starts one long-lived
// child each time next is called and returns the child's pid.
func startMembersHelperOnCue(t *testing.T) (*exec.Cmd, func() int, *[]int) {
	t.Helper()
	leader := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestShellCommandMembersHelper$")
	leader.Env = append(os.Environ(), "NM_SHELLENV_MEMBERS_HELPER=leader")
	ConfigureShellCommand(leader)
	cue, err := leader.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := StartShellCommand(leader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cue.Close()
		TerminateShellCommandGroup(leader)
		_ = leader.Wait()
	})
	lines := bufio.NewScanner(stdout)
	pids := []int{}
	next := func() int {
		t.Helper()
		if _, err := io.WriteString(cue, "start\n"); err != nil {
			t.Fatal(err)
		}
		if !lines.Scan() {
			t.Fatalf("helper leader ended before starting a child: %v", lines.Err())
		}
		pid, err := strconv.Atoi(lines.Text())
		if err != nil {
			t.Fatalf("helper leader wrote %q, want a pid", lines.Text())
		}
		pids = append(pids, pid)
		return pid
	}
	return leader, next, &pids
}

func TestShellCommandMembersHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_MEMBERS_HELPER") {
	case "leader":
		cues := bufio.NewScanner(os.Stdin)
		for cues.Scan() {
			child := exec.Command(os.Args[0], "-test.run=^TestShellCommandMembersHelper$")
			child.Env = append(os.Environ(), "NM_SHELLENV_MEMBERS_HELPER=child")
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			_, _ = io.WriteString(os.Stdout, strconv.Itoa(child.Process.Pid)+"\n")
		}
		os.Exit(0)
	case "child":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}
