package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonLifecycleRetainsStartupExclusion(t *testing.T) {
	for _, mode := range []string{"absent", "live", "force"} {
		t.Run(mode, func(t *testing.T) {
			for _, action := range []string{"stop", "restart", "start", "uninstall"} {
				t.Run(action, func(t *testing.T) {
					t.Setenv("NM_HOME", makeSocketSafeTempDir(t))
					if mode != "absent" {
						legacyLifecycleServer(t, 0, func(server *ipc.Server) {
							server.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
								return &ipc.GateContextResult{}, nil
							})
							server.Handle(ipc.MethodPrepareShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
								if mode == "force" {
									t.Error("force performed an idle check")
								}
								return &ipc.ShutdownResult{OK: true}, nil
							})
						})
					}
					oldStop, oldStart, oldGuarded := daemonStopFn, daemonStartFn, daemonStartGuardedFn
					oldUninstall, oldSupported := daemonUninstallFn, daemonUninstallSupportedFn
					t.Cleanup(func() {
						daemonStopFn, daemonStartFn, daemonStartGuardedFn = oldStop, oldStart, oldGuarded
						daemonUninstallFn, daemonUninstallSupportedFn = oldUninstall, oldSupported
					})
					isStopped := false
					daemonStopFn = func(p *paths.Paths) error {
						release, err := daemon.PrepareForcedShutdown(p)
						if release != nil {
							release()
						}
						if err == nil || !strings.Contains(err.Error(), "startup or maintenance is in progress") {
							t.Errorf("%s lost startup exclusion before stopping: %v", action, err)
						}
						isStopped = true
						return nil
					}
					daemonStartFn = func(p *paths.Paths) error {
						release, err := daemon.PrepareForcedShutdown(p)
						if err != nil {
							return err
						}
						release()
						return nil
					}
					daemonStartGuardedFn = func(p *paths.Paths, guard func() (func(), error)) error {
						release, err := guard()
						if err != nil {
							return err
						}
						err = daemonStopFn(p)
						release()
						if err != nil {
							return err
						}
						return daemonStartFn(p)
					}
					daemonUninstallSupportedFn = func() bool { return true }
					daemonUninstallFn = func(p *paths.Paths, guard func() (func(), error)) (string, error) {
						release, err := guard()
						if err != nil {
							return "", err
						}
						defer release()
						return "service", daemonStopFn(p)
					}
					arguments := []string{"daemon", action}
					if mode == "force" {
						arguments = append(arguments, "--force")
					}
					out, err := executeCmd(arguments...)
					if err != nil || !isStopped {
						t.Fatalf("%s stopped=%t: %v, %s", action, isStopped, err, out)
					}
				})
			}
		})
	}
}

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
			daemonStartGuardedFn = func(_ *paths.Paths, guard func() (func(), error)) error {
				release, err := guard()
				if err == nil {
					release()
				}
				return err
			}
			daemonUninstallSupportedFn = func() bool { return true }
			daemonUninstallFn = func(_ *paths.Paths, guard func() (func(), error)) (string, error) {
				release, err := guard()
				if err == nil {
					release()
				}
				return "", err
			}
			daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
			out, err := executeCmd("daemon", action)
			if err == nil || !strings.Contains(err.Error(), "late-admitted-run") {
				t.Fatalf("%s lost busy refusal: %v, %s", action, err, out)
			}
		})
	}
}
