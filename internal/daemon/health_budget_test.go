package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// A busy daemon on a loaded host can take well over 250 ms to answer health
// (#167), and that daemon is alive: the check waits for the configured
// daemon_connect_timeout, and only a daemon that stays silent past that budget
// fails as unresponsive.
func TestDaemonHealthCheckWaitsForTheConnectBudget(t *testing.T) {
	cases := []struct {
		name           string
		connectTimeout string
		replyDelay     time.Duration
		isAlive        bool
		isTimeout      bool
	}{
		{name: "slow live daemon within the budget", replyDelay: 600 * time.Millisecond, isAlive: true},
		{name: "wedged daemon past the budget", connectTimeout: "300ms", replyDelay: time.Minute, isTimeout: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			t.Setenv("NM_DAEMON_CONNECT_TIMEOUT", tc.connectTimeout)
			root, err := os.MkdirTemp("", "dhealth")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			server := ipc.NewServer()
			release := make(chan struct{})
			server.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				select {
				case <-time.After(tc.replyDelay):
				case <-release:
				}
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			if err := server.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			go func() { _ = server.ServeReady() }()
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })

			// Act
			isAlive, err := daemonIsRunningViaIPC(p)

			// Assert
			if tc.isTimeout {
				if isAlive || !ipc.IsCallTimeout(err) {
					t.Fatalf("health check = (%t, %v), want a call timeout for a daemon silent past its budget", isAlive, err)
				}
				return
			}
			if err != nil || isAlive != tc.isAlive {
				t.Fatalf("health check = (%t, %v), want (%t, nil)", isAlive, err, tc.isAlive)
			}
		})
	}
}

// A live daemon that misses even the daemon_connect_timeout health budget is
// busy, not stopped: EnsureDaemon reports it as slow, never starts a second
// daemon, and never suggests `daemon start`, whose restart would end every
// run the busy daemon serves (#245). Without a live, matching PID record the
// probe miss keeps its recovery hint.
func TestEnsureDaemonReportsALiveDaemonMissingItsHealthBudgetAsSlow(t *testing.T) {
	cases := []struct {
		name          string
		pidStartDrift time.Duration
		hasPIDFile    bool
		isSlow        bool
	}{
		{name: "live daemon pid", hasPIDFile: true, isSlow: true},
		{name: "recycled daemon pid", hasPIDFile: true, pidStartDrift: time.Hour},
		{name: "no pid file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			t.Setenv("NM_DAEMON_CONNECT_TIMEOUT", "300ms")
			root, err := os.MkdirTemp("", "dslow")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if tc.hasPIDFile {
				startedAt, err := processStartTime(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				record := daemonPIDFile{PID: os.Getpid(), StartedAt: startedAt.Add(tc.pidStartDrift).UTC()}
				if err := writeDaemonPIDFile(p.PIDFile(), record); err != nil {
					t.Fatal(err)
				}
			}
			server := ipc.NewServer()
			release := make(chan struct{})
			server.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
				<-release
				return &ipc.HealthResult{Status: "ok"}, nil
			})
			if err := server.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			go func() { _ = server.ServeReady() }()
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			originalStart := daemonStart
			isStarted := false
			daemonStart = func(*paths.Paths) error {
				isStarted = true
				return nil
			}
			t.Cleanup(func() { daemonStart = originalStart })

			// Act
			ensureErr := EnsureDaemon(p)
			_, runningErr := IsRunning(p)

			// Assert
			if ensureErr == nil || isStarted {
				t.Fatalf("EnsureDaemon = %v (started %t), want a failed probe that starts nothing", ensureErr, isStarted)
			}
			if !ipc.IsCallTimeout(ensureErr) {
				t.Fatalf("EnsureDaemon error = %v, want the health call timeout kept", ensureErr)
			}
			if IsSlow(ensureErr) != tc.isSlow || IsSlow(runningErr) != tc.isSlow {
				t.Fatalf("IsSlow(EnsureDaemon) = %t, IsSlow(IsRunning) = %t, want %t: %v", IsSlow(ensureErr), IsSlow(runningErr), tc.isSlow, ensureErr)
			}
			hasRestartHint := strings.Contains(ensureErr.Error(), "no-mistakes daemon start")
			if tc.isSlow {
				if hasRestartHint || !strings.Contains(ensureErr.Error(), "running but slow") {
					t.Fatalf("EnsureDaemon error = %q, want a slow report with no restart hint", ensureErr)
				}
				return
			}
			if !hasRestartHint {
				t.Fatalf("EnsureDaemon error = %q, want the recovery hint for an unproven daemon", ensureErr)
			}
		})
	}
}
