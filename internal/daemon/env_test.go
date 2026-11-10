package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func daemonEnvironmentTestPaths(t *testing.T) *paths.Paths {
	t.Helper()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	for _, key := range []string{"TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
		t.Setenv(key, os.Getenv(key))
	}
	return paths.WithRoot(root)
}

func TestPrepareDaemonEnvironment_OwnsTemporaryDirectory(t *testing.T) {
	for _, name := range []string{"caller", "login_shell", "relative_home", "resolved_home"} {
		t.Run(name, func(t *testing.T) {
			p := daemonEnvironmentTestPaths(t)
			root := p.Root()
			if name == "relative_home" {
				t.Chdir(root)
				p = paths.WithRoot("relative-home")
				t.Setenv("NM_HOME", p.Root())
				root = filepath.Join(root, "relative-home")
			}
			if name == "resolved_home" {
				t.Setenv("NM_HOME", "")
			}
			for _, key := range []string{"TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
				t.Setenv(key, filepath.Join(root, "caller-scratch"))
			}
			oldApply := applyShellEnvToProcess
			t.Cleanup(func() { applyShellEnvToProcess = oldApply })
			applyShellEnvToProcess = func(...string) error {
				if name == "resolved_home" {
					if err := os.Setenv("NM_HOME", filepath.Join(root, "shell-home")); err != nil {
						return err
					}
				}
				if name == "login_shell" || name == "resolved_home" {
					for _, key := range []string{"TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
						if err := os.Setenv(key, filepath.Join(root, "shell-scratch")); err != nil {
							return err
						}
					}
				}
				return nil
			}

			if err := prepareDaemonEnvironment(p); err != nil {
				t.Fatal(err)
			}

			want := filepath.Join(root, "tmp")
			for _, key := range []string{"TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
				if got := os.Getenv(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			file, err := os.CreateTemp("", "daemon-temp-*")
			if err != nil {
				t.Fatalf("create temporary file: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(file.Name()) != want {
				t.Errorf("temporary file = %q, want parent %q", file.Name(), want)
			}
			if err := os.Remove(file.Name()); err != nil {
				t.Fatal(err)
			}
			if entries, err := os.ReadDir(want); err != nil || len(entries) != 0 {
				t.Fatalf("temporary directory contains leftover probes: %v, %v", entries, err)
			}
		})
	}
}

func TestPrepareDaemonEnvironment_RejectsUnusableTemporaryDirectory(t *testing.T) {
	for _, name := range []string{"file", "unwritable"} {
		t.Run(name, func(t *testing.T) {
			p := daemonEnvironmentTestPaths(t)
			destination := filepath.Join(p.Root(), "tmp")
			if name == "file" {
				if err := os.WriteFile(destination, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(destination, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(destination, 0o700) })
				if runtime.GOOS == "windows" {
					changeACL := func(args ...string) {
						t.Helper()
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						command := exec.CommandContext(ctx, "icacls.exe", append([]string{destination}, args...)...)
						winproc.Harden(command)
						if out, err := command.CombinedOutput(); err != nil {
							t.Fatalf("set test directory permissions: %v\n%s", err, out)
						}
					}
					t.Cleanup(func() { changeACL("/remove:d", "*S-1-1-0") })
					changeACL("/deny", "*S-1-1-0:(W)")
				}
				if file, err := os.CreateTemp(destination, "permission-check-*"); err == nil {
					file.Close()
					if runtime.GOOS == "windows" {
						t.Fatal("test directory is still writable after denying writes")
					}
					t.Skip("current user can write to a read-only directory")
				}
			}
			oldApply := applyShellEnvToProcess
			t.Cleanup(func() { applyShellEnvToProcess = oldApply })
			applyShellEnvToProcess = func(...string) error { return nil }

			err := prepareDaemonEnvironment(p)

			if err == nil || !strings.Contains(err.Error(), destination) {
				t.Fatalf("startup error = %v, want unusable temporary directory %q", err, destination)
			}
		})
	}
}

func TestPrepareDaemonEnvironment_RemovesClaudeSessionVarsAndAppliesShellEnv(t *testing.T) {
	p := daemonEnvironmentTestPaths(t)
	for key, value := range map[string]string{
		"CLAUDECODE":                       "1",
		"CLAUDE_CODE_ENTRYPOINT":           "shell",
		"CLAUDE_CODE_ENTRY_POINT":          "shell",
		"CLAUDE_CODE_SESSION_ID":           "session",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN": "token",
	} {
		t.Setenv(key, value)
	}
	t.Setenv("PATH", os.Getenv("PATH"))

	oldApply := applyShellEnvToProcess
	defer func() { applyShellEnvToProcess = oldApply }()

	applied := false
	applyShellEnvToProcess = func(...string) error {
		applied = true
		return os.Setenv("PATH", "/resolved/bin")
	}

	if err := prepareDaemonEnvironment(p); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("expected shell env application")
	}
	for _, key := range []string{
		"CLAUDECODE",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_ENTRY_POINT",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN",
	} {
		if got := os.Getenv(key); got != "" {
			t.Fatalf("expected %s to be cleared, got %q", key, got)
		}
	}
	if got := os.Getenv("PATH"); got != "/resolved/bin" {
		t.Fatalf("expected applied PATH, got %q", got)
	}
}

// A daemon started from an orchestration harness's worker terminal must not
// hand that worker's identity to the gate agents it starts, or their hooks
// report as a worker long gone (#74). The installation variables stay.
func TestPrepareDaemonEnvironment_DropsTheLaunchingWorkersIdentity(t *testing.T) {
	// Arrange
	p := daemonEnvironmentTestPaths(t)
	dropped := []string{
		"CFO_ROLE", "CFO_TASK_ID", "CFO_SPAWN_GEN",
		"CFO_HOST_ID", "CFO_HOST_PROOF",
		"CFO_PARENT_SESSION_ID", "CFO_PARENT_HARNESS", "CFO_ROOT_SESSION_ID", "CFO_SESSION_ID", "CFO_SESSION_HARNESS",
		"CHROME_DEVTOOLS_AXI_SESSION",
		"CFO_CODEX_GUARD",
	}
	kept := map[string]string{
		"CFO_HOME":           filepath.Join(p.Root(), "fleet"),
		"CFO_STATE_OVERRIDE": filepath.Join(p.Root(), "fleet", "state"),
		"CFO_PROJECTS_ROOT":  filepath.Join(p.Root(), "projects"),
	}
	ownGuard := filepath.Join(p.Root(), "fleet", "state", "codex-guard", "worker-1")
	inheritedGuard := filepath.Join(p.Root(), "fleet", "state", "codex-guard", "rig-1")
	list := string(os.PathListSeparator)
	userPath := os.Getenv("PATH")
	for index, key := range dropped {
		value := "worker-1"
		if key == "CFO_CODEX_GUARD" {
			value = ownGuard + list + inheritedGuard
		}
		spelled := key
		if runtime.GOOS == "windows" && index%2 == 0 {
			spelled = strings.ToLower(key)
		}
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		if err := os.Setenv(spelled, value); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range kept {
		t.Setenv(key, value)
	}
	guardedPath := ownGuard + list + inheritedGuard + list + userPath
	if runtime.GOOS == "windows" {
		guardedPath = strings.ToUpper(ownGuard) + list + inheritedGuard + list + userPath
	}
	t.Setenv("PATH", guardedPath)
	oldApply := applyShellEnvToProcess
	t.Cleanup(func() { applyShellEnvToProcess = oldApply })
	var probed []string
	applyShellEnvToProcess = func(...string) error {
		probed = os.Environ()
		return nil
	}
	assertNoWorkerIdentity := func(t *testing.T, surface string, env []string) {
		t.Helper()
		values := map[string]string{}
		for _, entry := range env {
			name, value, _ := strings.Cut(entry, "=")
			if runtime.GOOS == "windows" {
				name = strings.ToUpper(name)
			}
			values[name] = value
			for _, key := range dropped {
				if name == key {
					t.Errorf("%s carries the worker's %s", surface, entry)
				}
			}
		}
		if values["NM_HOME"] != p.Root() {
			t.Errorf("%s NM_HOME = %q, want %q", surface, values["NM_HOME"], p.Root())
		}
		for key, value := range kept {
			if values[key] != value {
				t.Errorf("%s %s = %q, want %q", surface, key, values[key], value)
			}
		}
		if values["PATH"] != userPath {
			t.Errorf("%s PATH = %q, want the worker's PATH without its guard folders %q", surface, values["PATH"], userPath)
		}
	}

	// Act
	if err := prepareDaemonEnvironment(p); err != nil {
		t.Fatal(err)
	}
	agentEnvFile := filepath.Join(t.TempDir(), "agent-env.json")
	gateAgent, err := agent.New(types.AgentClaude, os.Args[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := gateAgent.Run(ctx, agent.RunOpts{
		CWD:    t.TempDir(),
		Prompt: "review",
		Env:    []string{"NM_DAEMON_HELPER_PROCESS=agent-env", "NM_TEST_AGENT_ENV_FILE=" + agentEnvFile},
	}); err != nil {
		t.Fatalf("run gate agent: %v", err)
	}
	recorded, err := os.ReadFile(agentEnvFile)
	if err != nil {
		t.Fatal(err)
	}
	var agentEnv []string
	if err := json.Unmarshal(recorded, &agentEnv); err != nil {
		t.Fatal(err)
	}

	// Assert
	assertNoWorkerIdentity(t, "login shell probe", probed)
	assertNoWorkerIdentity(t, "daemon", os.Environ())
	assertNoWorkerIdentity(t, "gate agent", agentEnv)
}

func TestPrepareDaemonEnvironment_PreservesExistingNMHome(t *testing.T) {
	p := daemonEnvironmentTestPaths(t)
	t.Setenv("PATH", os.Getenv("PATH"))

	oldApply := applyShellEnvToProcess
	defer func() { applyShellEnvToProcess = oldApply }()

	applyShellEnvToProcess = func(excluded ...string) error {
		protectNMHome := false
		for _, key := range excluded {
			protectNMHome = protectNMHome || key == "NM_HOME"
		}
		if !protectNMHome {
			if err := os.Setenv("NM_HOME", "/login/shell/root"); err != nil {
				return err
			}
		}
		return os.Setenv("PATH", "/resolved/bin")
	}

	if err := prepareDaemonEnvironment(p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("NM_HOME"); got != p.Root() {
		t.Fatalf("NM_HOME = %q, want %q", got, p.Root())
	}
	if got := os.Getenv("PATH"); got != "/resolved/bin" {
		t.Fatalf("PATH = %q, want %q", got, "/resolved/bin")
	}
}

// TestPrepareDaemonEnvironment_LogsPathSummary locks in observability for
// #143-style failures. Silent PATH regressions were the reason this bug
// took a forensic dive to diagnose - future bugs should be readable from
// the daemon log alone.
func TestPrepareDaemonEnvironment_LogsPathSummary(t *testing.T) {
	p := daemonEnvironmentTestPaths(t)
	t.Setenv("PATH", os.Getenv("PATH"))

	oldApply := applyShellEnvToProcess
	defer func() { applyShellEnvToProcess = oldApply }()
	applyShellEnvToProcess = func(...string) error {
		return os.Setenv("PATH", "/a/bin"+string(os.PathListSeparator)+"/b/bin"+string(os.PathListSeparator)+"/c/bin")
	}

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	if err := prepareDaemonEnvironment(p); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "daemon environment ready") {
		t.Fatalf("expected startup log line, got %q", out)
	}
	if !strings.Contains(out, "path_entries=3") {
		t.Fatalf("expected path_entries=3 in log, got %q", out)
	}
	if !strings.Contains(out, "/a/bin") {
		t.Fatalf("expected full PATH in log for debuggability, got %q", out)
	}
}
