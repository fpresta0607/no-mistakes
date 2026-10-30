package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type handbackReconciler struct {
	calls atomic.Int32
}

func (s *handbackReconciler) Name() types.StepName { return types.StepCI }
func (s *handbackReconciler) Execute(*StepContext) (*StepOutcome, error) {
	return &StepOutcome{}, nil
}
func (s *handbackReconciler) ReconcileApprovalGate(*StepContext) (bool, error) {
	s.calls.Add(1)
	return true, nil
}

func TestExecutorRecoveredCIHandbackPreservesReservation(t *testing.T) {
	for _, isAccepted := range []bool{false, true} {
		name := "released"
		if isAccepted {
			name = "accepted before revalidation"
		}
		t.Run(name, func(t *testing.T) {
			database, paths, run, repo := setupTest(t)
			for _, err := range []error{
				database.UpdateRunStatus(run.ID, types.RunRunning),
				database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"),
				database.UpdateRunPRState(run.ID, "open"),
				database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			step, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"ci-1","category":"ci-merge-conflict","severity":"error","action":"auto-fix","description":"conflict"}],"summary":"conflict"}`
			round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt := &db.CIHandback{RunID: run.ID, RoundID: round.ID, CallerDir: "caller", ReleasedHead: run.HeadSHA, PRURL: *run.PRURL, BaseBranch: "main", BaseHead: "base", PushGeneration: *run.PushGeneration, TargetFingerprint: "target", PushRef: "refs/heads/feature"}
			if err := database.ReleaseCIHandback(receipt, run, repo, findings); err != nil {
				t.Fatal(err)
			}
			if isAccepted {
				if err := database.AcceptCIHandback(receipt, "followup", repo); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateRecoveredRun(database, run, []Step{&handbackReconciler{}}); err == nil {
					t.Fatal("accepted head recovered its old conflict gate without fresh validation")
				}
				return
			}
			stepHandler := &handbackReconciler{}
			executor := NewExecutor(database, paths, &config.Config{}, nil, []Step{stepHandler}, nil)
			executor.SetGateReconcileTimings(time.Millisecond, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			workDir := t.TempDir()
			go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("recovered executor did not stop")
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				executor.mu.Lock()
				isWaiting := executor.waiting
				executor.mu.Unlock()
				if isWaiting || time.Now().After(deadline) {
					if !isWaiting {
						t.Fatal("released gate did not recover its park")
					}
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err := executor.Respond(types.StepCI, types.ActionApprove, nil); err == nil {
				t.Fatal("ordinary approval released caller custody after recovery")
			}
			if stepHandler.calls.Load() != 0 {
				t.Fatal("recovery reconciled a released conflict")
			}
		})
	}
}
