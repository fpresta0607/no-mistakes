package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// TestWaitAfterParseErrorKeepsStderrTheAgentAlreadyWrote pins issue #184: a
// parse error, a cancelled run among them, closed stderr's read end before its
// reader drained it, so a refusal the agent had already printed there was
// discarded and the run surfaced as a bare context cancellation.
func TestWaitAfterParseErrorKeepsStderrTheAgentAlreadyWrote(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' 'written before the stop' >&2\necho ready > ready.txt\nsleep 100\n", strings.Join([]string{
		"@echo off",
		"echo written before the stop 1>&2",
		"echo ready>ready.txt",
		"ping -n 101 127.0.0.1 > nul",
	}, "\r\n"))
	cmd := exec.CommandContext(context.Background(), bin)
	cmd.Dir = dir
	shellenv.ConfigureShellCommand(cmd)
	started, err := startNativeAgentCommand(cmd, "", nil, false)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	defer started.closePipes()
	defer started.terminate()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "ready.txt")); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake agent never wrote ready.txt")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Act
	returned := make(chan error, 1)
	go func() { returned <- started.waitAfterParseError(context.Canceled) }()
	// Read stderr only after waitAfterParseError has had its chance to discard
	// it: it returns at once when it closes the pipe itself, and otherwise waits
	// for this reader within the command's WaitDelay.
	var waitErr error
	isReturned := false
	select {
	case waitErr = <-returned:
		isReturned = true
	case <-time.After(time.Second):
	}
	stderr, readErr := io.ReadAll(started.stderr)
	if !isReturned {
		select {
		case waitErr = <-returned:
		case <-time.After(10 * time.Second):
			t.Fatal("waitAfterParseError did not return after stderr was drained")
		}
	}

	// Assert
	if readErr != nil || !strings.Contains(string(stderr), "written before the stop") {
		t.Fatalf("stderr = %q, read error = %v, want what the agent wrote before the stop", stderr, readErr)
	}
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("waitAfterParseError = %v, want the parse error", waitErr)
	}
}

// Issue #277: Claude Code emits init again for each query it starts in the
// same process, so a record that failed at the first init must not be retried
// at a later one, when it would take work the agent started for the CLI's own
// and stop it at the result grace.
func TestRecordCLIProcessesIsNotRetriedAfterAFailure(t *testing.T) {
	// Arrange
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestClaudeStdinHelper$")
	cmd.Env = append(os.Environ(), "NM_CLAUDE_STDIN_HELPER=init-result-with-background-child")
	shellenv.ConfigureShellCommand(cmd)
	command := &nativeAgentCommand{cmd: cmd}
	if err := command.recordCLIProcesses(time.Now()); err == nil {
		t.Fatal("recordCLIProcesses() before the start succeeded, want a failed record")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := shellenv.StartShellCommand(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shellenv.TerminateShellCommandGroup(cmd)
		_ = cmd.Wait()
	})
	lines := bufio.NewScanner(stdout)
	for lines.Scan() && !strings.Contains(lines.Text(), `"type":"result"`) {
	}

	// Act
	if err := command.recordCLIProcesses(time.Now()); err != nil {
		t.Fatalf("recordCLIProcesses() after a failed one = %v, want nothing recorded", err)
	}
	command.stopLeader()
	_ = cmd.Wait()

	// Assert
	// A probe that times out on a slow runner after it saw the work still
	// refuses on that work, as the turn's own probe does.
	active, members, err := shellenv.ShellCommandGroupActive(cmd, ownedChildrenSettleWindow)
	if err != nil && members == "" {
		t.Fatalf("ShellCommandGroupActive() error = %v", err)
	}
	if err == nil && !active {
		t.Fatal("the work the agent started after init was stopped with the leader, want it left for the owned-children probe")
	}
}
