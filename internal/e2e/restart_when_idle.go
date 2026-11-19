//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"time"
)

// daemonBusyRefusal is how the daemon refuses a lifecycle operation while it
// still holds an admission, a queued run or a run's executor.
const daemonBusyRefusal = "refusing shutdown because pipeline admission or execution is in progress"

// RestartDaemonWhenIdle restarts the daemon once it has let go of every run.
//
// A run's status turns terminal before the daemon has removed the run's
// worktree and evidence and released its executor, and until then the daemon
// rightly refuses a restart as busy (issue #212). A caller that has seen only
// the terminal status therefore retries that one refusal until the daemon is
// idle. Any other failure, a refusal over a run still active included, fails
// the test at once.
func (h *Harness) RestartDaemonWhenIdle() string {
	h.t.Helper()
	out, err := restartWhenIdle(func() (string, error) {
		return h.Run("daemon", "restart")
	}, h.timeout(time.Minute), 200*time.Millisecond)
	if err != nil {
		h.t.Fatalf("daemon restart: %v\n%s", err, out)
	}
	return out
}

func restartWhenIdle(restart func() (string, error), timeout, poll time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		out, err := restart()
		if err == nil || !strings.Contains(out, daemonBusyRefusal) {
			return out, err
		}
		if !time.Now().Before(deadline) {
			return out, fmt.Errorf("daemon still busy %v after its runs finished: %w", timeout, err)
		}
		time.Sleep(poll)
	}
}
