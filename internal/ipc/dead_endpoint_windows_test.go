//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deadOwnerPID returns a pid that has provably exited and has not been reused
// by a live process. Windows can recycle a just-freed pid, so the check is
// retried a bounded number of times.
func deadOwnerPID(t *testing.T) int {
	t.Helper()
	for attempt := 1; attempt <= 5; attempt++ {
		cmd := exec.Command("cmd", "/c", "exit", "0")
		if err := cmd.Run(); err != nil {
			t.Fatalf("run helper for a dead pid: %v", err)
		}
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			t.Fatalf("helper did not exit: state=%v", cmd.ProcessState)
		}
		if code := cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("helper exit code = %d, want 0", code)
		}
		pid := cmd.Process.Pid
		if !processAlive(pid) {
			return pid
		}
	}
	t.Fatal("could not obtain a provably dead helper pid after 5 attempts")
	return 0
}

func TestDialClassifiesDeadEndpointOwner(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "socket")
	pid := deadOwnerPID(t)
	content := fmt.Sprintf("127.0.0.1:1\n0123456789abcdef0123456789abcdef\n%d", pid)
	if err := os.WriteFile(endpoint, []byte(content), 0o600); err != nil {
		t.Fatalf("write endpoint file: %v", err)
	}

	client, err := Dial(endpoint)
	if client != nil {
		t.Fatal("Dial returned a client for a dead endpoint owner")
	}
	if !errors.Is(err, ErrEndpointProcessDead) {
		t.Fatalf("Dial error = %v, want ErrEndpointProcessDead", err)
	}
	if !strings.Contains(err.Error(), "is no longer running") {
		t.Fatalf("Dial error = %q, want the preserved dead-owner wording", err)
	}
}
