package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const stepHandbackFailure = `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"command failed with exit code 1"}]}`

// stepHandbackFailureWithNote is a contributor failure plus the informational
// note the Test step adds when a program it started opens a window.
const stepHandbackFailureWithNote = `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"command failed with exit code 1"},{"id":"test-window-1","severity":"info","action":"no-op","description":"a program opened a window on the hidden desktop"}]}`

type stepHandbackFixture struct {
	t        *testing.T
	caller   string
	managed  string
	database *db.DB
	run      *db.Run
	repo     *db.Repo
	step     *db.StepResult
	sctx     *pipeline.StepContext
	handler  pipeline.StepHandbackStep
}

// setupStepHandback parks name on a failure with automatic repair disabled,
// the way the executor leaves it, with the contributor's worktree at the
// run's head and the managed worktree sharing the gate's refs.
func setupStepHandback(t *testing.T, name types.StepName, findings string) *stepHandbackFixture {
	t.Helper()
	caller, base, head := setupGitRepo(t)
	gate := filepath.Join(t.TempDir(), "gate.git")
	managed := filepath.Join(t.TempDir(), "managed")
	gitCmd(t, caller, "clone", "--bare", caller, gate)
	gitCmd(t, gate, "worktree", "add", managed, "feature")
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(caller, "https://github.com/test/handback.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	step := parkStepForHandback(t, database, run, name, findings)
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handler pipeline.StepHandbackStep = &TestStep{}
	if name == types.StepLint {
		handler = &LintStep{}
	}
	sctx := &pipeline.StepContext{Ctx: t.Context(), DB: database, Run: run, Repo: repo, WorkDir: managed, GateDir: gate, StepResultID: step.ID, Config: &config.Config{}, Log: func(string) {}}
	return &stepHandbackFixture{t: t, caller: caller, managed: managed, database: database, run: run, repo: repo, step: step, sctx: sctx, handler: handler}
}

func parkStepForHandback(t *testing.T, database *db.DB, run *db.Run, name types.StepName, findings string) *db.StepResult {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	return step
}

func (f *stepHandbackFixture) request(isResume bool, receiptID string) ipc.StepHandbackParams {
	return ipc.StepHandbackParams{RepoID: f.repo.ID, RunID: f.run.ID, Branch: f.run.Branch, CallerDir: f.caller, HeadSHA: gitCmd(f.t, f.caller, "rev-parse", "HEAD"), Resume: isResume, ReceiptID: receiptID}
}

func (f *stepHandbackFixture) release() *ipc.StepHandbackResult {
	f.t.Helper()
	result, err := f.handler.HandbackStep(f.sctx, f.request(false, ""))
	if err != nil {
		f.t.Fatalf("release: %v", err)
	}
	return result
}

func commitCorrection(t *testing.T, dir, file string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", file)
	gitCmd(t, dir, "commit", "-m", "correct "+file)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

// assertHandbackUnmoved checks that a refused resume left the run, the managed
// worktree and the receipt exactly as the release left them.
func (f *stepHandbackFixture) assertHandbackUnmoved(released *ipc.StepHandbackResult) {
	f.t.Helper()
	if head := gitCmd(f.t, f.managed, "rev-parse", "HEAD"); head != released.ReleasedHead {
		f.t.Fatalf("managed head = %s, want released %s", head, released.ReleasedHead)
	}
	run, err := f.database.GetRun(f.run.ID)
	if err != nil || run.HeadSHA != released.ReleasedHead {
		f.t.Fatalf("run head = %+v, %v; want released %s", run, err, released.ReleasedHead)
	}
	receipt, err := f.database.GetStepHandback(released.ReceiptID)
	if err != nil || receipt == nil || receipt.AcceptedHead != nil {
		f.t.Fatalf("receipt after refusal = %+v, %v; want it still open", receipt, err)
	}
}

func TestStepHandbackAcceptsOneContributorCommitOnTheParkedHead(t *testing.T) {
	for _, test := range []struct {
		name     string
		findings string
	}{
		{"failure", stepHandbackFailure},
		{"failure with an informational note", stepHandbackFailureWithNote},
	} {
		for _, name := range []types.StepName{types.StepTest, types.StepLint} {
			t.Run(test.name+"/"+string(name), func(t *testing.T) {
				// Arrange
				fixture := setupStepHandback(t, name, test.findings)
				released := fixture.release()
				if released.ReceiptID == "" || released.ReleasedHead != fixture.run.HeadSHA || released.Step != name {
					t.Fatalf("release result = %+v", released)
				}
				if anchored := gitCmd(t, fixture.caller, "rev-parse", released.ReleasedRef); anchored != released.ReleasedHead {
					t.Fatalf("released ref %s = %s, want %s", released.ReleasedRef, anchored, released.ReleasedHead)
				}
				if replay := fixture.release(); replay.ReceiptID != released.ReceiptID {
					t.Fatalf("a second release of the same round issued receipt %s, want %s", replay.ReceiptID, released.ReceiptID)
				}
				correction := commitCorrection(t, fixture.caller, "correction.txt")

				// Act
				accepted, err := fixture.handler.HandbackStep(fixture.sctx, fixture.request(true, released.ReceiptID))

				// Assert
				if err != nil {
					t.Fatalf("resume: %v", err)
				}
				if accepted.AcceptedHead != correction || accepted.ReceiptID != released.ReceiptID {
					t.Fatalf("resume result = %+v, want accepted %s", accepted, correction)
				}
				if head := gitCmd(t, fixture.managed, "rev-parse", "HEAD"); head != correction {
					t.Fatalf("managed head = %s, want the correction %s", head, correction)
				}
				run, err := fixture.database.GetRun(fixture.run.ID)
				if err != nil || run.HeadSHA != correction || fixture.sctx.Run.HeadSHA != correction {
					t.Fatalf("run head = %+v, %v; in memory %s; want %s", run, err, fixture.sctx.Run.HeadSHA, correction)
				}
				receipt, err := fixture.database.GetStepHandback(released.ReceiptID)
				if err != nil || receipt.AcceptedHead == nil || *receipt.AcceptedHead != correction {
					t.Fatalf("receipt = %+v, %v", receipt, err)
				}
			})
		}
	}
}

func TestStepHandbackRefusesACorrectionThatIsNotOneCommitOnTheRunsHead(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, caller, released string)
	}{
		{"no commit", func(*testing.T, string, string) {}},
		{"two commits", func(t *testing.T, caller, _ string) {
			commitCorrection(t, caller, "first.txt")
			commitCorrection(t, caller, "second.txt")
		}},
		{"rewritten head", func(t *testing.T, caller, _ string) {
			gitCmd(t, caller, "commit", "--amend", "-m", "rewritten feature")
		}},
		{"merge commit", func(t *testing.T, caller, released string) {
			gitCmd(t, caller, "checkout", "-b", "side", released)
			commitCorrection(t, caller, "side.txt")
			gitCmd(t, caller, "checkout", "feature")
			gitCmd(t, caller, "merge", "--no-ff", "-m", "merge side", "side")
		}},
	} {
		for _, name := range []types.StepName{types.StepTest, types.StepLint} {
			t.Run(test.name+"/"+string(name), func(t *testing.T) {
				// Arrange
				fixture := setupStepHandback(t, name, stepHandbackFailure)
				released := fixture.release()
				test.prepare(t, fixture.caller, released.ReleasedHead)

				// Act
				_, err := fixture.handler.HandbackStep(fixture.sctx, fixture.request(true, released.ReceiptID))

				// Assert
				if err == nil || !strings.Contains(err.Error(), "exactly one commit on the released head") {
					t.Fatalf("resume = %v, want a one-commit refusal", err)
				}
				fixture.assertHandbackUnmoved(released)
			})
		}
	}
}

