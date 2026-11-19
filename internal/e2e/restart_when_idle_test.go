//go:build e2e

package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRestartWhenIdleWaitsOutOnlyTheBusyRefusal pins issue #212: a journey
// restarted the daemon as soon as its run's status turned terminal, while the
// daemon was still removing that run's worktree and holding its executor, so
// the restart was refused as busy and the journey failed. Only that refusal
// is waited out; any other outcome is the caller's to judge at once.
func TestRestartWhenIdleWaitsOutOnlyTheBusyRefusal(t *testing.T) {
	busy := "cannot atomically prepare daemon shutdown: " + daemonBusyRefusal + "; retry when idle or pass --force; refusing lifecycle operation"
	exitStatus := errors.New("exit status 1")
	for _, test := range []struct {
		name        string
		outcomes    []string
		wantCalls   int
		wantErr     bool
		wantOutput  string
		wantTimeout bool
	}{
		{name: "restarts at once when idle", outcomes: []string{"daemon restarted"}, wantCalls: 1, wantOutput: "daemon restarted"},
		{name: "waits out the busy window after a run finishes", outcomes: []string{busy, busy, "daemon restarted"}, wantCalls: 3, wantOutput: "daemon restarted"},
		{name: "never retries a refusal over an active run", outcomes: []string{"refusing daemon restart because 1 active pipeline runs are in progress"}, wantCalls: 1, wantErr: true},
		{name: "never retries another failure", outcomes: []string{"start daemon: daemon did not become ready"}, wantCalls: 1, wantErr: true},
		{name: "gives up once the deadline passes", outcomes: []string{busy}, wantErr: true, wantTimeout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			calls := 0
			restart := func() (string, error) {
				out := test.outcomes[min(calls, len(test.outcomes)-1)]
				calls++
				if out == "daemon restarted" {
					return out, nil
				}
				return out, exitStatus
			}
			timeout := time.Minute
			if test.wantTimeout {
				timeout = 50 * time.Millisecond
			}

			// Act
			out, err := restartWhenIdle(restart, timeout, time.Millisecond)

			// Assert
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, want error %v", err, test.wantErr)
			}
			if test.wantCalls > 0 && calls != test.wantCalls {
				t.Fatalf("restart attempts = %d, want %d", calls, test.wantCalls)
			}
			if test.wantOutput != "" && out != test.wantOutput {
				t.Fatalf("output = %q, want %q", out, test.wantOutput)
			}
			if test.wantTimeout && (calls < 2 || !strings.Contains(err.Error(), "still busy")) {
				t.Fatalf("after %d attempts err = %v, want a still-busy timeout after retrying", calls, err)
			}
		})
	}
}
