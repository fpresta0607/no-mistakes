package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rebaseOntoStep stands in for a Rebase that moved the run's head onto a newer
// base: the head it leaves is a rewrite of the submitted head, never its
// descendant, and it is recorded by the real Rebase step's head writer.
type rebaseOntoStep struct{ newBase string }

func (*rebaseOntoStep) Name() types.StepName { return types.StepRebase }

func (s *rebaseOntoStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "rebase", "--onto", s.newBase, sctx.Run.BaseSHA); err != nil {
		return nil, err
	}
	return updateHeadSHA(sctx.Ctx, sctx)
}

type dirtyReviewFailure struct {
	gate, caller, managed string
	submitted, head       string
	database              *db.DB
	run                   *db.Run
	service               *branchsync.Service
}

// runDirtyReviewFailure drives a run whose Review fix agent is killed
// mid-edit, so the run fails at Review with its partial edit still in the
// managed worktree. With rebase, Rebase first moves the head onto a newer main
// (issue #262); without it, the head never moves from the submitted head
// (issue #414).
func runDirtyReviewFailure(t *testing.T, rebase bool) *dirtyReviewFailure {
	t.Helper()
	gate, base, submitted := setupGitRepo(t)
	gitCmd(t, gate, "checkout", "main")
	if err := os.WriteFile(filepath.Join(gate, "main.txt"), []byte("newer main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, gate, "add", "main.txt")
	gitCmd(t, gate, "commit", "-m", "newer main")
	newBase := gitCmd(t, gate, "rev-parse", "HEAD")
	reviewBase := base
	steps := []pipeline.Step{&ReviewStep{}}
	if rebase {
		reviewBase = newBase
		steps = []pipeline.Step{&rebaseOntoStep{newBase: newBase}, &ReviewStep{}}
	}
	caller := filepath.Join(t.TempDir(), "caller")
	gitCmd(t, t.TempDir(), "clone", "--branch", "feature", gate, caller)
	managed := filepath.Join(t.TempDir(), "managed")
	gitCmd(t, gate, "worktree", "add", "--detach", managed, submitted)

	ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "review-fix" {
			if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("partial repair\n"), 0o600); err != nil {
				return nil, err
			}
			return nil, errors.New("agent exited with status 143")
		}
		findings := cleanReviewFindings()
		findings.ReviewedPaths = fullReviewCoverage(t, opts.CWD, reviewBase)
		findings.Items = []Finding{{ID: "repair-1", Severity: "error", File: "feature.txt", Description: "repair the source", Action: "auto-fix", ReviewScope: types.FindingReviewScopeSource}}
		encoded, err := json.Marshal(findings)
		return &agent.Result{Output: encoded}, err
	}}
	sctx := newTestContext(t, ag, managed, base, submitted, config.Commands{})
	repo, err := sctx.DB.InsertRepo(caller, gate, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.InsertRun(repo.ID, "feature", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetRunWorktreeDir(run.ID, managed); err != nil {
		t.Fatal(err)
	}
	sctx.Config.AutoFix.Review = 1
	appPaths := paths.WithRoot(t.TempDir())
	executor := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, steps, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := executor.Execute(ctx, run, repo, managed); err == nil {
		t.Fatal("the run whose Review fix agent was killed did not fail")
	}
	head := gitCmd(t, managed, "rev-parse", "HEAD")
	if (head != submitted) != rebase || gitCmd(t, managed, "status", "--porcelain") == "" {
		t.Fatalf("the fixture left head %s (submitted %s, rebase %v), want a dirty managed worktree", head, submitted, rebase)
	}
	recorded, err := sctx.DB.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Status != types.RunFailed || recorded.HeadSHA != head || recorded.TerminalHeadVerifiedAt != nil || recorded.TerminalSourceState == nil || *recorded.TerminalSourceState != "dirty:"+head {
		t.Fatalf("terminal record = status %s head %s verified %v source %q, want failed at the unverified dirty head %s", recorded.Status, recorded.HeadSHA, recorded.TerminalHeadVerifiedAt, deref(recorded.TerminalSourceState), head)
	}
	return &dirtyReviewFailure{
		gate: gate, caller: caller, managed: managed, submitted: submitted, head: head,
		database: sctx.DB, run: recorded,
		service: &branchsync.Service{DB: sctx.DB, Paths: appPaths, Repo: repo, WorkDir: caller, GateDir: filepath.Join(gate, ".git")},
	}
}

// TestRebasedRunFailingAtReviewWithDirtySourceRecoversByTheSupportedPath pins
// issue #262: status offered sync --recover for this run, and recovery refused
// it for good because its rebased head was unverified.
func TestRebasedRunFailingAtReviewWithDirtySourceRecoversByTheSupportedPath(t *testing.T) {
	t.Parallel()
	f := runDirtyReviewFailure(t, true)
	ctx := context.Background()
	if state := f.service.InspectCached(ctx); state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("status = %s/%s, want the supported recovery offered", state.State, state.Safety)
	}

	recovered := f.service.Recover(ctx, false)

	if !recovered.Recovered || recovered.Safety != "custody_returned" {
		t.Fatalf("recover = %s/%s: %s", recovered.State, recovered.Safety, recovered.Error)
	}
	if head := gitCmd(t, f.caller, "rev-parse", "refs/heads/feature"); head != f.head || gitCmd(t, f.caller, "rev-parse", "HEAD") != f.head {
		t.Fatalf("caller branch = %s, want the rebased head %s", head, f.head)
	}
	if gitCmd(t, f.caller, "status", "--porcelain") != "" {
		t.Fatal("recovery left the caller worktree dirty")
	}
	if local := gitCmd(t, f.caller, "rev-parse", custody.RecoveryLocalRef(f.run.ID)); local != f.submitted {
		t.Fatalf("pre-recovery head anchor = %s, want %s", local, f.submitted)
	}
	if anchored := gitCmd(t, f.gate, "rev-parse", custody.RecoveryRef(f.run.ID)); anchored != f.head {
		t.Fatalf("gate recovery ref = %s, want %s", anchored, f.head)
	}
	source := custody.RecoverySourceRef(f.run.ID)
	if parent := gitCmd(t, f.gate, "rev-parse", source+"^"); parent != f.head {
		t.Fatalf("preserved source parent = %s, want %s", parent, f.head)
	}
	if edit := gitCmd(t, f.gate, "show", source+":feature.txt"); edit != "partial repair" {
		t.Fatalf("preserved edit = %q", edit)
	}
	recorded, err := f.database.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.HeadSHA != f.head || recorded.TerminalHeadVerifiedAt == nil || recorded.CustodyReturnedAt == nil {
		t.Fatalf("run = head %s verified %v returned %v, want custody returned at %s", recorded.HeadSHA, recorded.TerminalHeadVerifiedAt, recorded.CustodyReturnedAt, f.head)
	}
}

