package agent

import (
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
