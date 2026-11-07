package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestUpdaterAtomicShutdownRefusalKeepsBinaryAndDaemon(t *testing.T) {
	for _, isUnsupported := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsupported_%t", isUnsupported), func(t *testing.T) {
			u, executable, _, _ := newActiveRunUpdater(t)
			finishActiveRuns(t, u.paths)
			stubDaemonAlive(t, false)
			u.resetDaemon = func(bool) error { t.Error("refusal reached daemon reset"); return nil }
			server := ipc.NewServer()
			if !isUnsupported {
				server.Handle(ipc.MethodPrepareShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
					return nil, fmt.Errorf("refusing shutdown: a run arrived during download")
				})
			}
			if err := server.Listen(u.paths.Socket()); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- server.ServeReady() }()
			t.Cleanup(func() {
				server.Close()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("fake daemon did not stop")
				}
			})
			if err := u.run(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot atomically prepare daemon shutdown") {
				t.Fatalf("update lost idle guard refusal: %v", err)
			}
			content, err := os.ReadFile(executable)
			if err != nil || string(content) != "old-binary" {
				t.Fatalf("refused update replaced binary: %q, %v", content, err)
			}
		})
	}
}
