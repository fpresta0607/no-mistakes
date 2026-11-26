package ipc_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// socketPath returns a short socket path to stay within macOS 104-byte limit.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func startServer(t *testing.T, sock string) *ipc.Server {
	t.Helper()
	srv := ipc.NewServer()
	if err := srv.Listen(sock); err != nil {
		t.Fatalf("listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeReady() }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	return srv
}
