package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestWorktreeReapObservesAKeptWorktreeAgainOnlyWhenItsReasonCanHaveChanged
// is the issue #367 regression: every finished run's sweep re-observed each
// leftover it kept with Git, so a long-lived daemon repeated the same Git work
// on every run and never converged. A second sweep over an unchanged kept
// worktree must start no Git process, and a change to any input of the keep
// decision must still be observed and release the worktree.
func TestWorktreeReapObservesAKeptWorktreeAgainOnlyWhenItsReasonCanHaveChanged(t *testing.T) {
	gitLog := installCountingGit(t)
	for _, test := range []struct {
		name    string
		keep    func(*testing.T, string, *db.Run)
		release func(*testing.T, *db.DB, string, *db.Run)
	}{
		{
			name: "dirty source cleaned",
			keep: writeUntrackedSource,
			release: func(t *testing.T, _ *db.DB, managed string, _ *db.Run) {
				if err := os.Remove(filepath.Join(managed, "scratch.txt")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unpushed correction anchored",
			keep: func(t *testing.T, managed string, _ *db.Run) {
				gitCmd(t, managed, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit", "--allow-empty", "-m", "unpushed correction")
			},
			release: func(t *testing.T, _ *db.DB, managed string, run *db.Run) {
				if err := custody.PreserveRecoveryHead(context.Background(), managed, run.ID, gitOutput(t, managed, "rev-parse", "HEAD")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "superseded by a newer run",
			keep: writeUntrackedSource,
			release: func(t *testing.T, database *db.DB, _ string, run *db.Run) {
				if _, err := database.InsertRun(run.RepoID, run.Branch, run.HeadSHA, run.BaseSHA); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: a failed run continuation could still accept left a
			// worktree the reaper must keep.
			manager, p, database, repo, head, _, _ := launchAssertionFixture(t, nil)
			run, err := database.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			managed := p.WorktreeDir(repo.ID, run.ID)
			if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), managed, head); err != nil {
				t.Fatal(err)
			}
			test.keep(t, managed, run)
			if err := database.UpdateRunErrorStatus(run.ID, "run ended", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			policy, later := worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour)
			sweep := func() string {
				t.Helper()
				if err := os.WriteFile(gitLog, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				manager.reapWorktrees(policy, later)
				content, err := os.ReadFile(gitLog)
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(content))
			}
			if invocations := sweep(); !strings.Contains(invocations, "status") {
				t.Fatalf("first sweep did not observe the kept worktree's source with Git: %q", invocations)
			}
			if _, err := os.Stat(managed); err != nil {
				t.Fatalf("first sweep did not keep the worktree: %v", err)
			}

			// Act
			unchanged := sweep()
			test.release(t, database, managed, run)
			sweep()

			// Assert
			if unchanged != "" {
				t.Fatalf("second sweep over the unchanged kept worktree started Git: %q", unchanged)
			}
			if _, err := os.Stat(managed); !os.IsNotExist(err) {
				t.Fatalf("sweep after the keep reason changed did not release the worktree: %v", err)
			}
		})
	}
}

func writeUntrackedSource(t *testing.T, managed string, _ *db.Run) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(managed, "scratch.txt"), []byte("unpublished work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// installCountingGit puts the fake CLI on PATH as git, forwarding every
// invocation to the real binary and appending its arguments to the returned log.
func installCountingGit(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	helperPath := filepath.Join(binDir, "git")
	if runtime.GOOS == "windows" {
		helperPath += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-ldflags=-s -w", "-o", helperPath, "../pipeline/fakecli")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build counting git: %v\n%s", err, output)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	gitLog := filepath.Join(t.TempDir(), "git.log")
	t.Setenv("FAKE_CLI_MODE", "git-passthrough")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("FAKE_CLI_LOG", gitLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return gitLog
}
