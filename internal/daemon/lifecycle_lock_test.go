package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestMaintenanceRetainsExclusionAfterLiveDaemonExit(t *testing.T) {
	p, _ := newParkedRunEnv(t)
	owner, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Release)
	server := ipc.NewServer()
	server.Handle(ipc.MethodPrepareShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.ShutdownResult{OK: true}, nil
	})
	if err := server.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- server.ServeReady() }()
	t.Cleanup(func() { server.Close() })

	release, err := PrepareShutdownIfIdle(p)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	owner.Release()
	server.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("old daemon did not exit")
	}
	if err := Start(p); err == nil || !strings.Contains(err.Error(), "startup or maintenance is in progress") {
		t.Fatalf("replacement startup escaped the old-daemon exit gap: %v", err)
	}

	startup, err := os.OpenFile(filepath.Join(p.Root(), "daemon-lifecycle.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer startup.Close()
	if err := tryLockFile(startup); err == nil {
		_ = unlockFile(startup)
		t.Fatal("old daemon exit released startup exclusion before maintenance completed")
	}
	release()
	if err := tryLockFile(startup); err != nil {
		t.Fatalf("completed maintenance retained startup exclusion: %v", err)
	}
	_ = unlockFile(startup)
}

func TestMaintenanceBlocksEveryStartupEntrypoint(t *testing.T) {
	p, database := newParkedRunEnv(t)
	t.Setenv("NM_HOME", p.Root())
	release, err := PrepareForcedShutdown(p)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for name, start := range map[string]func() error{
		"run":       Run,
		"resources": func() error { return RunWithResources(p, database) },
		"options":   func() error { return RunWithOptions(p, database, nil) },
		"start":     func() error { return Start(p) },
		"ensure":    func() error { return EnsureDaemon(p) },
		"detached":  func() error { return startDetachedDaemon(p) },
		"launch":    func() error { return launchDetachedDaemon(p, exec.Command("must-not-launch")) },
		"prepared":  func() error { return startPreparedManagedDaemon(p) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := start(); err == nil || !strings.Contains(err.Error(), "startup or maintenance is in progress") {
				t.Fatalf("startup bypassed maintenance exclusion: %v", err)
			}
		})
	}
}

func TestLifecycleLockRefusesUnreadablePathBeforeIPC(t *testing.T) {
	p, _ := newParkedRunEnv(t)
	if err := os.Mkdir(p.LifecycleLockFile(), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDial := daemonDial
	t.Cleanup(func() { daemonDial = oldDial })
	daemonDial = func(string) (*ipc.Client, error) {
		t.Fatal("unreadable lock reached admission preparation")
		return nil, nil
	}
	if release, err := PrepareShutdownIfIdle(p); err == nil || release != nil {
		t.Fatalf("unreadable lock granted teardown: %v", err)
	}
}

func TestMaintenanceBlocksEnsureBeforeArtifactProbe(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	release, err := PrepareForcedShutdown(p)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	oldHealth := daemonHealthCheck
	t.Cleanup(func() { daemonHealthCheck = oldHealth })
	isProbed := false
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		isProbed = true
		return false, nil
	}
	if err := EnsureDaemon(p); err == nil {
		t.Fatal("ensure daemon bypassed maintenance")
	}
	if isProbed {
		t.Fatal("ensure daemon reached an artifact-mutating health probe during maintenance")
	}
}

func TestLifecycleStartupParentAndChildShareExclusion(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	release, err := acquireLifecycleLock(p, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	child := startLifecycleLockProcess(t, p, "startup")
	if releaseMaintenance, err := PrepareForcedShutdown(p); err == nil {
		releaseMaintenance()
		t.Fatal("maintenance bypassed startup parent and child")
	}
	release()
	if releaseMaintenance, err := PrepareForcedShutdown(p); err == nil {
		releaseMaintenance()
		t.Fatal("maintenance bypassed the remaining child")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	releaseMaintenance, err := PrepareForcedShutdown(p)
	if err != nil {
		t.Fatalf("child death retained startup exclusion: %v", err)
	}
	releaseMaintenance()
}

func TestLifecycleLockCrashReleasesMaintenance(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	child := startLifecycleLockProcess(t, p, "maintenance")
	if release, err := acquireLifecycleLock(p, false); err == nil {
		release()
		t.Fatal("startup bypassed maintenance process")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	release, err := acquireLifecycleLock(p, false)
	if err != nil {
		t.Fatalf("maintenance death retained startup exclusion: %v", err)
	}
	release()
}

func startLifecycleLockProcess(t *testing.T, p *paths.Paths, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleLockProcess$")
	cmd.Env = append(os.Environ(), "NM_LOCK_TEST_ROOT="+p.Root(), "NM_LOCK_TEST_MODE="+mode)
	setSysProcAttr(cmd)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &strings.Builder{}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "locked" {
		t.Fatalf("lock child did not become ready: %q, %v", ready, err)
	}
	return cmd
}

func TestLifecycleLockProcess(t *testing.T) {
	root := os.Getenv("NM_LOCK_TEST_ROOT")
	if root == "" {
		return
	}
	release, err := acquireLifecycleLock(paths.WithRoot(root), os.Getenv("NM_LOCK_TEST_MODE") == "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Println("locked")
	var input [1]byte
	_, _ = os.Stdin.Read(input[:])
}

func TestIdleMaintenanceStopsLiveDaemonAndReleasesStartup(t *testing.T) {
	p, _ := startTestDaemon(t)
	if err := Stop(p); err != nil {
		t.Fatal(err)
	}
	if alive, err := IsRunning(p); err != nil || alive {
		t.Fatalf("idle daemon did not stop: alive=%t, %v", alive, err)
	}
	release, err := acquireLifecycleLock(p, false)
	if err != nil {
		t.Fatalf("successful stop retained startup exclusion: %v", err)
	}
	release()
}

func TestBusyMaintenanceRefusalReleasesExclusionAndKeepsAdmission(t *testing.T) {
	p, database := startTestDaemon(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "busy", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if release, err := PrepareShutdownIfIdle(p); err == nil || !strings.Contains(err.Error(), run.ID) {
		if release != nil {
			release()
		}
		t.Fatalf("maintenance did not preserve busy refusal: %v", err)
	}
	release, err := acquireLifecycleLock(p, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(ipc.MethodAdmitPush, map[string]string{}, nil)
	if err == nil || strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("refused maintenance closed daemon admission: %v", err)
	}
}

func TestPreparedManagedStartupDoesNotKillAReplacement(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	defer stubServiceRuntime(t)()
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			runtimeGOOS = platform
			serviceUserHomeDir = func() (string, error) { return t.TempDir(), nil }
			serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
			serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
				command := name + " " + strings.Join(args, " ")
				if strings.Contains(command, "bootout") || strings.Contains(command, "-k") || strings.Contains(command, "restart") {
					t.Fatalf("prepared startup can kill another replacement: %s", command)
				}
				return nil, nil
			}
			daemonHealthCheck = func(*paths.Paths) (bool, error) { return true, nil }
			if err := startPreparedManagedDaemon(p); err != nil {
				t.Fatal(err)
			}
		})
	}
}
