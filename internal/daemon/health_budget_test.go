package daemon

import (
	"context"
	"encoding/json"
	"os"
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
