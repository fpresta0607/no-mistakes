//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// deadDaemonHelperPID returns a pid that has provably exited and has not been
// reused by a live process. Windows can recycle a just-freed pid, so the check
// is retried a bounded number of times.
func deadDaemonHelperPID(t *testing.T) int {
	t.Helper()
	for attempt := 1; attempt <= 5; attempt++ {
		helper := exec.Command(os.Args[0])
		helper.Env = append(os.Environ(), "NM_DAEMON_HELPER_PROCESS=exit")
		runErr := helper.Run() // exits 23 by design
		if helper.ProcessState == nil || !helper.ProcessState.Exited() {
			t.Fatalf("dead-owner helper did not exit: state=%v err=%v", helper.ProcessState, runErr)
		}
		if code := helper.ProcessState.ExitCode(); code != 23 {
			t.Fatalf("dead-owner helper exit code = %d, want 23", code)
		}
		pid := helper.Process.Pid
		if running, err := daemonProcessRunning(pid); err == nil && !running {
			return pid
		}
	}
	t.Fatal("could not obtain a provably dead helper pid after 5 attempts")
	return 0
}

// TestEnsureDaemonStartsOnDeadEndpointOwner is the reboot regression: a stale
// endpoint file naming a dead owner pid must not keep EnsureDaemon from the
// existing start path. It drives the real detached start on an isolated root,
// never the shared daemon.
func TestEnsureDaemonStartsOnDeadEndpointOwner(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "daemon")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "15s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "25ms")

	// A dead owner pid in the endpoint file is exactly what a reboot leaves.
	deadPID := deadDaemonHelperPID(t)

	endpoint := fmt.Sprintf("127.0.0.1:1\n0123456789abcdef0123456789abcdef\n%d", deadPID)
	if err := os.WriteFile(p.Socket(), []byte(endpoint), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureDaemon(p); err != nil {
		t.Fatalf("EnsureDaemon on a dead endpoint owner: %v", err)
	}

	// Register cleanup before any later assertion can fail: a daemon that
	// started must not leak when the dial or health check below fails.
	pid, pidErr := ReadPID(p)
	t.Cleanup(func() {
		if pidErr == nil {
			shutdownIsolatedDaemon(t, p, pid)
			return
		}
		if client, err := ipc.Dial(p.Socket()); err == nil {
			_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			_ = client.Close()
		}
	})
	if pidErr != nil {
		t.Fatalf("read daemon pid: %v", pidErr)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial restored daemon: %v", err)
	}
	defer client.Close()
	var health ipc.HealthResult
	if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
		t.Fatalf("health request: %v", err)
	}
	if health.Status != "ok" {
		t.Fatalf("health status = %q, want ok", health.Status)
	}
}
