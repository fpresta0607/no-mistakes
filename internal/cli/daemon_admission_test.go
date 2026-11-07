package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonLifecyclePreservesAtomicBusyRefusal(t *testing.T) {
	for _, action := range []string{"stop", "restart", "start", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			legacyLifecycleServer(t, 0, func(server *ipc.Server) {
				server.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.GateContextResult{}, nil
				})
				server.Handle(ipc.MethodPrepareShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
					return nil, fmt.Errorf("refusing shutdown: late-admitted-run is active")
				})
			})
			oldStop, oldStart, oldGuarded := daemonStopFn, daemonStartFn, daemonStartGuardedFn
			oldUninstall, oldSupported, oldRunning := daemonUninstallFn, daemonUninstallSupportedFn, daemonIsRunningFn
			t.Cleanup(func() {
				daemonStopFn, daemonStartFn, daemonStartGuardedFn = oldStop, oldStart, oldGuarded
				daemonUninstallFn, daemonUninstallSupportedFn, daemonIsRunningFn = oldUninstall, oldSupported, oldRunning
			})
			daemonStopFn = func(*paths.Paths) error { t.Fatal("refusal reached stop or PID fallback"); return nil }
			daemonStartFn = func(*paths.Paths) error { t.Fatal("refusal reached restart"); return nil }
			daemonStartGuardedFn = func(_ *paths.Paths, guard func() error) error { return guard() }
			daemonUninstallSupportedFn = func() bool { return true }
			daemonUninstallFn = func(_ *paths.Paths, guard func() error) (string, error) { return "", guard() }
			daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
			out, err := executeCmd("daemon", action)
			if err == nil || !strings.Contains(err.Error(), "late-admitted-run") {
				t.Fatalf("%s lost busy refusal: %v, %s", action, err, out)
			}
		})
	}
}
