package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestUninstallWithoutManagedServiceIsANoOp(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			defer stubServiceRuntime(t)()
			runtimeGOOS = platform
			home := t.TempDir()
			serviceUserHomeDir = func() (string, error) { return home, nil }
			serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
				if name == "schtasks" && args[0] == "/Query" {
					return nil, fmt.Errorf("ERROR: The system cannot find the file specified")
				}
				t.Fatalf("unexpected service command: %s %v", name, args)
				return nil, nil
			}
			p := paths.WithRoot(filepath.Join(t.TempDir(), "absent"))
			if got, err := Uninstall(p); err != nil || got != "" {
				t.Fatalf("Uninstall = %q, %v; want no-op", got, err)
			}
			if _, err := os.Stat(p.Root()); !os.IsNotExist(err) {
				t.Fatalf("uninstall should not create application state: %v", err)
			}
		})
	}
}

// fakeServiceManager stands in for the platform service manager. Files model
// the LaunchAgent and systemd unit; tasks model registered scheduled tasks.
type fakeServiceManager struct {
	tasks    map[string]bool
	failOn   string
	commands []string
	onStop   func()
}

func (m *fakeServiceManager) run(name string, args ...string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")
	m.commands = append(m.commands, command)
	if m.failOn != "" && strings.HasPrefix(command, m.failOn) {
		return nil, fmt.Errorf("permission denied")
	}
	if name == "schtasks" && len(args) >= 3 && args[1] == "/TN" {
		if !m.tasks[args[2]] {
			return nil, fmt.Errorf("ERROR: The system cannot find the file specified")
		}
		if args[0] == "/Delete" {
			delete(m.tasks, args[2])
		}
	}
	if strings.HasPrefix(command, "systemctl --user stop ") || strings.HasPrefix(command, "schtasks /End ") || strings.HasPrefix(command, "launchctl bootout ") {
		if m.onStop != nil {
			m.onStop()
		}
	}
	return nil, nil
}

type uninstallPlatform struct {
	goos      string
	install   func(t *testing.T, m *fakeServiceManager, p *paths.Paths)
	installed func(m *fakeServiceManager, p *paths.Paths) bool
	stop      func(p *paths.Paths) string
	want      func(p *paths.Paths) (string, []string)
}

func writeServiceFileForTest(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("service"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func serviceFileExists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}

func uninstallPlatforms() []uninstallPlatform {
	return []uninstallPlatform{
		{
			goos: "darwin",
			install: func(t *testing.T, _ *fakeServiceManager, p *paths.Paths) {
				writeServiceFileForTest(t, launchAgentPath(p))
			},
			installed: func(_ *fakeServiceManager, p *paths.Paths) bool { return serviceFileExists(launchAgentPath(p)) },
			stop:      func(p *paths.Paths) string { return "launchctl bootout gui/501/" + launchdServiceLabel(p) },
			want: func(p *paths.Paths) (string, []string) {
				return launchAgentPath(p), []string{"launchctl bootout gui/501/" + launchdServiceLabel(p)}
			},
		},
		{
			goos: "linux",
			install: func(t *testing.T, _ *fakeServiceManager, p *paths.Paths) {
				writeServiceFileForTest(t, systemdUserServicePath(p))
			},
			installed: func(_ *fakeServiceManager, p *paths.Paths) bool { return serviceFileExists(systemdUserServicePath(p)) },
			stop:      func(p *paths.Paths) string { return "systemctl --user stop " + systemdServiceName(p) },
			want: func(p *paths.Paths) (string, []string) {
				return systemdUserServicePath(p), []string{
					"systemctl --user stop " + systemdServiceName(p),
					"systemctl --user disable " + systemdServiceName(p),
					"systemctl --user daemon-reload",
				}
			},
		},
		{
			goos:      "windows",
			install:   func(_ *testing.T, m *fakeServiceManager, p *paths.Paths) { m.tasks[windowsTaskName(p)] = true },
			installed: func(m *fakeServiceManager, p *paths.Paths) bool { return m.tasks[windowsTaskName(p)] },
			stop:      func(p *paths.Paths) string { return "schtasks /End /TN " + windowsTaskName(p) },
			want: func(p *paths.Paths) (string, []string) {
				name := windowsTaskName(p)
				return name, []string{
					"schtasks /Query /TN " + name,
					"schtasks /Query /TN " + name,
					"schtasks /End /TN " + name,
					"schtasks /Delete /TN " + name + " /F",
				}
			},
		},
	}
}

func stubUninstallPlatform(t *testing.T, goos string) *fakeServiceManager {
	t.Helper()
	t.Cleanup(stubServiceRuntime(t))
	runtimeGOOS = goos
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, nil }
	m := &fakeServiceManager{tasks: map[string]bool{}}
	serviceCommandRunner = m.run
	return m
}

