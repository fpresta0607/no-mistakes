package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runStepGit(sctx *StepContext, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = sctx.WorkDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

// recordWorktreeHead records the worktree's head as the run head, the way the
// real Rebase and Review steps do after they move it.
func recordWorktreeHead(sctx *StepContext) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = sctx.WorkDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(out))
	sctx.Run.HeadSHA = head
	return head, sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head)
}

// rebaseOntoNewerBaseStep moves the run's head onto a newer base, so the head
// it records is a rewrite of the submitted head, never its descendant.
type rebaseOntoNewerBaseStep struct{ newBase string }

func (*rebaseOntoNewerBaseStep) Name() types.StepName { return types.StepRebase }

func (s *rebaseOntoNewerBaseStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := runStepGit(sctx, "rebase", "--onto", s.newBase, sctx.Run.BaseSHA); err != nil {
		return nil, err
	}
	_, err := recordWorktreeHead(sctx)
	return &StepOutcome{}, err
}

// reviewFixCommitStep commits a Review fix on the rebased head and certifies
// the result, as a completed Review that repaired a finding does.
type reviewFixCommitStep struct{}

func (*reviewFixCommitStep) Name() types.StepName { return types.StepReview }

func (*reviewFixCommitStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "review.txt"), []byte("review repair\n"), 0o644); err != nil {
		return nil, err
	}
	if err := runStepGit(sctx, "add", "review.txt"); err != nil {
		return nil, err
	}
	if err := runStepGit(sctx, "commit", "-m", "no-mistakes(review): repair the finding"); err != nil {
		return nil, err
	}
	head, err := recordWorktreeHead(sctx)
	if err != nil {
		return nil, err
	}
	return &StepOutcome{ReviewApprovedHeadSHA: head}, nil
}

type unobservedTerminalHead struct {
	gate, caller, managed string
	submitted, reviewed   string
	database              *db.DB
	run                   *db.Run
	service               *branchsync.Service
}

