package ipc_test

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestServerAuthenticatesLocalPeerPID(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("peer PID transport not implemented on this platform")
	}
	sock := socketPath(t)
	srv := ipc.NewServer()
	srv.Handle("peer", func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		return map[string]int{"pid": ipc.PeerPID(ctx)}, nil
	})
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
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	var result struct {
		PID int `json:"pid"`
	}
	if err := client.Call("peer", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.PID != os.Getpid() {
		t.Fatalf("peer pid = %d, want authenticated client pid %d", result.PID, os.Getpid())
	}
}
