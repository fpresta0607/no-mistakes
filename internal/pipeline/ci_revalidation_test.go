package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIRepairSnapshotBindsOriginalRoundsAndEffectiveLimits(t *testing.T) {
	database, _, run, _ := setupTest(t)
	for _, name := range []types.StepName{types.StepReview, types.StepCI} {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepCI {
			if err := database.SetStepAutoFixLimit(step.ID, 1); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := database.InsertStepRound(step.ID, 1, "initial", nil, nil, 1); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := ReadCIRepairSnapshot(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := CIRepairSnapshotDigest(snapshot)
	if err != nil || len(digest) != 64 || len(snapshot.Steps) != 2 || snapshot.Steps[0].EffectiveAutoFixLimit != 0 || snapshot.Steps[1].EffectiveAutoFixLimit != 1 {
		t.Fatalf("incomplete original snapshot: %+v, %v", snapshot, err)
	}
	for _, original := range snapshot.Steps {
		rounds, err := database.GetRoundsByStep(original.Snapshot.ID)
		if err != nil || len(original.Rounds) != 1 || !reflect.DeepEqual(original.Rounds[0], *rounds[0]) {
			t.Fatalf("round snapshot differs: %+v, %v", original, err)
		}
	}
	if _, err := database.InsertStepRound(snapshot.Steps[1].Snapshot.ID, 2, "auto_fix", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	changed, err := ReadCIRepairSnapshot(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, err := CIRepairSnapshotDigest(changed)
	if err != nil || changedDigest == digest {
		t.Fatal("new round did not invalidate the original binding")
	}
}

func TestCIPublicationSelectionEndsAtItsDurablePublication(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	binding := db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}
	if err := database.UpdateRunPublication(run.ID, binding); err != nil {
		t.Fatal(err)
	}
	original, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(run.ID, binding); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{ExpectedRun: *original}
	executor.publicationForge = &forgecontext.Context{}
	ordinary := &forgecontext.Context{}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		step := &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.ForgeContext != ordinary {
				t.Error("completed publication selection leaked into a later operation")
			}
			return &StepOutcome{}, nil
		}}
		outcome, err := executor.executeWithPublicationSelection(step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo, ForgeContext: ordinary})
		if err != nil || outcome == nil || outcome.NeedsApproval {
			t.Fatalf("ordinary operation after publication failed: %+v, %v", outcome, err)
		}
	}
}

func TestCIAdoptionRevokesOldScopesAndDurableSessions(t *testing.T) {
	database, locations, run, _ := setupTest(t)
	for _, role := range []SessionRole{SessionRoleReviewer, SessionRoleFixer} {
		if err := database.UpsertRunAgentSession(run.ID, string(role), "fake", "old-session"); err != nil {
			t.Fatal(err)
		}
	}
	executor := NewExecutor(database, locations, &config.Config{SessionReuse: true}, nil, nil, nil)
	executor.initializeRunScopes(run.ID)
	previousShared := executor.shared
	previousShared.RecordBaseline(run.HeadSHA, true)
	previousShared.SetSideLanes("old lane")
	wasClosed := false
	previousShared.OnClose(func() { wasClosed = true })
	executor.stepRecords = map[types.StepName]*db.StepResult{types.StepTest: {ID: "old-test"}}
	approved, ready := run.HeadSHA, int64(1)
	run.ReviewApprovedHeadSHA, run.CIReadyAt, run.TerminalHeadVerifiedAt = &approved, &ready, &ready
	adoption := &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: strings.Repeat("c", 40)}

	executor.acceptCIAdoption(run, adoption, nil, "operator")

	if !wasClosed || executor.shared == previousShared || executor.shared.SideLanes() != nil || executor.stepRecords != nil {
		t.Fatal("adoption reused the earlier side lanes or step results")
	}
	if _, _, recorded := executor.shared.Baseline(); recorded {
		t.Fatal("old Test baseline survived adoption")
	}
	if run.HeadSHA != adoption.CandidateSHA || run.ReviewApprovedHeadSHA != nil || run.CIReadyAt != nil || run.TerminalHeadVerifiedAt != nil {
		t.Fatal("adoption reused old head authority")
	}
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("old reviewer or fixer session survived: %+v, %v", sessions, err)
	}
}

func TestCIAdoptionFailedReservationLeavesTheOriginalGate(t *testing.T) {
	database, locations, _, _ := setupTest(t)
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.waiting, executor.waitingStep = true, types.StepCI
	failure := errors.New("stale candidate")
	err := executor.AdoptCIRepair(func() (*db.CIRepairAdoption, error) { return nil, failure }, nil, "operator")
	if !errors.Is(err, failure) || !executor.waiting || executor.ciAdoptionReserved || len(executor.approvalCh) != 0 {
		t.Fatalf("failed adoption consumed the original gate: %v", err)
	}
}

