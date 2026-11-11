package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// A slow get_run falls back to a health probe; a busy daemon that answers
// health in more than 250 ms is alive, so the drive keeps going instead of
// failing on a fixed 250 ms read deadline (#167).
func TestRunStateHealthProbeWaitsForASlowLiveDaemon(t *testing.T) {
	// Arrange
	t.Setenv("NM_DAEMON_CONNECT_TIMEOUT", "")
	socketPath := filepath.Join(makeSocketSafeTempDir(t), "reconcile.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		time.Sleep(600 * time.Millisecond)
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	startIPCServer(t, srv, socketPath)

	// Act
	err := (&ipcRunStateSource{socketPath: socketPath}).probeHealth(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("health probe of a slow live daemon = %v, want nil", err)
	}
}
