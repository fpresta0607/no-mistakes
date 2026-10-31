package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIRevalidationStartupPreservesUnreadableCandidateBeforeStaleCleanup(t *testing.T) {
	locations := paths.WithRoot(t.TempDir())
	if err := locations.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(locations.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/retained.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "fix/retained", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"red","severity":"error","action":"ask-user","description":"still red"}]}`
	if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusFixReview, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, locations, nil)
	plans, preserved, err := manager.recoverableParkedRuns(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("recovery plans = %v, %v", plans, err)
	}
	if _, ok := preserved[run.ID]; !ok {
		t.Fatal("unreadable candidate never entered the startup preserved set")
	}
	if _, err := database.RecoverStaleRunsExcept("crashed", preserved); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning || stored.HeadSHA != run.HeadSHA {
		t.Fatalf("source ownership changed: %+v, %v", stored, err)
	}
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
		if err := manager.HandleRespond(run.ID, types.StepCI, action, nil); err == nil {
			t.Fatalf("%s consumed hold without executor", action)
		}
	}
	if err := manager.refuseCIRepairReplacement(context.Background(), repo.ID, run.Branch); err == nil {
		t.Fatal("replacement consumed the hold")
	}
}

func TestCIRevalidationStartupPreservesTheReceiptUntilPushCompletes(t *testing.T) {
	locations := paths.WithRoot(t.TempDir())
	if err := locations.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(locations.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/retained.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "fix/retained", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		database.UpdateRunStatus(run.ID, types.RunRunning),
		database.UpdateRunPRURL(run.ID, "https://github.com/example/retained/pull/1"),
		database.UpdateRunPRState(run.ID, "open"),
		database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	findings := `{"findings":[{"id":"red","severity":"error","action":"ask-user","description":"still red"}]}`
	for _, name := range types.AllSteps() {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
			t.Fatal(err)
		}
		round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		if name == types.StepCI {
			selected := `["red"]`
			if err := database.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceAutoFix, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(step.ID, 2, "auto_fix", &findings, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusFixReview, 0, 1, &findings); err != nil {
				t.Fatal(err)
			}
		}
	}
	snapshot, err := pipeline.ReadCIRepairSnapshot(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.CandidateSHA, snapshot.CandidateTreeSHA = strings.Repeat("c", 40), strings.Repeat("d", 40)
	snapshot.CallerDir, snapshot.ManagedDir, snapshot.GateDir = repo.WorkingPath, t.TempDir(), locations.RepoDir(repo.ID)
	snapshot.CallerHeadSHA, snapshot.GateBranch = run.HeadSHA, run.Branch
	snapshot.Operator, snapshot.Reason = "authenticated fixture process", "fresh validation of retained contents"
	if _, _, err := database.AdoptCIRepairForRevalidation(*snapshot); err != nil {
		t.Fatal(err)
	}
	var pushID string
	for _, original := range snapshot.Steps {
		step := original.Snapshot
		if step.StepName.Order() < types.StepPush.Order() {
			if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		} else if step.StepName == types.StepPush {
			pushID = step.ID
			if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: snapshot.CandidateSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, locations, nil)
	for _, isPushActive := range []bool{true, false} {
		if err := database.SetRunPushActive(run.ID, isPushActive); err != nil {
			t.Fatal(err)
		}

		plans, preserved, err := manager.recoverableParkedRuns(t.Context())

		if err != nil || len(plans) != 0 {
			t.Fatalf("receipt recovery plans = %v, %v", plans, err)
		}
		if _, ok := preserved[run.ID]; !ok {
			t.Fatalf("post-receipt Push with push_active=%v was not preserved", isPushActive)
		}
		if _, err := database.RecoverStaleRunsExcept("crashed", preserved); err != nil {
			t.Fatal(err)
		}
		stored, err := database.GetRun(run.ID)
		if err != nil || stored.Status != types.RunRunning || stored.HeadSHA != snapshot.CandidateSHA || *stored.PushGeneration != *snapshot.ExpectedRun.PushGeneration+1 {
			t.Fatalf("startup lost the existing receipt: %+v, %v", stored, err)
		}
		if err := manager.refuseCIRepairReplacement(t.Context(), repo.ID, run.Branch); err == nil {
			t.Fatal("replacement consumed an interrupted delivery")
		}
	}
	if err := database.CompleteStep(pushID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := manager.ciRepairHeld(t.Context(), stored); err != nil || held {
		t.Fatalf("completed delivery retained the temporary hold: %v, %v", held, err)
	}
}
