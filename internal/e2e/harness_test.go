//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHarnessFakeExecutablesPreserveDispatchAndInterception(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "pi"})
	fakeInfo, err := os.Stat(h.FakeAgent)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex", "grok", "opencode", "pi", "antigravity", "agy", "gh", "tea"} {
		path := filepath.Join(h.BinDir, executableName(name))
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if !info.Mode().IsRegular() || !os.SameFile(fakeInfo, info) || info.Mode().Perm() != fakeInfo.Mode().Perm() {
			t.Fatalf("%s must share the fake executable and its permissions without a symlink", name)
		}
		resolved, err := exec.LookPath(name)
		if err != nil || resolved != path {
			t.Fatalf("resolve %s = %q, %v; want %q", name, resolved, err, path)
		}
	}

	t.Setenv("FAKEAGENT_SCENARIO", "")
	t.Setenv("FAKEAGENT_GH_MODE", "")
	for _, test := range []struct {
		name     string
		args     []string
		want     string
		wantExit int
	}{
		{name: "pi", want: `"type":"agent_end"`},
		{name: "gh", args: []string{"auth", "status"}, want: "fakeagent gh: not authenticated (e2e stub)", wantExit: 1},
		{name: "tea", args: []string{"api", "/user"}, want: `{"login":"e2e-tea-user"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, test.name, test.args...)
			command.Dir = h.WorkDir
			output, err := command.CombinedOutput()
			exitCode := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatalf("execute %s: %v\n%s", test.name, err, output)
				}
				exitCode = exitError.ExitCode()
			}
			if exitCode != test.wantExit || !strings.Contains(string(output), test.want) {
				t.Fatalf("%s: exit=%d, output=%s; want exit=%d containing %q", test.name, exitCode, output, test.wantExit, test.want)
			}
		})
	}
	invocations := h.AgentInvocations()
	if len(invocations) != 1 || invocations[0].Agent != "pi" {
		t.Fatalf("basename dispatch recorded %v, want one Pi invocation", invocations)
	}
}

// The pipeline starts a configured command through the platform shell, which is
// cmd.exe /c on Windows, so a journey's POSIX script fixture must start by its
// bare name there too (#373).
func TestShellCommandFixtureStartsUnderThePlatformShell(t *testing.T) {
	binDir := t.TempDir()
	workDir := t.TempDir()
	writeShellCommand(t, binDir, "nm-shell-command-e2e", "#!/bin/sh\nprintf ran > ran.txt\nexit 3\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", "-c", "nm-shell-command-e2e")
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(ctx, "cmd.exe", "/c", "nm-shell-command-e2e")
	}
	command.Dir = workDir

	output, err := command.CombinedOutput()

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 3 {
		t.Fatalf("start the fixture by name: %v, want its own exit code 3\n%s", err, output)
	}
	ran, err := os.ReadFile(filepath.Join(workDir, "ran.txt"))
	if err != nil || string(ran) != "ran" {
		t.Fatalf("fixture output = %q, %v; want %q", ran, err, "ran")
	}
}

func TestHarnessFixtureCheckoutIgnoresSystemGitConfig(t *testing.T) {
	systemConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(systemConfig, []byte("[core]\n\tautocrlf = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", systemConfig)
	h := NewHarness(t, SetupOpts{Agent: "pi", Scenario: writeReviewAgentsRoutingScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const (
		branch  = "feature/line-endings"
		content = "seed for review-agents routing\n"
	)
	h.CommitChange(branch, "feature.txt", content, "seed feature")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

	// Scripted agent edits run in the daemon's run worktree, not in a checkout
	// the harness makes itself, so that is the one whose bytes must hold.
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(h.repoID(), parked.ID)
	checkedOut, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(checkedOut) != content {
		t.Fatalf("fixture checkout of feature.txt = %q, want %q unchanged by the system git config", checkedOut, content)
	}
}

func TestFixtureRootFromRepoRoot(t *testing.T) {
	root, err := fixtureRootFromRepoRoot(t.TempDir())
	if err == nil {
		t.Fatalf("fixtureRootFromRepoRoot succeeded with %q, want error", root)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	root, err = fixtureRootFromRepoRoot(repoRoot)
	if err != nil {
		t.Fatalf("fixtureRootFromRepoRoot: %v", err)
	}
	want := filepath.Join(repoRoot, "internal", "e2e", "fixtures")
	if root != want {
		t.Fatalf("fixture root = %q, want %q", root, want)
	}
}

func TestDaemonStartTimeoutLeavesRoomForLoginShellProbe(t *testing.T) {
	if e2eDaemonStartTimeout <= 30*time.Second {
		t.Fatalf("e2eDaemonStartTimeout = %v, want more than the 30s login-shell probe budget", e2eDaemonStartTimeout)
	}
}

func TestCommitChangeCreatesMissingBranchFromMain(t *testing.T) {
	workDir := t.TempDir()
	h := &Harness{t: t, WorkDir: workDir}
	ctx := context.Background()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := h.runGit(ctx, workDir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	mustGit("init", "--initial-branch=main")
	mustGit("config", "user.email", "e2e@example.com")
	mustGit("config", "user.name", "E2E Test")
	mustGit("config", "commit.gpgsign", "false")

	readme := filepath.Join(workDir, "README.md")
	if err := os.WriteFile(readme, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	mustGit("add", "README.md")
	mustGit("commit", "-m", "initial commit")
	mainSHA := mustGit("rev-parse", "HEAD")

	mustGit("checkout", "-b", "feature/existing")
	featureOnly := filepath.Join(workDir, "feature-only.txt")
	if err := os.WriteFile(featureOnly, []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature-only.txt: %v", err)
	}
	mustGit("add", "feature-only.txt")
	mustGit("commit", "-m", "feature commit")

	h.CommitChange("feature/new", "hello.txt", "hello\n", "new branch commit")

	mergeBase := mustGit("merge-base", "feature/new", "main")
	if mergeBase != mainSHA {
		t.Fatalf("merge-base(feature/new, main) = %s, want %s", mergeBase, mainSHA)
	}
	if _, err := os.Stat(filepath.Join(workDir, "feature-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature-only.txt present on new branch, want branch rooted at main")
	}
	show := mustGit("show", "feature/new:hello.txt")
	if show != "hello" {
		t.Fatalf("hello.txt contents = %q, want %q", show, "hello")
	}
}

func TestDaemonStopDirFallsBackWhenWorkDirMoved(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "missing-work")
	homeDir := t.TempDir()
	h := &Harness{WorkDir: workDir, HomeDir: homeDir}

	if got := h.daemonStopDir(); got != homeDir {
		t.Fatalf("daemonStopDir() = %q, want home dir %q", got, homeDir)
	}
}

func TestWaitForRunScalesTimeoutAndPrefersNewestRunOnBranch(t *testing.T) {
	t.Setenv("NM_E2E_TIMEOUT_SCALE", "10000")
	nmHome, err := os.MkdirTemp("", "nm-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nmHome) })
	workDir := t.TempDir()
	p := paths.WithRoot(nmHome)
	if err := os.MkdirAll(nmHome, 0o755); err != nil {
		t.Fatalf("mkdir nm home: %v", err)
	}

	server := ipc.NewServer()
	var calls atomic.Int32
	server.Handle(ipc.MethodGetRuns, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		if calls.Add(1) == 1 {
			return ipc.GetRunsResult{Runs: []ipc.RunInfo{
				{ID: "run-new", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunRunning, CreatedAt: 20, UpdatedAt: 20},
				{ID: "run-old", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunCompleted, CreatedAt: 10, UpdatedAt: 10},
			}}, nil
		}
		return ipc.GetRunsResult{Runs: []ipc.RunInfo{
			{ID: "run-new", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunCompleted, CreatedAt: 20, UpdatedAt: 30},
			{ID: "run-old", RepoID: "ignored", Branch: "feature/e2e", Status: types.RunCompleted, CreatedAt: 10, UpdatedAt: 10},
		}}, nil
	})

	if err := server.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ServeReady()
	}()
	t.Cleanup(func() {
		server.Close()
		if err := <-errCh; err != nil {
			t.Errorf("ipc server: %v", err)
		}
	})

	h := &Harness{t: t, NMHome: nmHome, WorkDir: workDir}
	run := h.WaitForRun("feature/e2e", time.Millisecond)
	if run.ID != "run-new" {
		t.Fatalf("WaitForRun returned %q, want newest run", run.ID)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("WaitForRun status = %s, want %s", run.Status, types.RunCompleted)
	}
	if calls.Load() < 2 {
		t.Fatalf("GetRuns calls = %d, want at least 2 polls", calls.Load())
	}
}
