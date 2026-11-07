package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestUpdaterRetainsStartupExclusionThroughReset(t *testing.T) {
	for _, mode := range []string{"absent", "live", "force"} {
		t.Run(mode, func(t *testing.T) {
			u, executable, _, _ := newActiveRunUpdater(t)
			finishActiveRuns(t, u.paths)
			stubDaemonAlive(t, false)
			if mode != "absent" {
				server := ipc.NewServer()
				server.Handle(ipc.MethodPrepareShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
					if mode == "force" {
						t.Error("force performed an idle check")
					}
					return &ipc.ShutdownResult{OK: true}, nil
				})
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
			}
			u.force = mode == "force"
			oldStop, oldStart := daemonStop, daemonStart
			t.Cleanup(func() { daemonStop, daemonStart = oldStop, oldStart })
			isStopped, isStarted := false, false
			daemonStop = func(p *paths.Paths) error {
				release, err := daemon.PrepareForcedShutdown(p)
				if release != nil {
					release()
				}
				if err == nil || !strings.Contains(err.Error(), "startup or maintenance is in progress") {
					t.Errorf("update lost startup exclusion before stop: %v", err)
				}
				content, err := os.ReadFile(executable)
				if err != nil || string(content) == "old-binary" {
					t.Errorf("reset preceded binary replacement: %q, %v", content, err)
				}
				isStopped = true
				return nil
			}
			daemonStart = func(p *paths.Paths) error {
				release, err := daemon.PrepareForcedShutdown(p)
				if err != nil {
					return err
				}
				release()
				isStarted = true
				return nil
			}
			u.resetDaemon = func(_ bool, releaseShutdown func()) error {
				return defaultResetDaemon(u.paths, true, "", releaseShutdown)
			}
			if err := u.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !isStopped || !isStarted {
				t.Fatalf("reset stopped=%t, started=%t", isStopped, isStarted)
			}
		})
	}
}

func TestUpdaterAtomicShutdownRefusalKeepsBinaryAndDaemon(t *testing.T) {
	for _, isUnsupported := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsupported_%t", isUnsupported), func(t *testing.T) {
			u, executable, _, _ := newActiveRunUpdater(t)
			finishActiveRuns(t, u.paths)
			stubDaemonAlive(t, false)
			u.resetDaemon = func(bool, func()) error { t.Error("refusal reached daemon reset"); return nil }
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