func TestUninstallStopsThenRemovesOnlyThisInstancesServiceOnEveryPlatform(t *testing.T) {
	for _, platform := range uninstallPlatforms() {
		t.Run(platform.goos, func(t *testing.T) {
			m := stubUninstallPlatform(t, platform.goos)
			p := paths.WithRoot(t.TempDir())
			other := paths.WithRoot(t.TempDir())
			if err := os.WriteFile(filepath.Join(p.Root(), "keep-data"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			platform.install(t, m, p)
			platform.install(t, m, other)
			isStopped := false
			m.onStop = func() {
				isStopped = true
				lock, err := acquireSingletonLock(p)
				if lock != nil {
					lock.Release()
				}
				if !errors.Is(err, ErrSingletonLockHeld) {
					t.Fatalf("uninstall teardown lost absent-daemon ownership: %v", err)
				}
				if !platform.installed(m, p) {
					t.Fatal("service definition must remain until stopped")
				}
			}
			wantDefinition, wantCommands := platform.want(p)

			got, err := Uninstall(p)

			if err != nil || got != wantDefinition {
				t.Fatalf("Uninstall = %q, %v; want %q", got, err, wantDefinition)
			}
			if !isStopped {
				t.Fatal("uninstall removed the service without stopping it")
			}
			if !reflect.DeepEqual(m.commands, wantCommands) {
				t.Fatalf("commands = %v, want %v", m.commands, wantCommands)
			}
			if platform.installed(m, p) {
				t.Fatal("service definition remains")
			}
			if !platform.installed(m, other) {
				t.Fatal("another instance's service changed")
			}
			if data, err := os.ReadFile(filepath.Join(p.Root(), "keep-data")); err != nil || string(data) != "data" {
				t.Fatalf("application data changed: %q, %v", data, err)
			}
			m.commands = nil
			if got, err := Uninstall(p); err != nil || got != "" {
				t.Fatalf("second Uninstall = %q, %v; want no-op", got, err)
			}
		})
	}
}

func TestUninstallKeepsDefinitionWhenStopFails(t *testing.T) {
	for _, platform := range uninstallPlatforms() {
		t.Run(platform.goos, func(t *testing.T) {
			m := stubUninstallPlatform(t, platform.goos)
			p := paths.WithRoot(t.TempDir())
			platform.install(t, m, p)
			m.failOn = platform.stop(p)

			got, err := Uninstall(p)

			if err == nil || got != "" || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("Uninstall = %q, %v; want stop failure", got, err)
			}
			if !platform.installed(m, p) {
				t.Fatal("failed stop must keep service definition")
			}
		})
	}
}

func TestInstalledServiceDefinitionNamesOnlyAnInstalledService(t *testing.T) {
	for _, platform := range uninstallPlatforms() {
		t.Run(platform.goos, func(t *testing.T) {
			m := stubUninstallPlatform(t, platform.goos)
			p := paths.WithRoot(t.TempDir())
			if got := InstalledServiceDefinition(p); got != "" {
				t.Fatalf("absent service definition = %q", got)
			}
			platform.install(t, m, p)
			want, _ := platform.want(p)
			if got := InstalledServiceDefinition(p); got != want {
				t.Fatalf("installed service definition = %q, want %q", got, want)
			}
		})
	}
	t.Run("freebsd", func(t *testing.T) {
		stubUninstallPlatform(t, "freebsd")
		if got := InstalledServiceDefinition(paths.WithRoot(t.TempDir())); got != "" {
			t.Fatalf("platform without a service manager named %q", got)
		}
	})
}

func TestUninstallAsksADaemonOutsideTheServiceToShutDown(t *testing.T) {
	p, _ := startTestDaemon(t)
	defer stubServiceRuntime(t)()
	runtimeGOOS = "darwin"
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
	definition := launchAgentPath(p)
	if err := os.MkdirAll(filepath.Dir(definition), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, []byte("service"), 0o644); err != nil {
		t.Fatal(err)
	}
	serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }
	daemonKillPID = func(pid int) error {
		t.Fatalf("daemon was killed by pid %d instead of asked to shut down", pid)
		return nil
	}
	t.Cleanup(func() { daemonKillPID = killPID })

	if got, err := Uninstall(p); err != nil || got != definition {
		t.Fatalf("Uninstall = %q, %v; want %q", got, err, definition)
	}
	if alive, _ := IsRunning(p); alive {
		t.Fatal("daemon still running after uninstall")
	}
}
