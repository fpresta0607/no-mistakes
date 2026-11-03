package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// crashedRunFixture is a running run whose managed worktree holds a committed
// pipeline fix past the submitted head, as a daemon crash leaves it.
type crashedRunFixture struct {
	root      *paths.Paths
	database  *db.DB
	run       *db.Run
	gate      string
	managed   string
	submitted string
	preserved string
}

func newCrashedRunFixture(t *testing.T) crashedRunFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	source := filepath.Join(t.TempDir(), "source")
	gitCmd(t, "", "init", source)
	gitCmd(t, source, "config", "user.email", "test@test.com")
	gitCmd(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("submitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", "file.txt")
	gitCmd(t, source, "commit", "-m", "submitted")
	submitted := gitOutput(t, source, "rev-parse", "HEAD")

	repo, err := d.InsertRepoWithID("crash-repo", source, "https://example.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/crash", submitted, submitted)
	if err != nil {
		t.Fatal(err)
	}
	gate := p.RepoDir(repo.ID)
	gitCmd(t, "", "init", "--bare", gate)
	gitCmd(t, source, "push", gate, "HEAD:refs/heads/feature/crash")
	managed := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), gate, managed, submitted); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, managed, "config", "user.email", "test@test.com")
	gitCmd(t, managed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(managed, "fix.txt"), []byte("pipeline fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, managed, "add", "fix.txt")
	gitCmd(t, managed, "commit", "-m", "pipeline fix")
	preserved := gitOutput(t, managed, "rev-parse", "HEAD")
	return crashedRunFixture{root: p, database: d, run: run, gate: gate, managed: managed, submitted: submitted, preserved: preserved}
}

func TestPreserveStaleRunHeadsAnchorsCrashWorkBeforeTerminalization(t *testing.T) {
	t.Parallel()
	fixture := newCrashedRunFixture(t)
	d, p, run, gate, submitted, preserved := fixture.database, fixture.root, fixture.run, fixture.gate, fixture.submitted, fixture.preserved

	preserveStaleRunHeads(d, p, nil)
	if got := gitOutput(t, gate, "rev-parse", custody.RecoveryRef(run.ID)); got != preserved {
		t.Fatalf("startup recovery anchor = %s, want %s", got, preserved)
	}
	if _, err := d.RecoverStaleRunsExcept("daemon crashed during execution", nil); err != nil {
		t.Fatal(err)
	}
	terminal, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != types.RunFailed || terminal.HeadSHA != preserved || terminal.TerminalHeadVerifiedAt == nil {
		t.Fatalf("crash-terminalized run = status %s head %s verified %#v", terminal.Status, terminal.HeadSHA, terminal.TerminalHeadVerifiedAt)
	}
	if got := gitOutput(t, gate, "rev-parse", "refs/heads/feature/crash"); got != submitted {
		t.Fatalf("startup preservation moved gate branch = %s, want %s", got, submitted)
	}
}

func TestPreserveRunHeadAnchorsCrashWorkWhenTheSourceObservationStalls(t *testing.T) {
	helperDirectory := t.TempDir()
	helperPath := filepath.Join(helperDirectory, "git")
	if runtime.GOOS == "windows" {
		helperPath += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-ldflags=-s -w", "-o", helperPath, "../pipeline/fakecli")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake git: %v\n%s", err, output)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	fixture := newCrashedRunFixture(t)
	// The status scan outlives the observation's whole budget, the way a cold
	// large checkout does right after a crash or reboot.
	t.Setenv("FAKE_CLI_MODE", "git-hung-status")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("PATH", helperDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	head, isPreserved := preserveRunHead(fixture.database, fixture.managed, fixture.run)

	if !isPreserved || head != fixture.preserved {
		t.Fatalf("preserveRunHead = %q, %v; want the crash work %s anchored", head, isPreserved, fixture.preserved)
	}
	if got := gitOutput(t, fixture.gate, "rev-parse", custody.RecoveryRef(fixture.run.ID)); got != fixture.preserved {
		t.Fatalf("recovery anchor = %s, want %s", got, fixture.preserved)
	}
	observed, err := fixture.database.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.TerminalSourceState == nil || *observed.TerminalSourceState != "unreadable" {
		t.Fatalf("stalled observation recorded %v, want unreadable", observed.TerminalSourceState)
	}
}

func TestPreserveRunHeadAnchorsASuccessorUnmovedAtACandidateItsCallerNeverHad(t *testing.T) {
	t.Parallel()
	fixture := newCrashedRunFixture(t)
	d, run, submitted, preserved := fixture.database, fixture.run, fixture.submitted, fixture.preserved
	profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {profile}, "reviewer": {profile}, "fixer": {profile}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	// The crashed fixture run holds its own branch, so the continuation uses another.
	predecessor, err := d.InsertRunWithLaunchAssertion(run.RepoID, "feature/successor", submitted, submitted, nil, "nonce", "generation", "intent", "", false, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(predecessor.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatusWithVerifiedHead(predecessor.ID, "interrupted", types.RunFailed, preserved); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.ReadContinuationSnapshot(predecessor.ID)
	if err != nil {
		t.Fatal(err)
	}
	adoption, _, err := d.AdoptTerminalCandidate(context.Background(), db.TerminalAdoption{Snapshot: *snapshot, CandidateSHA: preserved, CandidateTree: gitOutput(t, fixture.gate, "rev-parse", preserved+"^{tree}"), SourceRef: "refs/no-mistakes/terminal-candidate/successor", CallerDir: t.TempDir(), CallerHead: submitted, Reason: "validate preserved source", Operator: "fixture", Assertion: assertion, Proof: proof})
	if err != nil {
		t.Fatal(err)
	}
	successor, err := d.GetRun(adoption.Successor.ID)
	if err != nil {
		t.Fatal(err)
	}

	head, isPreserved := preserveRunHead(d, fixture.managed, successor)

	if !isPreserved || head != preserved {
		t.Fatalf("preserveRunHead = %q, %v; want the owed candidate %s anchored", head, isPreserved, preserved)
	}
	if got := gitOutput(t, fixture.gate, "rev-parse", custody.RecoveryRef(successor.ID)); got != preserved {
		t.Fatalf("successor recovery anchor = %s, want %s", got, preserved)
	}
}

// TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree is the regression test
// for the second half of the duplicate-daemon wedge: startup cleanup used to
// remove every worktree directory under the shared root with no check of the
// owning run's status, so a duplicate daemon's cleanup could delete the
// checkout out from under a pipeline that was still actively running in it
// (observed live as "chdir .../worktrees/...: no such file or directory").
// A worktree whose run row is pending or running must survive cleanup.
//
// This exercises cleanupOrphanWorktrees directly (rather than the full
// recoverOnStartup) because RecoverStaleRuns, which runs first in
// production, unconditionally marks every pending/running run failed - so by
// design there is no pending/running row left by the time cleanup runs in
// the normal single-daemon path. Testing cleanupOrphanWorktrees in isolation
// verifies its DB-aware skip logic as defense in depth, independent of
// whatever recovery step runs before it.
func TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	activeRun, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if activeRun.Status != types.RunPending {
		t.Fatalf("expected new run to default to pending, got %s", activeRun.Status)
	}

	activeWT := p.WorktreeDir(repo.ID, activeRun.ID)
	if err := os.MkdirAll(activeWT, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeWT+"/marker", []byte("still running"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A terminal run's worktree, for contrast: cleanup should remove this one.
	terminalRun, err := d.InsertRun(repo.ID, "old-branch", "headsha2", "basesha2")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(terminalRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	terminalWT := p.WorktreeDir(repo.ID, terminalRun.ID)
	if err := os.MkdirAll(terminalWT, 0o755); err != nil {
		t.Fatal(err)
	}

	// The default-tree removal decision belongs to reapWorktrees (retention
	// aware); force every eligible directory past the retention window
	// regardless of its actual mtime, mirroring recoverOnStartup's order.
	future := time.Now().Add(365 * 24 * time.Hour)
	NewRunManager(d, p, nil).reapWorktrees(worktreeReapPolicy{Retention: time.Nanosecond}, future)
	cleanupOrphanWorktrees(d, p, leftoverRecordedRunWorktrees(d, p))

	if _, err := os.Stat(activeWT); err != nil {
		t.Fatalf("active run worktree must survive cleanup, got: %v", err)
	}
	if _, err := os.Stat(terminalWT); !os.IsNotExist(err) {
		t.Fatalf("terminal run worktree should have been cleaned up, stat err: %v", err)
	}

	got, err := d.GetRun(activeRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending {
		t.Fatalf("expected active run to remain pending, got %s", got.Status)
	}
}

// TestRunWithOptions_RequiresSingletonLockBeforeRecovery proves the ordering
// the fix depends on: when another process already holds the singleton lock
// for this root, RunWithOptions must fail before ever calling
// RecoverStaleRuns, so a duplicate daemon can never mark a live daemon's
// active runs as crashed.
func TestRunWithOptions_RequiresSingletonLockBeforeRecovery(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate another live daemon already owning this root.
	lock, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := RunWithOptions(p, d, nil); err == nil {
		t.Fatal("expected RunWithOptions to fail while the singleton lock is held elsewhere")
	} else if !errors.Is(err, ErrSingletonLockHeld) {
		t.Fatalf("expected ErrSingletonLockHeld, got %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending {
		t.Fatalf("recovery must not have run: expected run to remain pending, got %s", got.Status)
	}
}