// runUnobservedTerminalHead drives issue #364's run: Rebase moves the head
// onto a newer main, Review commits a fix on it, and Test fails on
// infrastructure before any test runs. The host is loaded, so terminalization
// cannot even read the managed worktree's head within its budget: it neither
// anchors nor verifies the head nor records the source state, and the managed
// worktree is retained exactly at the reviewed head.
func runUnobservedTerminalHead(t *testing.T) *unobservedTerminalHead {
	t.Helper()
	database, appPaths, _, _ := setupTest(t)
	gate := filepath.Join(t.TempDir(), "gate")
	if err := os.MkdirAll(gate, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, gate)
	execGit(t, gate, "branch", "-M", "main")
	base := gitOutput(t, gate, "rev-parse", "HEAD")
	execGit(t, gate, "checkout", "-b", "feature")
	writeTestFile(t, gate, "feature.txt", "operator work\n")
	execGit(t, gate, "add", "feature.txt")
	execGit(t, gate, "commit", "-m", "operator work")
	submitted := gitOutput(t, gate, "rev-parse", "HEAD")
	execGit(t, gate, "checkout", "main")
	writeTestFile(t, gate, "main.txt", "newer main\n")
	execGit(t, gate, "add", "main.txt")
	execGit(t, gate, "commit", "-m", "newer main")
	newBase := gitOutput(t, gate, "rev-parse", "HEAD")
	caller := filepath.Join(t.TempDir(), "caller")
	execGit(t, filepath.Dir(caller), "clone", "--branch", "feature", gate, caller)
	managed := filepath.Join(t.TempDir(), "managed")
	execGit(t, gate, "worktree", "add", "--detach", managed, submitted)

	repo, err := database.InsertRepo(caller, gate, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunWorktreeDir(run.ID, managed); err != nil {
		t.Fatal(err)
	}
	infrastructure := errors.New("prepare test dependencies: clean preparation changes: reset --hard submodule vendor: context deadline exceeded (exit status 1)")
	executor := NewExecutor(database, appPaths, &config.Config{}, nil, []Step{
		&rebaseOntoNewerBaseStep{newBase: newBase}, &reviewFixCommitStep{}, newFailStep(types.StepTest, infrastructure),
	}, nil)
	previous := terminalHeadObservationTimeout
	terminalHeadObservationTimeout = time.Nanosecond
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	err = executor.Execute(ctx, run, repo, managed)
	cancel()
	terminalHeadObservationTimeout = previous
	if err == nil {
		t.Fatal("the run whose Test failed on infrastructure did not fail")
	}

	reviewed := gitOutput(t, managed, "rev-parse", "HEAD")
	isAncestor := exec.Command("git", "merge-base", "--is-ancestor", submitted, reviewed)
	isAncestor.Dir = gate
	if reviewed == submitted || isAncestor.Run() == nil {
		t.Fatal("the fixture's Rebase did not rewrite the submitted head")
	}
	recorded, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Status != types.RunFailed || recorded.HeadSHA != reviewed || recorded.ReviewApprovedHeadSHA == nil || *recorded.ReviewApprovedHeadSHA != reviewed ||
		recorded.TerminalHeadVerifiedAt != nil || recorded.TerminalSourceState != nil {
		t.Fatalf("terminal record = status %s head %s verified %v source %v, want failed at the unobserved reviewed head %s", recorded.Status, recorded.HeadSHA, recorded.TerminalHeadVerifiedAt, recorded.TerminalSourceState, reviewed)
	}
	if refs := gitOutput(t, gate, "for-each-ref", "refs/no-mistakes"); refs != "" {
		t.Fatalf("terminalization anchored refs it could not observe: %s", refs)
	}
	if gateBranch := gitOutput(t, gate, "rev-parse", "refs/heads/feature"); gateBranch != submitted {
		t.Fatalf("gate branch = %s, want the submitted head %s", gateBranch, submitted)
	}
	return &unobservedTerminalHead{
		gate: gate, caller: caller, managed: managed, submitted: submitted, reviewed: reviewed,
		database: database, run: recorded,
		service: &branchsync.Service{DB: database, Paths: appPaths, Repo: repo, WorkDir: caller, GateDir: filepath.Join(gate, ".git")},
	}
}

// TestUnobservedTerminalHeadRecoversByTheOfferedPath pins issue #364: status
// offered sync --recover for this run, and every recovery refused it for good
// because terminalization never verified its rebased, reviewed head.
func TestUnobservedTerminalHeadRecoversByTheOfferedPath(t *testing.T) {
	for _, test := range []struct {
		name string
		// observe stands in for what the daemon later recorded about the
		// retained managed worktree, if anything.
		observe func(t *testing.T, f *unobservedTerminalHead)
		// position moves the caller before recovery.
		position func(t *testing.T, f *unobservedTerminalHead)
	}{
		{name: "caller at the submitted head"},
		{name: "caller at the exact reviewed head", position: func(t *testing.T, f *unobservedTerminalHead) {
			execGit(t, f.caller, "fetch", f.gate, f.reviewed)
			execGit(t, f.caller, "reset", "--hard", f.reviewed)
		}},
		{name: "retention later recorded the clean source", observe: func(t *testing.T, f *unobservedTerminalHead) {
			if err := f.database.RecordTerminalSourceState(f.run.ID, "clean:"+f.reviewed); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "terminalization could not read the source", observe: func(t *testing.T, f *unobservedTerminalHead) {
			if err := f.database.RecordTerminalSourceState(f.run.ID, "unreadable"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runUnobservedTerminalHead(t)
			if test.observe != nil {
				test.observe(t, f)
			}
			if test.position != nil {
				test.position(t, f)
			}
			ctx := context.Background()
			if state := f.service.InspectCached(ctx); state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
				t.Fatalf("status = %s/%s %+v, want the supported recovery offered", state.State, state.Safety, state.NextAction)
			}

			recovered := f.service.Recover(ctx, false)

			if !recovered.Recovered || recovered.Safety != "custody_returned" {
				t.Fatalf("recover = %s/%s: %s", recovered.State, recovered.Safety, recovered.Error)
			}
			if head := gitOutput(t, f.caller, "rev-parse", "HEAD"); head != f.reviewed {
				t.Fatalf("caller HEAD = %s, want the reviewed head %s", head, f.reviewed)
			}
			if branch := gitOutput(t, f.caller, "rev-parse", "refs/heads/feature"); branch != f.reviewed {
				t.Fatalf("caller branch = %s, want the reviewed head %s", branch, f.reviewed)
			}
			if status := gitOutput(t, f.caller, "status", "--porcelain"); status != "" {
				t.Fatalf("recovery left the caller worktree dirty: %s", status)
			}
			if anchored := gitOutput(t, f.gate, "rev-parse", custody.RecoveryRef(f.run.ID)); anchored != f.reviewed {
				t.Fatalf("gate recovery ref = %s, want %s", anchored, f.reviewed)
			}
			recorded, err := f.database.GetRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.HeadSHA != f.reviewed || recorded.TerminalHeadVerifiedAt == nil || recorded.CustodyReturnedAt == nil ||
				recorded.TerminalSourceState == nil || *recorded.TerminalSourceState != "clean:"+f.reviewed {
				t.Fatalf("run = head %s verified %v returned %v source %v, want custody returned at the observed clean head %s", recorded.HeadSHA, recorded.TerminalHeadVerifiedAt, recorded.CustodyReturnedAt, recorded.TerminalSourceState, f.reviewed)
			}
		})
	}
}

// TestUnobservedTerminalHeadFailsClosedWithoutOfferingRecovery keeps every
// other unobserved head refused, and status never offers the recovery that
// would refuse it.
func TestUnobservedTerminalHeadFailsClosedWithoutOfferingRecovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, f *unobservedTerminalHead)
	}{
		{name: "managed worktree removed", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			execGit(t, f.gate, "worktree", "remove", "--force", f.managed)
		}},
		{name: "managed worktree dirty", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			writeTestFile(t, f.managed, "review.txt", "unfinished edit\n")
		}},
		{name: "managed worktree moved off the recorded head", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			writeTestFile(t, f.managed, "later.txt", "later\n")
			execGit(t, f.managed, "add", "later.txt")
			execGit(t, f.managed, "commit", "-m", "later commit")
		}},
		{name: "source recorded dirty", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			if err := f.database.RecordTerminalSourceState(f.run.ID, "dirty:"+f.reviewed); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "source recorded at another head", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			if err := f.database.RecordTerminalSourceState(f.run.ID, "clean:"+f.submitted); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "recovery ref names another head", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			execGit(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.submitted)
		}},
		{name: "caller dirty at the exact reviewed head", mutate: func(t *testing.T, f *unobservedTerminalHead) {
			execGit(t, f.caller, "fetch", f.gate, f.reviewed)
			execGit(t, f.caller, "reset", "--hard", f.reviewed)
			writeTestFile(t, f.caller, "feature.txt", "uncommitted operator edit\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runUnobservedTerminalHead(t)
			test.mutate(t, f)
			ctx := context.Background()
			gateRefs := gitOutput(t, f.gate, "for-each-ref", "refs/no-mistakes")
			callerHead := gitOutput(t, f.caller, "rev-parse", "HEAD")

			state := f.service.InspectCached(ctx)
			refused := f.service.Recover(ctx, false)

			if state.NextAction != nil && state.NextAction.Code == "recover_custody" {
				t.Fatalf("status offered %q for a head recovery refuses", state.NextAction.Command)
			}
			if refused.Recovered || refused.Safety == "custody_returned" {
				t.Fatalf("recover = %s/%s, want a refusal", refused.State, refused.Safety)
			}
			if head := gitOutput(t, f.caller, "rev-parse", "HEAD"); head != callerHead {
				t.Fatalf("caller moved from %s to %s", callerHead, head)
			}
			if refs := gitOutput(t, f.caller, "for-each-ref", "refs/no-mistakes"); refs != "" {
				t.Fatalf("caller refs changed: %s", refs)
			}
			if refs := gitOutput(t, f.gate, "for-each-ref", "refs/no-mistakes"); refs != gateRefs {
				t.Fatalf("gate refs changed:\n%s\nwant:\n%s", refs, gateRefs)
			}
			recorded, err := f.database.GetRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recorded.TerminalHeadVerifiedAt != nil || recorded.CustodyReturnedAt != nil {
				t.Fatal("a refused recovery stamped the run")
			}
		})
	}
}