func TestStepHandbackRefusesAReceiptFromAnotherRun(t *testing.T) {
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		t.Run(string(name), func(t *testing.T) {
			// Arrange
			fixture := setupStepHandback(t, name, stepHandbackFailure)
			released := fixture.release()
			other, err := fixture.database.InsertRun(fixture.repo.ID, "other", fixture.run.HeadSHA, fixture.run.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			otherStep := parkStepForHandback(t, fixture.database, other, name, stepHandbackFailure)
			otherRounds, err := fixture.database.GetRoundsByStep(otherStep.ID)
			if err != nil {
				t.Fatal(err)
			}
			foreign := &db.StepHandback{RunID: other.ID, StepResultID: otherStep.ID, StepName: name, RoundID: otherRounds[0].ID, CallerDir: released.CallerDir, ReleasedHead: other.HeadSHA}
			if err := fixture.database.ReleaseStepHandback(foreign); err != nil {
				t.Fatal(err)
			}
			commitCorrection(t, fixture.caller, "correction.txt")

			// Act
			_, err = fixture.handler.HandbackStep(fixture.sctx, fixture.request(true, foreign.ID))

			// Assert
			if err == nil || !strings.Contains(err.Error(), "does not belong to run "+fixture.run.ID) {
				t.Fatalf("resume with another run's receipt = %v", err)
			}
			fixture.assertHandbackUnmoved(released)
		})
	}
}

