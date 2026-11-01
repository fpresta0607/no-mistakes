package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOrphanCleanupRechecksSourceFlushedDuringSweep(t *testing.T) {
	for _, cleanupPath := range []string{"default reap", "recorded startup"} {
		for _, sourceState := range []string{"tracked", "staged", "untracked", "unreadable", "clean"} {
			t.Run(cleanupPath+"/"+sourceState, func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				database, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { database.Close() })
				repo, head := setupTestGitRepo(t, p, database, "sweep-flush")
				run, err := database.InsertRun(repo.ID, "feature", head, head)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				managed := p.WorktreeDir(repo.ID, run.ID)
				if cleanupPath == "recorded startup" {
					managed = filepath.Join(t.TempDir(), "recorded", run.ID)
					if err := database.SetRunWorktreeDir(run.ID, managed); err != nil {
						t.Fatal(err)
					}
				}
				if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), managed, head); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, managed, "config", "status.showUntrackedFiles", "no")
				if isDirty, err := git.HasUncommittedChanges(context.Background(), managed); err != nil || isDirty {
					t.Fatalf("initial source is not clean: dirty=%v, %v", isDirty, err)
				}
				manager := NewRunManager(database, p, nil)
				originalSweep := sweepRunWorktrees
				t.Cleanup(func() { sweepRunWorktrees = originalSweep })
				sweepCount := 0
				flushedFile, flushedContent := "test.txt", "shutdown-flushed source\n"
				if sourceState == "untracked" {
					flushedFile = ".shutdown-flush"
				} else if sourceState == "unreadable" {
					flushedFile, flushedContent = ".git", "unreadable managed checkout\n"
				}
				sweepRunWorktrees = func(_ string, worktrees []procreap.Worktree, _ string) {
					sweepCount++
					if len(worktrees) != 1 || worktrees[0].RunID != run.ID || !samePath(worktrees[0].Dir, managed) {
						t.Fatalf("sweep did not target the terminal checkout: %+v", worktrees)
					}
					observed, err := database.GetRun(run.ID)
					if err != nil || observed.TerminalSourceState == nil || *observed.TerminalSourceState != "clean:"+head {
						t.Fatalf("source was not observed clean before the sweep: %+v, %v", observed, err)
					}
					if cleanupPath == "default reap" {
						branchMutex := manager.branchMutex(repo.ID, run.Branch)
						if branchMutex.TryLock() {
							branchMutex.Unlock()
							t.Fatal("sweep did not retain the continuation branch lock")
						}
					}
					if sourceState != "clean" {
						if err := os.WriteFile(filepath.Join(managed, flushedFile), []byte(flushedContent), 0o600); err != nil {
							t.Fatal(err)
						}
						if sourceState == "staged" {
							gitCmd(t, managed, "add", flushedFile)
						}
					}
				}

				if cleanupPath == "default reap" {
					manager.reapWorktrees(worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
				} else {
					cleanupOrphanWorktrees(database, p, leftoverRecordedRunWorktrees(database, p))
				}

				if sweepCount != 1 {
					t.Fatalf("scoped sweeps = %d, want 1", sweepCount)
				}
				expectedObservation := "clean:" + head
				if sourceState == "clean" {
					if _, err := os.Stat(managed); !os.IsNotExist(err) {
						t.Fatalf("clean control was not removed: %v", err)
					}
				} else {
					if content, err := os.ReadFile(filepath.Join(managed, flushedFile)); err != nil || string(content) != flushedContent {
						t.Fatalf("shutdown-flushed source was lost: %q, %v", content, err)
					}
					expectedObservation = "dirty:" + head
					if sourceState == "unreadable" {
						expectedObservation = "unreadable"
					} else if sourceState == "staged" {
						if staged := gitOutput(t, managed, "diff", "--cached", "--name-only"); staged != flushedFile {
							t.Fatalf("shutdown-flushed index was lost: %q", staged)
						}
					}
				}
				current, err := database.GetRun(run.ID)
				if err != nil || current.TerminalSourceState == nil || *current.TerminalSourceState != expectedObservation {
					t.Fatalf("post-sweep observation = %+v, want %q: %v", current, expectedObservation, err)
				}
			})
		}
	}
}

// TestCleanupOrphanWorktreesSweepsEveryRemovableDirectoryInOneSnapshot bounds
// what startup cleanup pays for the sweep that has to precede every removal.
// One snapshot per removal pass makes the daemon slower to start the more a
// crash left behind: a scoped sweep has no age floor, so each snapshot
// enumerates every process on the machine (a whole-machine lsof on darwin,
// capped at 10s), and the whole pass runs before the socket is bound, against
// the startup budget. Startup runs two passes - reapWorktrees for the default
// tree (retention-aware) and cleanupOrphanWorktrees for operator-placed
// leftovers (unconditional) - and each must still sweep its own directories
// in one snapshot rather than one per directory. Per-directory decisions must
// not change: an active run's worktree is neither swept nor removed.
func TestCleanupOrphanWorktreesSweepsEveryRemovableDirectoryInOneSnapshot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	workingPath := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(workingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo1", workingPath, "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}

	defaultTree := filepath.Join(p.WorktreesDir(), repo.ID)

	// Two finished runs in the default tree, one in a directory of the
	// operator's own, and one run still executing.
	firstDefault := placeCleanupRun(t, d, repo.ID, defaultTree, types.RunFailed)
	secondDefault := placeCleanupRun(t, d, repo.ID, defaultTree, types.RunCompleted)
	operatorRoot := filepath.Join(t.TempDir(), "repo-runs")
	recorded := placeCleanupRun(t, d, repo.ID, operatorRoot, types.RunFailed)
	activeDefault := placeCleanupRun(t, d, repo.ID, defaultTree, types.RunRunning)

	var sweeps [][]procreap.Worktree
	original := sweepRunWorktrees
	t.Cleanup(func() { sweepRunWorktrees = original })
	sweepRunWorktrees = func(worktreesRoot string, wts []procreap.Worktree, reason string) {
		sweeps = append(sweeps, wts)
	}

	// Force every eligible default-tree directory past the retention window
	// regardless of its actual mtime, mirroring recoverOnStartup's order.
	future := time.Now().Add(365 * 24 * time.Hour)
	NewRunManager(d, p, nil).reapWorktrees(worktreeReapPolicy{Retention: time.Nanosecond}, future)
	cleanupOrphanWorktrees(d, p, leftoverRecordedRunWorktrees(d, p))

	if len(sweeps) != 2 {
		t.Fatalf("startup cleanup ran %d sweeps, want 2 (default tree, then operator-placed leftovers)", len(sweeps))
	}
	swept := map[string]bool{}
	for _, batch := range sweeps {
		for _, wt := range batch {
			swept[wt.Dir] = true
		}
	}
	for _, dir := range []string{firstDefault, secondDefault, recorded} {
		if !swept[dir] {
			t.Errorf("removable worktree %q was not swept before removal", dir)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("worktree %q survived cleanup, stat err: %v", dir, err)
		}
	}
	if swept[activeDefault] {
		t.Errorf("cleanup swept %q, whose run is still executing", activeDefault)
	}
	if _, err := os.Stat(activeDefault); err != nil {
		t.Errorf("cleanup removed the worktree of a running run: %v", err)
	}
}

// placeCleanupRun records a run placed in root, creates its directory, and
// leaves it in the given status.
func placeCleanupRun(t *testing.T, d *db.DB, repoID, root string, status types.RunStatus) string {
	t.Helper()
	run, err := d.InsertRun(repoID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, run.ID)
	if err := d.SetRunWorktreeDir(run.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, status); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
