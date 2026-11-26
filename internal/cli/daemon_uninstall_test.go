package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonUninstallReportsRemovedDefinitionAndNoService(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	prev := daemonUninstallFn
	t.Cleanup(func() { daemonUninstallFn = prev })
	for _, definition := range []string{"/tmp/home/Library/LaunchAgents/daemon.plist", ""} {
		t.Run(definition, func(t *testing.T) {
			called := false
			daemonUninstallFn = func(_ *paths.Paths, _ func() (func(), error)) (string, error) {
				called = true
				return definition, nil
			}
			out, err := executeCmd("daemon", "uninstall")
			if err != nil || !called {
				t.Fatalf("uninstall called=%t error=%v output=%q", called, err, out)
			}
			if definition != "" {
				if !strings.Contains(out, "removed managed daemon service: "+definition) {
					t.Fatalf("uninstall must name removed definition: %q", out)
				}
			} else {
				p, err := paths.New()
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out, "No managed daemon service is installed for NM_HOME "+p.Root()) {
					t.Fatalf("uninstall must explain the no-op: %q", out)
				}
			}
		})
	}
}

func TestDaemonUninstallRefusesActiveRunsUnlessForced(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))
	prev := daemonUninstallFn
	t.Cleanup(func() { daemonUninstallFn = prev })
	called := false
	daemonUninstallFn = func(_ *paths.Paths, guard func() (func(), error)) (string, error) {
		release, err := guard()
		if err != nil {
			return "", err
		}
		defer release()
		called = true
		return "/tmp/service.plist", nil
	}
	out, err := executeCmd("daemon", "uninstall")
	if err == nil || called || !strings.Contains(out+err.Error(), "refusing daemon uninstall") {
		t.Fatalf("uninstall must refuse active runs: called=%t error=%v output=%q", called, err, out)
	}
	out, err = executeCmd("daemon", "uninstall", "--force")
	if err != nil || !called || !strings.Contains(out, "FORCE: daemon uninstall") {
		t.Fatalf("forced uninstall: called=%t error=%v output=%q", called, err, out)
	}
}

func TestDaemonStopNamesRetainedServiceAndUninstallCommand(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	prevStop, prevDefinition := daemonStopFn, daemonServiceDefinitionFn
	t.Cleanup(func() { daemonStopFn, daemonServiceDefinitionFn = prevStop, prevDefinition })
	daemonStopFn = func(*paths.Paths) error { return nil }
	for _, definition := range []string{
		"/tmp/home/Library/LaunchAgents/com.kunchenguid.no-mistakes.daemon.test.plist",
		"/tmp/home/.config/systemd/user/no-mistakes-daemon-test.service",
		"no-mistakes-daemon-test",
	} {
		t.Run(definition, func(t *testing.T) {
			daemonServiceDefinitionFn = func(*paths.Paths) string { return definition }
			out, err := executeCmd("daemon", "stop")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"daemon stopped", "starts again at the next login", definition, "no-mistakes daemon uninstall"} {
				if !strings.Contains(out, want) {
					t.Fatalf("stop must contain %q: %q", want, out)
				}
			}
		})
	}
	daemonServiceDefinitionFn = func(*paths.Paths) string { return "" }
	out, err := executeCmd("daemon", "stop")
	if err != nil || strings.Contains(out, "next login") {
		t.Fatalf("no retained service should mean no login notice: %q, %v", out, err)
	}
}
