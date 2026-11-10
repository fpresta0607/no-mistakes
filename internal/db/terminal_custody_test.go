package db

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newTerminalAdoptionFixture(t *testing.T) (*DB, TerminalAdoption) {
	t.Helper()
	database := openTestDB(t)
	repo, err := database.InsertRepo("/fixture", "https://github.com/example/fixture", "main")
	if err != nil {
		t.Fatal(err)
	}
	profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {profile}, "reviewer": {profile}, "fixer": {profile}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", strings.Repeat("b", 40), strings.Repeat("c", 40), nil, "nonce", "frozen-policy", "intent-digest", "", false, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range types.AllSteps() {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
			t.Fatal(err)
		}
		findings := `{"findings":[{"id":"original","severity":"error","description":"retained finding"}]}`
		round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 5)
		if err != nil {
			t.Fatal(err)
		}
		selected := `["original"]`
		if err := database.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceAutoFix); err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 5, "original.log"); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunErrorStatusWithVerifiedHead(run.ID, "interrupted", types.RunFailed, run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	before, err := database.ReadContinuationSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := *assertion
	fresh.TrustedSHA = strings.Repeat("d", 40)
	freshProof, err := fresh.Verify(fresh.TrustedSHA, fresh.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	event := TerminalAdoption{Snapshot: *before, CandidateSHA: run.HeadSHA, CandidateTree: strings.Repeat("e", 40), SourceRef: "refs/no-mistakes/recover/" + run.ID, Reason: "validate preserved source", Operator: "fixture", Assertion: &fresh, Proof: freshProof}
	return database, event
}

func TestTerminalContinuationKeepsCancelledPredecessorImmutable(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	if err := database.UpdateRunErrorStatusWithVerifiedHead(event.Snapshot.Run.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled, event.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	before, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	event.Snapshot = *before
	if _, err := database.AdoptTerminalCandidate(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	after, err := database.ReadContinuationSnapshot(before.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("cancelled predecessor was rewritten: status %s -> %s; source continuation must create a bound successor", before.Run.Status, after.Run.Status)
	}
	runs, err := database.GetRunsByRepo(before.Repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Errorf("source continuation has %d run rows, want immutable predecessor plus fresh-validation successor", len(runs))
	}
}

func TestTerminalAdoptionKeepsHistoryAndOriginalLaunchReceipt(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	event.Gates = `[{"name":"fresh-required","after":"test","command":"exit 0"}]`
	before := &event.Snapshot
	run := &before.Run
	accepted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatalf("adoption = %+v, %v", accepted, err)
	}
	after, err := database.ReadContinuationSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("adoption mutated predecessor source, history or receipt")
	}
	child, err := database.ReadContinuationSnapshot(accepted.Successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Run.ID == run.ID || child.Run.Status != types.RunPending || child.Run.HeadSHA != event.CandidateSHA || child.Run.ReviewApprovedHeadSHA != nil || child.Run.TerminalHeadVerifiedAt != nil || child.Run.CIReadyAt != nil || child.Run.LaunchReceiptClaimedAt != nil || len(child.Steps) != 0 {
		t.Fatalf("successor reused validation authority: %+v", child)
	}
	if child.Gates != event.Gates || child.Gates == before.Gates {
		t.Fatal("successor did not pin its own required gates")
	}
	if !reflect.DeepEqual(child.Run.LaunchAssertion, event.Assertion) || !reflect.DeepEqual(child.Run.LaunchAssertionProof, event.Proof) || !reflect.DeepEqual(child.Run.LaunchValidationGeneration, run.LaunchValidationGeneration) || child.Run.LaunchNonce == nil || *child.Run.LaunchNonce == *run.LaunchNonce {
		t.Fatal("successor lacks fresh proof and nonce under frozen policy")
	}
	if repeated, err := database.AdoptTerminalCandidate(context.Background(), event); err == nil {
		t.Fatalf("a repeated acceptance created another successor: %+v", repeated)
	}
	if _, err := database.sql.Exec(`UPDATE terminal_adoptions SET event_json = '{}' WHERE run_id = ?`, run.ID); err == nil {
		t.Fatal("accepted provenance could be rewritten")
	}
	event.CandidateTree = strings.Repeat("f", 40)
	if _, err := database.AdoptTerminalCandidate(context.Background(), event); err == nil {
		t.Fatal("different recovery binding replaced the original")
	}
}

func TestTerminalAdoptionRefusesASuccessorAsPredecessor(t *testing.T) {
	// Arrange: a successor that failed after acceptance is the branch's latest
	// terminal run, which is exactly what a raw caller could once name.
	database, event := newTerminalAdoptionFixture(t)
	accepted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatusWithVerifiedHead(accepted.Successor.ID, "interrupted", types.RunFailed, accepted.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	successor, err := database.ReadContinuationSnapshot(accepted.Successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	chained := TerminalAdoption{Snapshot: *successor, CandidateSHA: accepted.CandidateSHA, CandidateTree: accepted.CandidateTree, SourceRef: event.SourceRef, Reason: "continue the continuation", Operator: "fixture", Assertion: event.Assertion, Proof: event.Proof}

	// Act
	adopted, err := database.AdoptTerminalCandidate(context.Background(), chained)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "successor cannot be adopted") {
		t.Fatalf("successor adopted as a predecessor: %+v, %v", adopted, err)
	}
	if record, err := database.GetTerminalAdoption(accepted.Successor.ID); err != nil || record != nil {
		t.Fatalf("refusal recorded a chained acceptance: %+v, %v", record, err)
	}
	if runs, err := database.GetRunsByRepo(event.Snapshot.Repo.ID); err != nil || len(runs) != 2 {
		t.Fatalf("refusal changed the run family: %d runs, %v", len(runs), err)
	}
}

func TestSourceMayContinueOnlyWhileContinuationCanAcceptOrReplay(t *testing.T) {
	for _, test := range []struct {
		name            string
		isSuccessor     bool
		hasStep         bool
		successorStatus types.RunStatus
		isReplayable    bool
		shouldRetain    bool
	}{
		{name: "predecessor of an untouched successor", isReplayable: true, shouldRetain: true},
		{name: "predecessor of a progressed successor", hasStep: true},
		{name: "predecessor of a successor that failed before its first step", successorStatus: types.RunFailed},
		{name: "predecessor of a successor cancelled before its first step", successorStatus: types.RunCancelled},
		{name: "failed successor", isSuccessor: true, hasStep: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			database, event := newTerminalAdoptionFixture(t)
			accepted, err := database.AdoptTerminalCandidate(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			if test.hasStep {
				if _, err := database.InsertStepResult(accepted.Successor.ID, types.StepIntent); err != nil {
					t.Fatal(err)
				}
			}
			if test.successorStatus != "" {
				if err := database.UpdateRunStatus(accepted.Successor.ID, test.successorStatus); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.ValidateTerminalSuccessor(accepted); (err == nil) != test.isReplayable {
				t.Fatalf("replay error = %v, want replayable=%v", err, test.isReplayable)
			}
			runID := event.Snapshot.Run.ID
			if test.isSuccessor {
				if err := database.UpdateRunErrorStatusWithVerifiedHead(accepted.Successor.ID, "interrupted", types.RunFailed, accepted.CandidateSHA); err != nil {
					t.Fatal(err)
				}
				runID = accepted.Successor.ID
			}
			run, err := database.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}

			// Act
			mayContinue, err := database.SourceMayContinue(run)

			// Assert
			if err != nil || mayContinue != test.shouldRetain {
				t.Fatalf("SourceMayContinue = %v, %v; want %v", mayContinue, err, test.shouldRetain)
			}
		})
	}
}

func TestTerminalAdoptionRefusalIsAtomic(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *DB, *TerminalAdoption)
	}{
		{"generation", func(t *testing.T, database *DB, event *TerminalAdoption) {
			_, err := database.sql.Exec(`UPDATE runs SET launch_validation_generation = 'changed' WHERE id = ?`, event.Snapshot.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"repository", func(_ *testing.T, _ *DB, event *TerminalAdoption) { event.Snapshot.Repo.ID = "wrong" }},
		{"run", func(_ *testing.T, _ *DB, event *TerminalAdoption) { event.Snapshot.Run.ID = "wrong" }},
		{"branch", func(_ *testing.T, _ *DB, event *TerminalAdoption) { event.Snapshot.Run.Branch = "wrong" }},
		{"proof", func(_ *testing.T, _ *DB, event *TerminalAdoption) { event.Proof = nil }},
		{"candidate", func(_ *testing.T, _ *DB, event *TerminalAdoption) { event.CandidateSHA = "HEAD" }},
		{"writer", func(t *testing.T, database *DB, event *TerminalAdoption) {
			if err := database.SetRunPushActive(event.Snapshot.Run.ID, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"newer run", func(t *testing.T, database *DB, event *TerminalAdoption) {
			if _, err := database.InsertRun(event.Snapshot.Repo.ID, event.Snapshot.Run.Branch, "other", "base"); err != nil {
				t.Fatal(err)
			}
		}},
		{"round history", func(t *testing.T, database *DB, event *TerminalAdoption) {
			if _, err := database.InsertStepRound(event.Snapshot.Steps[0].ID, 2, "initial", nil, nil, 1); err != nil {
				t.Fatal(err)
			}
		}},
		{"rollback", func(t *testing.T, database *DB, _ *TerminalAdoption) {
			if _, err := database.sql.Exec(`CREATE TEMP TRIGGER refuse_terminal_child BEFORE INSERT ON terminal_adoptions BEGIN SELECT RAISE(ABORT, 'fixture refusal'); END`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, event := newTerminalAdoptionFixture(t)
			runID := event.Snapshot.Run.ID
			test.mutate(t, database, &event)
			before, err := database.ReadContinuationSnapshot(runID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.AdoptTerminalCandidate(context.Background(), event); err == nil {
				t.Fatal("invalid adoption was accepted")
			}
			after, err := database.ReadContinuationSnapshot(runID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal mutated source history: %v", err)
			}
			if record, err := database.GetTerminalAdoption(runID); err != nil || record != nil {
				t.Fatalf("refusal recorded an acceptance: %+v %v", record, err)
			}
		})
	}
}

func TestTerminalAdoptionRefusesChangedOrProgressedSuccessor(t *testing.T) {
	for _, state := range []string{"untouched", "running", "failed", "cancelled", "completed", "step", "head", "generation", "receipt", "gates", "newer run"} {
		t.Run(state, func(t *testing.T) {
			database, event := newTerminalAdoptionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := database.AdoptTerminalCandidate(ctx, event); err == nil {
				t.Fatal("cancelled transaction accepted")
			}
			accepted, err := database.AdoptTerminalCandidate(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			childID := accepted.Successor.ID
			switch state {
			case "running", "failed", "cancelled", "completed":
				if err := database.UpdateRunStatus(childID, types.RunStatus(state)); err != nil {
					t.Fatal(err)
				}
			case "step":
				if _, err := database.InsertStepResult(childID, types.StepIntent); err != nil {
					t.Fatal(err)
				}
			case "head":
				if _, err := database.sql.Exec(`UPDATE runs SET head_sha = 'changed' WHERE id = ?`, childID); err != nil {
					t.Fatal(err)
				}
			case "generation":
				if _, err := database.sql.Exec(`UPDATE runs SET push_generation = 2 WHERE id = ?`, childID); err != nil {
					t.Fatal(err)
				}
			case "newer run":
				other, err := database.InsertRun(event.Snapshot.Repo.ID, event.Snapshot.Run.Branch, "other", "base")
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunStatus(other.ID, types.RunCompleted); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				if _, err := database.sql.Exec(`UPDATE runs SET launch_receipt_claimed_at = 123 WHERE id = ?`, childID); err != nil {
					t.Fatal(err)
				}
			case "gates":
				if err := database.SetRunGates(childID, `[{"name":"changed","after":"test","command":"exit 0"}]`); err != nil {
					t.Fatal(err)
				}
			}
			before, err := database.ReadContinuationSnapshot(childID)
			if err != nil {
				t.Fatal(err)
			}
			err = database.ValidateTerminalSuccessor(accepted)
			if state == "untouched" {
				if err != nil {
					t.Fatalf("untouched successor refused: %v", err)
				}
			} else if err == nil {
				t.Fatal("changed successor accepted")
			}
			after, readErr := database.ReadContinuationSnapshot(childID)
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("replay changed successor")
			}
			parent, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
			if err != nil || !reflect.DeepEqual(parent, &event.Snapshot) {
				t.Fatal("replay changed predecessor")
			}
			runs, err := database.GetRunsByRepo(event.Snapshot.Repo.ID)
			expectedRuns := 2
			if state == "newer run" {
				expectedRuns = 3
			}
			if err != nil || len(runs) != expectedRuns {
				t.Fatal("repeat created another child")
			}
		})
	}
}

func TestTerminalSuccessorSerializesIndependentAcceptances(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	var sequence int
	var name, path string
	if err := database.sql.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	independent, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	start := make(chan struct{})
	type result struct {
		event *TerminalAdoption
		err   error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, handle := range []*DB{database, independent} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			accepted, err := handle.AdoptTerminalCandidate(context.Background(), event)
			results <- result{accepted, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	createdCount := 0
	for result := range results {
		if result.err == nil {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("independent acceptances created %d successors, want exactly 1", createdCount)
	}
	runs, err := database.GetRunsByRepo(event.Snapshot.Repo.ID)
	if err != nil || len(runs) != 2 {
		t.Fatal("race changed run count")
	}
	original, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
	if err != nil || !reflect.DeepEqual(original, &event.Snapshot) {
		t.Fatal("race changed predecessor")
	}
}

func TestTerminalSuccessorCountsManualAndAutomaticFamilySpending(t *testing.T) {
	for _, manual := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			database, event := newTerminalAdoptionFixture(t)
			var review StepResult
			for _, step := range event.Snapshot.Steps {
				if step.StepName == types.StepReview {
					review = step
				}
			}
			if _, err := database.sql.Exec(`UPDATE step_results SET auto_fix_limit=3 WHERE id=?`, review.ID); err != nil {
				t.Fatal(err)
			}
			// The selected original round is one dispatched automatic attempt.
			for index := 0; index < manual; index++ {
				_, err := database.InsertStepRound(review.ID, index+2, "user_fix", nil, nil, 1)
				if err != nil {
					t.Fatal(err)
				}

			}
			snapshot, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			event.Snapshot = *snapshot
			accepted, err := database.AdoptTerminalCandidate(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			remaining, family, err := database.TerminalPhaseRemaining(accepted.Successor.ID, types.StepReview, 3)
			expected := max(0, 3-max(1, manual))
			if err != nil || !family || remaining.Remaining != expected || remaining.Spent != max(1, manual) {
				t.Fatalf("remaining=%+v family=%v err=%v want=%d", remaining, family, err, expected)
			}
			step, err := database.InsertStepResult(accepted.Successor.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(step.ID, 1, "user_fix", nil, nil, 1); err != nil {
				t.Fatal(err)
			}
			remaining, _, err = database.TerminalPhaseRemaining(accepted.Successor.ID, types.StepReview, 3)
			if err != nil || remaining.Remaining != max(0, expected-1) {
				t.Fatalf("child manual spending=%+v err=%v", remaining, err)
			}
			untouched, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
			if err != nil || !reflect.DeepEqual(untouched, &event.Snapshot) {
				t.Fatal("budget query changed predecessor")
			}
		})
	}
}

func TestTerminalPredecessorRejectsHistoryMutation(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	if _, err := database.AdoptTerminalCandidate(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(event.Snapshot.Run.ID, types.RunRunning); err == nil {
		t.Fatal("predecessor reopened")
	}
	if _, err := database.InsertStepResult(event.Snapshot.Run.ID, types.StepReview); err == nil {
		t.Fatal("step appended to predecessor")
	}
	if err := database.FailStep(event.Snapshot.Steps[0].ID, "rewritten", 0); err == nil {
		t.Fatal("predecessor step changed")
	}
	if _, err := database.InsertStepRound(event.Snapshot.Steps[0].ID, 2, "auto_fix", nil, nil, 1); err == nil {
		t.Fatal("round appended to predecessor")
	}
	if err := database.SetStepRoundSelection(event.Snapshot.Rounds[0][0].ID, nil, RoundSelectionSourceUser); err == nil {
		t.Fatal("predecessor selection changed")
	}
	if err := database.RecordTerminalSourceState(event.Snapshot.Run.ID, "dirty:changed"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
	if err != nil || !reflect.DeepEqual(snapshot, &event.Snapshot) {
		t.Fatal("immutable history changed")
	}
}

func TestTerminalSuccessorInheritsPublicBindingWithoutValidationStamps(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	parentID := event.Snapshot.Run.ID
	if err := database.UpdateRunPushBinding(parentID, PushBinding{HeadSHA: event.Snapshot.Run.HeadSHA, TargetKind: "fork", TargetFingerprint: "fixture-target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(parentID, "https://github.com/example/fixture/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRState(parentID, "open"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadContinuationSnapshot(parentID)
	if err != nil {
		t.Fatal(err)
	}
	event.Snapshot = *snapshot
	accepted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	child := accepted.Successor
	if !reflect.DeepEqual(child.PushGeneration, snapshot.Run.PushGeneration) || !reflect.DeepEqual(child.LastPushedSHA, snapshot.Run.LastPushedSHA) || !reflect.DeepEqual(child.LastPushedAt, snapshot.Run.LastPushedAt) || !reflect.DeepEqual(child.PRURL, snapshot.Run.PRURL) || child.ReviewApprovedHeadSHA != nil || child.CIReadyAt != nil || child.TerminalHeadVerifiedAt != nil {
		t.Fatalf("inherited binding invented authority: %+v", child)
	}
	if err := database.UpdateRunPublication(child.ID, PushBinding{HeadSHA: strings.Repeat("f", 40), TargetKind: "fork", TargetFingerprint: "fixture-target", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	published, err := database.GetRun(child.ID)
	if err != nil || published.PushGeneration == nil || *published.PushGeneration != 2 {
		t.Fatalf("generation=%+v err=%v", published, err)
	}
	unchanged, err := database.ReadContinuationSnapshot(parentID)
	if err != nil || !reflect.DeepEqual(unchanged, snapshot) {
		t.Fatal("successor publication changed predecessor")
	}
	if _, err := database.sql.Exec(`DELETE FROM terminal_adoptions WHERE run_id=?`, parentID); err == nil {
		t.Fatal("durable successor binding deleted")
	}
}

func TestTerminalHistoryLeavesOnlyWithItsRepository(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	adopted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	parentID := event.Snapshot.Run.ID
	for name, statement := range map[string]string{
		"predecessor": `DELETE FROM runs WHERE id = '` + parentID + `'`,
		"successor":   `DELETE FROM runs WHERE id = '` + adopted.Successor.ID + `'`,
		"step":        `DELETE FROM step_results WHERE id = '` + event.Snapshot.Steps[0].ID + `'`,
		"round":       `DELETE FROM step_rounds WHERE id = '` + event.Snapshot.Rounds[0][0].ID + `'`,
		"adoption":    `DELETE FROM terminal_adoptions WHERE run_id = '` + parentID + `'`,
	} {
		if _, err := database.sql.Exec(statement); err == nil {
			t.Fatalf("%s deleted while its repository is registered", name)
		}
	}

	if err := database.DeleteRepo(event.Snapshot.Repo.ID); err != nil {
		t.Fatalf("repository with terminal history could not be removed: %v", err)
	}

	for table, query := range map[string]string{
		"runs":               `SELECT COUNT(*) FROM runs`,
		"step_results":       `SELECT COUNT(*) FROM step_results`,
		"step_rounds":        `SELECT COUNT(*) FROM step_rounds`,
		"terminal_adoptions": `SELECT COUNT(*) FROM terminal_adoptions`,
	} {
		var count int
		if err := database.sql.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s kept %d rows after its repository was removed: %v", table, count, err)
		}
	}
}

func TestTerminalSuccessorKeepsTheConfiguredAllowanceOfAnUnreachedPhase(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	// The predecessor ended at Review: every later phase stayed pending, never
	// started and so never recorded an allowance or spent a repair.
	for _, step := range event.Snapshot.Steps {
		if step.StepOrder <= types.StepReview.Order() {
			continue
		}
		if _, err := database.sql.Exec(`DELETE FROM step_rounds WHERE step_result_id = ?`, step.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := database.sql.Exec(`UPDATE step_results SET status = ?, started_at = NULL, completed_at = NULL, auto_fix_limit = NULL, exit_code = NULL, duration_ms = NULL, log_path = NULL WHERE id = ?`, types.StepStatusPending, step.ID); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := database.ReadContinuationSnapshot(event.Snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	event.Snapshot = *snapshot
	adopted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}

	unreached, _, err := database.TerminalPhaseRemaining(adopted.Successor.ID, types.StepTest, 2)
	reached, _, reachedErr := database.TerminalPhaseRemaining(adopted.Successor.ID, types.StepReview, 2)

	if err != nil || unreached.Limit != 2 || unreached.Spent != 0 || unreached.Remaining != 2 {
		t.Errorf("unreached phase lost its configured allowance: %+v err=%v", unreached, err)
	}
	if reachedErr != nil || reached.Limit != 1 || reached.Remaining != 0 {
		t.Errorf("reached phase escaped its recorded allowance: %+v err=%v", reached, reachedErr)
	}
}

func TestTerminalSuccessorBudgetSurvivesARepositoryRegistrationChange(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	adopted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateRepoMetadataWithFork(event.Snapshot.Repo.ID, "git@github.com:example/fixture.git", "https://github.com/fork/fixture", "trunk"); err != nil {
		t.Fatal(err)
	}

	budget, family, err := database.TerminalPhaseRemaining(adopted.Successor.ID, types.StepReview, 3)

	if err != nil || !family || budget.Limit != 1 || budget.Spent != 1 || budget.Remaining != 0 {
		t.Fatalf("re-registered repository broke the successor's repair budget: %+v family=%v err=%v", budget, family, err)
	}
}

func TestTerminalSuccessorGivesAnUnrecordedPhaseNoAutomaticAllowance(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	adopted, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	phase := types.CustomGateStepName(types.StepTest, "new-required-gate")
	budget, family, err := database.TerminalPhaseRemaining(adopted.Successor.ID, phase, 5)
	if err != nil || !family || budget.Limit != 0 || budget.Remaining != 0 {
		t.Fatalf("new phase renewed repair capacity: %+v err=%v", budget, err)
	}
}