func TestCIPublicationReselectionCannotConsumeFreshValidationHold(t *testing.T) {
	for _, findingID := range []string{PublicationSelectionFindingID, PublicationRevalidationFindingID} {
		t.Run(findingID, func(t *testing.T) {
			database, locations, run, _ := setupTest(t)
			step, err := database.InsertStepResult(run.ID, types.StepPush)
			if err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"` + findingID + `","severity":"warning","description":"preserve this gate","action":"ask-user"}]}`
			if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, locations, nil, nil, nil, nil)
			executor.adoption, executor.runID = &db.CIRepairAdoption{}, run.ID
			executor.waiting, executor.waitingStep, executor.publicationSelectionRequired = true, types.StepPush, true
			for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
				if err := executor.Respond(types.StepPush, action, nil); err == nil {
					t.Fatalf("ordinary %s consumed the hold", action)
				}
			}
			err = executor.ReselectPublication(&forgecontext.Context{}, "operator")
			if findingID == PublicationSelectionFindingID {
				if err != nil || executor.waiting || len(executor.approvalCh) != 1 {
					t.Fatalf("explicit credential reselection refused: %v", err)
				}
			} else if err == nil || !executor.waiting || len(executor.approvalCh) != 0 {
				t.Fatal("credential reselection consumed a validation requirement")
			}
		})
	}
}

func TestCIRetainedHeadUnreadablePreservesGateAndRefusesOrdinaryResponses(t *testing.T) {
	database, paths, run, _ := setupTest(t)
	public := strings.Repeat("a", 40)
	generation := int64(1)
	run.HeadSHA, run.LastPushedSHA, run.PushGeneration = public, &public, &generation
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	held, err := RetainedCIHead(context.Background(), run, t.TempDir())
	if !held || err == nil {
		t.Fatalf("unreadable retained head = held %v, error %v", held, err)
	}
	executor := NewExecutor(database, paths, nil, nil, nil, nil)
	executor.workDir, executor.runID = t.TempDir(), run.ID
	executor.waiting, executor.waitingStep = true, types.StepCI
	// An unreadable binding must be preserved, including when the DB read fails.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
		if err := executor.Respond(types.StepCI, action, nil); err == nil {
			t.Fatalf("%s consumed an unreadable CI hold", action)
		}
		if !executor.waiting || len(executor.approvalCh) != 0 {
			t.Fatal("refusal changed the waiting gate")
		}
	}
}

func TestCIAdoptionReservationPreventsOldGateReconciliation(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	step.resolved.Store(true)
	executor := NewExecutor(database, paths, nil, nil, []Step{step}, nil)
	executor.ciAdoptionReserved = true
	resolved, err := executor.reconcileApprovalGate(t.Context(), step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo}, `{"findings":[]}`)
	if err != nil || resolved || step.calls.Load() != 0 {
		t.Fatalf("old CI reconciled after adoption reservation: resolved=%v error=%v calls=%d", resolved, err, step.calls.Load())
	}
}

func TestCIRetainedGuardLeavesProtectedPathRetryOwnership(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	step := &protectedPathReconcilerStep{reconcilingApprovalStep: reconcilingApprovalStep{name: types.StepCI}}
	parked := make(chan struct{}, 2)
	executor := NewExecutor(database, locations, nil, nil, []Step{step}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
			parked <- struct{}{}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	workDir := t.TempDir()
	go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("protected-path executor did not stop")
		}
	})
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("protected-path refusal did not park")
	}
	if err := executor.Respond(types.StepCI, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "use fix") {
		t.Fatalf("retained guard displaced the protected-path decision: %v", err)
	}
	if err := executor.Respond(types.StepCI, types.ActionFix, nil); err != nil {
		t.Fatalf("retained guard displaced the protected-path retry: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("original protected-path retry did not execute")
	}
}

func TestCIAdoptionLimitsAndReviewCarrySurviveOrdinaryReset(t *testing.T) {
	database, paths, run, _ := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"old","description":"old finding","file":"source.go","severity":"error","action":"auto-fix"}]}`
	first, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["old"]`
	if err := database.SetStepRoundUserDecision(first.ID, &selected, db.RoundSelectionSourceAutoFix, nil); err != nil {
		t.Fatal(err)
	}
	original, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	configuration := &config.Config{}
	configuration.AutoFix.Review = 9
	executor := NewExecutor(database, paths, configuration, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{Steps: []db.CIRepairAdoptionStep{{Snapshot: *step, Rounds: []db.StepRound{*original[0]}, EffectiveAutoFixLimit: 1}}}
	state, err := executor.durableExecutionState(step.ID)
	if err != nil || state.autoFixAttempts != 1 || state.roundNum != 1 || state.outstandingFindings != "" || len(state.selectedOutstandingIDs) != 0 {
		t.Fatalf("adopted state renews budget or reuses review: %+v, %v", state, err)
	}
	if limit := executor.autoFixLimit(types.StepReview); limit != 1 {
		t.Fatalf("original limit changed to %d", limit)
	}
}

func TestCIAdoptionRestartsOnlyAnExactlyReviewedInterruptedPush(t *testing.T) {
	for _, name := range []string{"exact push", "agent writer", "review missing", "interrupted test"} {
		t.Run(name, func(t *testing.T) {
			database, _, run, _ := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			run.Status, run.PushActive = types.RunRunning, true
			head := run.HeadSHA
			run.ReviewApprovedHeadSHA = &head
			var steps []Step
			for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepPush, types.StepPR, types.StepCI} {
				step, err := database.InsertStepResult(run.ID, stepName)
				if err != nil {
					t.Fatal(err)
				}
				steps = append(steps, newPassStep(stepName))
				if stepName == types.StepReview || stepName == types.StepTest {
					if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				}
				if stepName == types.StepPush || (name == "interrupted test" && stepName == types.StepTest) {
					if err := database.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
						t.Fatal(err)
					}
				}
				if name == "agent writer" && stepName == types.StepPush {
					pid := 123
					if err := database.SetStepAgentActivity(step.ID, "active writer", &pid); err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "review missing" {
				run.ReviewApprovedHeadSHA = nil
			}
			index, err := CIAdoptionPendingIndex(database, run, steps)
			if name == "exact push" {
				if err != nil || index != 2 {
					t.Fatalf("exact publication replay refused: %d, %v", index, err)
				}
			} else if err == nil {
				t.Fatal("interrupted validation or writer was replayable")
			}
		})
	}
}
