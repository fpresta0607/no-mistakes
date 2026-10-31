package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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
