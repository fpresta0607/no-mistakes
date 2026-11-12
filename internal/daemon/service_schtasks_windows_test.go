package daemon

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"golang.org/x/sys/windows"
)

// TestInstallWindowsTaskRegistersUserScopedLogonTask registers the real task
// with Task Scheduler (issue #1192). From a non-elevated token the old unscoped
// /SC ONLOGON registration failed with Access is denied; this asserts the
// registered task's logon trigger is scoped to the current user and runs on a
// limited interactive token. The task is disposable (unique per-root name,
// deleted on cleanup) and its action is a nonexistent executable, so a task
// leaked by a killed test process fails silently at logon instead of running
// anything. Task Scheduler does not validate the action path at registration.
func TestInstallWindowsTaskRegistersUserScopedLogonTask(t *testing.T) {
	if testing.Short() {
		t.Skip("registers a real scheduled task")
	}
	if _, err := exec.LookPath("schtasks"); err != nil {
		t.Skipf("schtasks unavailable: %v", err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}

	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	taskName := windowsTaskName(p)
	t.Cleanup(func() { _, _ = runServiceCommand("schtasks", "/Delete", "/TN", taskName, "/F") })

	exe := filepath.Join(t.TempDir(), "missing", "no-mistakes.exe")
	if err := installWindowsTask(p, exe); err != nil {
		t.Fatalf("installWindowsTask: %v", err)
	}

	exported, err := runServiceCommand("schtasks", "/Query", "/TN", taskName, "/XML")
	if err != nil {
		t.Fatalf("query registered task: %v", err)
	}
	task := parseWindowsTaskDefinition(t, string(exported))
	if len(task.LogonTriggers) != 1 {
		t.Fatalf("logon triggers = %d, want 1\n%s", len(task.LogonTriggers), exported)
	}
	if got := task.LogonTriggers[0].UserID; !strings.EqualFold(got, current.Username) && !strings.EqualFold(got, current.Uid) {
		t.Fatalf("logon trigger user = %q, want current user %q (%s)", got, current.Username, current.Uid)
	}
	if task.Principal.LogonType != "InteractiveToken" {
		t.Fatalf("principal logon type = %q, want InteractiveToken", task.Principal.LogonType)
	}
	if task.Principal.RunLevel != "" && task.Principal.RunLevel != "LeastPrivilege" {
		t.Fatalf("principal run level = %q, want limited", task.Principal.RunLevel)
	}
	if task.Priority != "4" {
		t.Fatalf("registered task priority = %q, want 4 (issue #253)\n%s", task.Priority, exported)
	}
	if !serviceDefinitionMatchesRoot(exported, p) {
		t.Fatalf("registered task does not match its root\n%s", exported)
	}
}

// Issue #253: what the daemon starts inherits the task's priority, so at Task
// Scheduler's default of 7 every git, agent and test command ran below normal
// with low disk and memory page priority. This registers the real definition
// with a GUI-subsystem probe as its action (no console window), runs it, and
// reads the priorities of a child the probe starts the way the daemon starts
// its commands. The task is disposable, ended and deleted on cleanup.
func TestManagedDaemonTaskStartsChildrenAtNormalPriority(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a probe and runs a real scheduled task")
	}
	if _, err := exec.LookPath("schtasks"); err != nil {
		t.Skipf("schtasks unavailable: %v", err)
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		t.Fatalf("session of this process: %v", err)
	}
	if session == 0 {
		t.Skip("an InteractiveToken task runs only in an interactive logon session; this process is in session 0")
	}

	probe := filepath.Join(t.TempDir(), "priorityprobe.exe")
	build, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(build, "go", "build", "-ldflags=-H=windowsgui", "-o", probe, "./testdata/priorityprobe").CombinedOutput(); err != nil {
		t.Fatalf("build priority probe: %v\n%s", err, out)
	}

	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := os.MkdirAll(p.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	taskName := windowsTaskName(p)
	t.Cleanup(func() {
		_, _ = runServiceCommand("schtasks", "/End", "/TN", taskName)
		_, _ = runServiceCommand("schtasks", "/Delete", "/TN", taskName, "/F")
	})
	if err := installWindowsTask(p, probe); err != nil {
		t.Fatalf("installWindowsTask: %v", err)
	}
	if err := startWindowsTask(p); err != nil {
		t.Fatalf("startWindowsTask: %v", err)
	}

	// The child writes its report before it exits, and the probe exits after
	// its child, so a report plus a task no longer running means both are gone
	// and the probe binary can be removed.
	report := filepath.Join(p.Root(), "child-priority.txt")
	deadline := time.Now().Add(time.Minute)
	for {
		data, readErr := os.ReadFile(report)
		observation, inspectErr := inspectWindowsManagedDaemon(p)
		if readErr == nil && len(data) > 0 && inspectErr == nil && observation.state != 4 {
			if got, want := string(data), "class=0x20 io=2 page=5"; got != want {
				t.Fatalf("child of the managed task reports %q, want %q (normal processor class, disk priority 2, page priority 5)", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			status, _ := runServiceCommand("schtasks", "/Query", "/TN", taskName, "/V", "/FO", "LIST")
			t.Fatalf("no finished priority report from the task's child within a minute (report %q, read error %v, inspect error %v)\n%s", data, readErr, inspectErr, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