func TestStepHandbackLeavesOperatorDecisionsParked(t *testing.T) {
	for _, test := range []struct {
		name     string
		findings string
		autoFix  int
	}{
		{"automatic repair enabled", stepHandbackFailure, 1},
		{"ask-user finding", `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"failed"},{"id":"verdict-1","severity":"warning","action":"ask-user","description":"inconclusive"}]}`, 0},
		{"safety refusal", `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"failed"},{"id":"agent-safety-refusal","severity":"error","action":"ask-user","description":"the agent refused"}]}`, 0},
		{"protected-path refusal", `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"failed"},{"id":"protected-path-refusal","severity":"error","action":"ask-user","description":"the repair touched a protected path"}]}`, 0},
		{"only an informational note", `{"findings":[{"id":"test-window-1","severity":"info","action":"no-op","description":"a program opened a window on the hidden desktop"}]}`, 0},
	} {
		for _, name := range []types.StepName{types.StepTest, types.StepLint} {
			t.Run(test.name+"/"+string(name), func(t *testing.T) {
				// Arrange
				fixture := setupStepHandback(t, name, test.findings)
				fixture.sctx.Config.AutoFix.Test, fixture.sctx.Config.AutoFix.Lint = test.autoFix, test.autoFix

				// Act
				_, err := fixture.handler.HandbackStep(fixture.sctx, fixture.request(false, ""))

				// Assert
				if err == nil {
					t.Fatal("handback released a gate an operator must decide")
				}
				rounds, roundErr := fixture.database.GetRoundsByStep(fixture.step.ID)
				if roundErr != nil {
					t.Fatal(roundErr)
				}
				if receipt, receiptErr := fixture.database.GetStepHandbackByRound(rounds[0].ID); receiptErr != nil || receipt != nil {
					t.Fatalf("refused release recorded receipt %+v, %v", receipt, receiptErr)
				}
			})
		}
	}
}

func TestStepHandbackRefusesAResumeFromAnotherWorktree(t *testing.T) {
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		t.Run(string(name), func(t *testing.T) {
			// Arrange
			fixture := setupStepHandback(t, name, stepHandbackFailure)
			released := fixture.release()
			other := filepath.Join(t.TempDir(), "other")
			gitCmd(t, fixture.caller, "clone", "--branch", "feature", fixture.caller, other)
			correction := commitCorrection(t, other, "correction.txt")
			request := fixture.request(true, released.ReceiptID)
			request.CallerDir, request.HeadSHA = other, correction

			// Act
			_, err := fixture.handler.HandbackStep(fixture.sctx, request)

			// Assert
			if err == nil || !strings.Contains(err.Error(), "resume from the worktree the failure was released to") {
				t.Fatalf("resume from another worktree = %v, want a recorded-worktree refusal", err)
			}
			fixture.assertHandbackUnmoved(released)
		})
	}
}
