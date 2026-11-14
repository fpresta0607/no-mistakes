package pipeline

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const handbackGateFailure = `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"command failed"}]}`

// handbackGateStep fails its first execution, accepts a handback by moving
// the run to its correction, and passes only when re-executed on that head.
type handbackGateStep struct {
	name       types.StepName
	correction string
	executions atomic.Int32
}

func (s *handbackGateStep) Name() types.StepName { return s.name }

func (s *handbackGateStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if s.executions.Add(1) == 1 && sctx.Run.HeadSHA != s.correction {
		return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: handbackGateFailure}, nil
	}
	if sctx.Run.HeadSHA != s.correction || sctx.Fixing {
		return nil, fmt.Errorf("re-executed on %s with fixing %v, want a fresh validation of %s", sctx.Run.HeadSHA, sctx.Fixing, s.correction)
	}
	return &StepOutcome{}, nil
}

func (s *handbackGateStep) HandbackStep(sctx *StepContext, request ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	if !request.Resume {
		return &ipc.StepHandbackResult{RunID: sctx.Run.ID, Step: s.name, ReceiptID: "receipt"}, nil
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, s.correction); err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = s.correction
	return &ipc.StepHandbackResult{RunID: sctx.Run.ID, Step: s.name, ReceiptID: "receipt", AcceptedHead: s.correction}, nil
}

func waitForHandbackGate(t *testing.T, executor *Executor) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !executor.AwaitingApproval() {
		if time.Now().After(deadline) {
			t.Fatal("step never parked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// handBackAndResume releases the parked gate, checks it stays parked, then
// submits the correction.
func handBackAndResume(t *testing.T, executor *Executor, run *db.Run, repo *db.Repo) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request := ipc.StepHandbackParams{RepoID: repo.ID, RunID: run.ID, Branch: run.Branch}
	if _, err := executor.HandbackStep(ctx, request); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !executor.AwaitingApproval() {
		t.Fatal("a released gate stopped waiting before its correction arrived")
	}
	request.Resume, request.ReceiptID = true, "receipt"
	if _, err := executor.HandbackStep(ctx, request); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func assertHandbackRevalidatedInTheSameRun(t *testing.T, database *db.DB, run *db.Run, gate *handbackGateStep, next *mockStep, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after handback: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after handback")
	}
	if gate.executions.Load() != 2 || next.callCount() != 1 {
		t.Fatalf("%s ran %d times and the next step %d times, want 2 and 1", gate.name, gate.executions.Load(), next.callCount())
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[0].Trigger != "initial" || rounds[1].Trigger != "handback" || rounds[1].IsFixRound() {
		t.Fatalf("rounds = %+v, want the parked initial round kept and one handback round", rounds)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunCompleted || stored.HeadSHA != gate.correction {
		t.Fatalf("run = %+v, %v; want the same run completed on the correction", stored, err)
	}
}

func TestExecutorHandbackRevalidatesTheParkedStepInTheSameRun(t *testing.T) {
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		t.Run(string(name), func(t *testing.T) {
			// Arrange
			database, paths, run, repo := setupTest(t)
			gate := &handbackGateStep{name: name, correction: "correction"}
			next := newPassStep(types.StepPush)
			executor := NewExecutor(database, paths, &config.Config{}, nil, []Step{gate, next}, nil)
			done := make(chan error, 1)
			go func() { done <- executor.Execute(context.Background(), run, repo, t.TempDir()) }()
			waitForHandbackGate(t, executor)

			// Act
			handBackAndResume(t, executor, run, repo)

			// Assert
			assertHandbackRevalidatedInTheSameRun(t, database, run, gate, next, done)
		})
	}
}

// parkRecoveredHandbackGate records name parked on its failed initial round,
// ahead of a pending Push, as a restarted daemon finds it.
func parkRecoveredHandbackGate(t *testing.T, database *db.DB, run *db.Run, name types.StepName) *db.StepHandback {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	parked, err := database.InsertStepResult(run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepPush); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(parked.ID, 0); err != nil {
		t.Fatal(err)
	}
	findings := handbackGateFailure
	round, err := database.InsertStepRound(parked.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, parked.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	return &db.StepHandback{RunID: run.ID, StepResultID: parked.ID, StepName: name, RoundID: round.ID, CallerDir: "caller", ReleasedHead: run.HeadSHA}
}

func TestExecutorRecoveredHandbackRevalidatesTheParkedStepInTheSameRun(t *testing.T) {
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		t.Run(string(name), func(t *testing.T) {
			// Arrange
			database, paths, run, repo := setupTest(t)
			parkRecoveredHandbackGate(t, database, run, name)
			run, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			gate := &handbackGateStep{name: name, correction: "correction"}
			gate.executions.Store(1)
			next := newPassStep(types.StepPush)
			executor := NewExecutor(database, paths, &config.Config{}, nil, []Step{gate, next}, nil)
			done := make(chan error, 1)
			go func() { done <- executor.Resume(context.Background(), run, repo, t.TempDir()) }()
			waitForHandbackGate(t, executor)

			// Act
			handBackAndResume(t, executor, run, repo)

			// Assert
			assertHandbackRevalidatedInTheSameRun(t, database, run, gate, next, done)
		})
	}
}

// A daemon that stopped after accepting a correction but before validating it
// must not leave the gate parked, where an approval would publish that
// unvalidated head.
func TestExecutorRecoveryValidatesAnAcceptedCorrectionBeforeItCanBeApproved(t *testing.T) {
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		t.Run(string(name), func(t *testing.T) {
			// Arrange
			database, paths, run, repo := setupTest(t)
			receipt := parkRecoveredHandbackGate(t, database, run, name)
			if err := database.ReleaseStepHandback(receipt); err != nil {
				t.Fatal(err)
			}
			gate := &handbackGateStep{name: name, correction: "correction"}
			if err := database.AcceptStepHandback(receipt, gate.correction); err != nil {
				t.Fatal(err)
			}
			run, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			gate.executions.Store(1)
			next := newPassStep(types.StepPush)
			executor := NewExecutor(database, paths, &config.Config{}, nil, []Step{gate, next}, nil)

			// Act
			done := make(chan error, 1)
			go func() { done <- executor.Resume(context.Background(), run, repo, t.TempDir()) }()

			// Assert
			assertHandbackRevalidatedInTheSameRun(t, database, run, gate, next, done)
		})
	}
}

func TestExecutorHandbackRefusesAGateWithAutomaticRepair(t *testing.T) {
	// Arrange
	database, paths, run, repo := setupTest(t)
	gate := &handbackGateStep{name: types.StepTest, correction: "correction"}
	executor := NewExecutor(database, paths, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{gate}, nil)
	executor.waiting, executor.waitingStep = true, types.StepTest
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := executor.waitForApprovalOrReconcile(ctx, gate, &StepContext{Run: run, DB: database}, handbackGateFailure, false)
		done <- err
	}()

	// Act
	handbackContext, cancelHandback := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelHandback()
	_, err := executor.HandbackStep(handbackContext, ipc.StepHandbackParams{RepoID: repo.ID, RunID: run.ID, Branch: run.Branch, Resume: true, ReceiptID: "receipt"})

	// Assert
	if err == nil {
		t.Fatal("handback released a gate whose own repair agent is enabled")
	}
	if !executor.AwaitingApproval() {
		t.Fatal("a refused handback released the gate")
	}
	cancel()
	<-done
}