func TestRebasedRunFailingAtReviewWithDirtySourceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, f *dirtyReviewFailure)
	}{
		{name: "dirty source discarded", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.managed, "checkout", "--", "feature.txt")
		}},
		{name: "managed worktree removed", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.gate, "worktree", "remove", "--force", f.managed)
		}},
		{name: "managed worktree moved off the recorded head", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.managed, "commit", "-am", "operator commit")
		}},
		{name: "recovery ref moved", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.submitted)
		}},
		{name: "source observation unreadable", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			if err := f.database.RecordTerminalSourceState(f.run.ID, "unreadable"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := runDirtyReviewFailure(t, true)
			test.mutate(t, f)

			assertDirtyRecoveryRefused(t, f)
		})
	}
}

// TestUnmovedRunFailingAtReviewWithDirtySourcePreservesItOnRecovery pins issue
// #414: with no Rebase the recorded head is the gate branch head, so recovery
// verified it as the gate head and returned custody without preserving the
// partial edit, which the next worktree cleanup then deleted.
func TestUnmovedRunFailingAtReviewWithDirtySourcePreservesItOnRecovery(t *testing.T) {
	t.Parallel()
	f := runDirtyReviewFailure(t, false)
	ctx := context.Background()
	if state := f.service.InspectCached(ctx); state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("status = %s/%s, want the supported recovery offered", state.State, state.Safety)
	}

	recovered := f.service.Recover(ctx, false)

	if !recovered.Recovered || recovered.Safety != "custody_returned" {
		t.Fatalf("recover = %s/%s: %s", recovered.State, recovered.Safety, recovered.Error)
	}
	if head := gitCmd(t, f.caller, "rev-parse", "HEAD"); head != f.submitted {
		t.Fatalf("caller moved to %s, want it left at the unmoved head %s", head, f.submitted)
	}
	source := custody.RecoverySourceRef(f.run.ID)
	if parent := gitCmd(t, f.gate, "rev-parse", source+"^"); parent != f.submitted {
		t.Fatalf("preserved source parent = %s, want %s", parent, f.submitted)
	}
	if edit := gitCmd(t, f.gate, "show", source+":feature.txt"); edit != "partial repair" {
		t.Fatalf("preserved edit = %q", edit)
	}
	recorded, err := f.database.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.HeadSHA != f.submitted || recorded.TerminalHeadVerifiedAt == nil || recorded.CustodyReturnedAt == nil {
		t.Fatalf("run = head %s verified %v returned %v, want custody returned at %s", recorded.HeadSHA, recorded.TerminalHeadVerifiedAt, recorded.CustodyReturnedAt, f.submitted)
	}
}

