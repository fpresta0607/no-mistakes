package pipeline

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A repair round's rereview keeps the previous round's verdict on the files
// the fix left alone, so it must receive that round's coverage record on
// every way into a fix round. The live auto-fix entry is driven end to end in
// steps (TestReviewLoop_RepairRoundReviewsOnlyWhatTheFixChanged); these drive
// the operator's fix response, live and after a daemon restart, which build
// the fix round's state separately.

const previousCoverageFinding = `{"findings":[{"id":"f-1","severity":"warning","file":"a.go","line":1,"description":"needs a decision","action":"ask-user"}],"summary":"1 issue","reviewed_paths":["a.go","b.go"]}`

var previousCoverage = []string{"a.go", "b.go"}

// fixRoundInputs is what a fix round was handed to judge its rereview by.
type fixRoundInputs struct {
	coverage []string
	pending  []string
}

// coverageRecordingStep parks its first round on one ask-user finding with a
// full coverage record, and on a fix round sends the inputs it received and
// verifies the finding so the run completes.
func coverageRecordingStep(received chan<- fixRoundInputs) *adaptiveCallStep {
	return &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if !sctx.Fixing {
				return &StepOutcome{NeedsApproval: true, Findings: previousCoverageFinding, ReviewedPaths: previousCoverage, ReviewablePaths: previousCoverage}, nil
			}
			received <- fixRoundInputs{coverage: sctx.PreviousReviewedPaths, pending: sctx.PendingVerificationIDs}
			return &StepOutcome{ReviewedPaths: previousCoverage, ReviewablePaths: previousCoverage}, nil
		},
	}
}

func respondFixWhenParked(t *testing.T, exec *Executor) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for exec.Respond(types.StepReview, types.ActionFix, []string{"f-1"}) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the review gate never accepted a fix response")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func expectPreviousCoverage(t *testing.T, received <-chan fixRoundInputs, done <-chan error) {
	t.Helper()
	select {
	case got := <-received:
		if !reflect.DeepEqual(got.coverage, previousCoverage) {
			t.Fatalf("fix round received PreviousReviewedPaths %v, want the parked round's %v", got.coverage, previousCoverage)
		}
		if !reflect.DeepEqual(got.pending, []string{"f-1"}) {
			t.Fatalf("fix round received PendingVerificationIDs %v, want the selection it can clear", got.pending)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fix round never ran")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not complete after the fix round verified its finding")
	}
}

func TestExecutor_FixResponseHandsTheFixRoundThePreviousCoverage(t *testing.T) {
	database, p, run, repo := setupTest(t)
	received := make(chan fixRoundInputs, 1)
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{coverageRecordingStep(received)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	respondFixWhenParked(t, exec)

	expectPreviousCoverage(t, received, done)
}

func TestExecutor_RecoveredFixResponseHandsTheFixRoundThePreviousCoverage(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := previousCoverageFinding
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan fixRoundInputs, 1)
	exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{coverageRecordingStep(received)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()
	respondFixWhenParked(t, exec)

	expectPreviousCoverage(t, received, done)
}
