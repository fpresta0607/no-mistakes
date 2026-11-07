//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestAbsentStartupRechecksEndpointOwnerWithoutPIDFile(t *testing.T) {
	for _, scenario := range []string{"dead", "live", "changed_to_live", "malformed", "unreadable", "unsupported", "non_positive_pid", "out_of_range_pid"} {
		t.Run(scenario, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			endpoint := fmt.Sprintf("127.0.0.1:1\ntoken\n%d", deadDaemonHelperPID(t))
			switch scenario {
			case "live":
				endpoint = fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
			case "malformed":
				endpoint = "unreadable owner"
			case "non_positive_pid":
				endpoint = "127.0.0.1:1\ntoken\n-1"
			case "out_of_range_pid":
				endpoint = "127.0.0.1:1\ntoken\n4294967296"
			}
			if scenario == "unreadable" {
				if err := os.Mkdir(p.Socket(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(p.Socket(), []byte(endpoint), 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario == "changed_to_live" {
				if alive, err := daemonIsRunningViaIPC(p); err != nil || alive {
					t.Fatalf("initial dead-owner probe: alive=%t, err=%v", alive, err)
				}
			}
			oldDial, oldList := daemonDial, daemonListDaemonProcesses
			t.Cleanup(func() { daemonDial, daemonListDaemonProcesses = oldDial, oldList })
			daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) {
				if scenario == "changed_to_live" {
					endpoint = fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
					if err := os.WriteFile(p.Socket(), []byte(endpoint), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return nil, nil
			}
			isRechecked := false
			daemonDial = func(endpoint string) (*ipc.Client, error) {
				isRechecked = true
				if release, err := acquireLifecycleLock(p, false); err == nil {
					release()
					t.Error("owner recheck lost maintenance exclusion")
				}
				if lock, err := acquireSingletonLock(p); !errors.Is(err, ErrSingletonLockHeld) {
					if lock != nil {
						lock.Release()
					}
					t.Errorf("owner recheck lost singleton: %v", err)
				}
				if scenario == "unsupported" {
					return nil, errors.New("owner inspection unsupported")
				}
				return ipc.Dial(endpoint)
			}
			release, err := prepareAbsentDaemonStartup(p)
			if release != nil {
				defer release()
			}
			if !isRechecked {
				t.Error("missing PID file bypassed the locked endpoint-owner recheck")
			}
			if scenario == "dead" {
				if err != nil || release == nil {
					t.Fatalf("proven dead endpoint refused: %v", err)
				}
				if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
					t.Fatalf("dead endpoint survived preparation: %v", err)
				}
				return
			}
			if err == nil || release != nil {
				t.Fatalf("unconfirmed endpoint granted startup: %v", err)
			}
			if scenario != "unreadable" {
				data, err := os.ReadFile(p.Socket())
				if err != nil || string(data) != endpoint {
					t.Fatalf("refusal changed endpoint: %q, %v", data, err)
				}
			}
			if release, err := acquireLifecycleLock(p, false); err != nil {
				t.Fatalf("refusal retained maintenance exclusion: %v", err)
			} else {
				release()
			}
			if scenario == "unsupported" && !strings.Contains(err.Error(), "owner inspection unsupported") {
				t.Fatalf("unsupported inspection was hidden: %v", err)
			}
		})
	}
}

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
			_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{Force: true}, nil)
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