func TestUnmovedRunFailingAtReviewWithDirtySourceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, f *dirtyReviewFailure)
	}{
		{name: "dirty source discarded", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.managed, "checkout", "--", "feature.txt")
		}},
		{name: "managed worktree removed", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.gate, "worktree", "remove", "--force", f.managed)
		}},
		{name: "managed worktree moved off the recorded head", mutate: func(t *testing.T, f *dirtyReviewFailure) {
			gitCmd(t, f.managed, "commit", "-am", "operator commit")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := runDirtyReviewFailure(t, false)
			test.mutate(t, f)
			if state := f.service.InspectCached(context.Background()); state.NextAction != nil && state.NextAction.Command == "no-mistakes axi sync --recover" {
				t.Fatalf("status = %s/%s offers a recovery that cannot preserve the dirty source", state.State, state.Safety)
			}

			assertDirtyRecoveryRefused(t, f)
		})
	}
}

// assertDirtyRecoveryRefused runs recovery and requires it to refuse without
// moving the caller, writing a recovery ref or stamping the run.
func assertDirtyRecoveryRefused(t *testing.T, f *dirtyReviewFailure) {
	t.Helper()
	gateRefs := gitCmd(t, f.gate, "for-each-ref", "refs/no-mistakes")

	refused := f.service.Recover(context.Background(), false)

	if refused.Recovered || refused.Safety != "blocked_recover_unverified_head" {
		t.Fatalf("recover = %s/%s: %s, want blocked_recover_unverified_head", refused.State, refused.Safety, refused.Error)
	}
	if head := gitCmd(t, f.caller, "rev-parse", "HEAD"); head != f.submitted {
		t.Fatalf("caller moved to %s", head)
	}
	if refs := gitCmd(t, f.caller, "for-each-ref", "refs/no-mistakes"); refs != "" {
		t.Fatalf("caller refs changed: %s", refs)
	}
	if refs := gitCmd(t, f.gate, "for-each-ref", "refs/no-mistakes"); refs != gateRefs {
		t.Fatalf("gate refs changed:\n%s\nwant:\n%s", refs, gateRefs)
	}
	recorded, err := f.database.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.TerminalHeadVerifiedAt != nil || recorded.CustodyReturnedAt != nil {
		t.Fatal("a refused recovery stamped the run")
	}
}
