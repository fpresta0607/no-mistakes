package db

import (
	"context"
	"reflect"
	"strings"
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
	before, err := database.ReadTerminalSnapshot(run.ID)
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

func TestTerminalAdoptionKeepsHistoryAndOriginalLaunchReceipt(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	before := &event.Snapshot
	run := &before.Run
	accepted, created, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil || !created {
		t.Fatalf("adoption = %+v, %v, %v", accepted, created, err)
	}
	after, err := database.ReadTerminalSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Run.ID != before.Run.ID || after.Run.Status != types.RunRunning || after.Run.ReviewApprovedHeadSHA != nil || after.Run.TerminalHeadVerifiedAt != nil || after.Run.CIReadyAt != nil {
		t.Fatalf("adopted source must need fresh validation: %+v", after.Run)
	}
	if !reflect.DeepEqual(after.Run.LaunchAssertion, before.Run.LaunchAssertion) || !reflect.DeepEqual(after.Run.LaunchAssertionProof, before.Run.LaunchAssertionProof) || !reflect.DeepEqual(after.Run.LaunchReceiptClaimedAt, before.Run.LaunchReceiptClaimedAt) || !reflect.DeepEqual(after.Run.LaunchValidationGeneration, before.Run.LaunchValidationGeneration) {
		t.Fatal("original launch receipt changed")
	}
	for index, step := range after.Steps {
		if !reflect.DeepEqual(after.Rounds[index], before.Rounds[index]) || !reflect.DeepEqual(step.AutoFixLimit, before.Steps[index].AutoFixLimit) {
			t.Fatal("history or limits changed")
		}
		if step.StepOrder >= types.StepReview.Order() && step.Status != types.StepStatusPending {
			t.Fatalf("%s did not require validation", step.StepName)
		}
	}
	replayed, isNew, err := database.AdoptTerminalCandidate(context.Background(), event)
	if err != nil || isNew || !reflect.DeepEqual(replayed, accepted) {
		t.Fatalf("independent replay = %+v %v %v", replayed, isNew, err)
	}
	if _, err := database.sql.Exec(`UPDATE terminal_adoptions SET event_json = '{}' WHERE run_id = ?`, run.ID); err == nil {
		t.Fatal("accepted provenance could be rewritten")
	}
	event.CandidateTree = strings.Repeat("f", 40)
	if _, _, err := database.AdoptTerminalCandidate(context.Background(), event); err == nil {
		t.Fatal("different recovery binding replaced the original")
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
			if _, err := database.sql.Exec(`CREATE TEMP TRIGGER refuse_terminal_reset BEFORE UPDATE OF status ON step_results BEGIN SELECT RAISE(ABORT, 'fixture refusal'); END`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, event := newTerminalAdoptionFixture(t)
			runID := event.Snapshot.Run.ID
			test.mutate(t, database, &event)
			before, err := database.ReadTerminalSnapshot(runID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := database.AdoptTerminalCandidate(context.Background(), event); err == nil {
				t.Fatal("invalid adoption was accepted")
			}
			after, err := database.ReadTerminalSnapshot(runID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal mutated source history: %v", err)
			}
			if record, err := database.GetTerminalAdoption(runID); err != nil || record != nil {
				t.Fatalf("refusal recorded an acceptance: %+v %v", record, err)
			}
		})
	}
}

func TestTerminalAdoptionCancelledAndCrashReplay(t *testing.T) {
	database, event := newTerminalAdoptionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := database.AdoptTerminalCandidate(ctx, event); err == nil {
		t.Fatal("cancelled adoption was accepted")
	}
	if _, _, err := database.AdoptTerminalCandidate(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecoverStaleRuns("interrupted after acceptance"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(event.Snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RestartAcceptedTerminal(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(event.Snapshot.Run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err = database.ReadTerminalSnapshot(event.Snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RestartAcceptedTerminal(context.Background(), snapshot); err == nil {
		t.Fatal("explicit cancellation was silently restarted")
	}
}

func TestTerminalAdoptionReplayClearsExecutionErrorAndPreservesAudit(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunRunning, types.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			database, event := newTerminalAdoptionFixture(t)
			accepted, _, err := database.AdoptTerminalCandidate(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			runID := event.Snapshot.Run.ID
			if err := database.UpdateRunErrorStatus(runID, "daemon crashed during execution", status); err != nil {
				t.Fatal(err)
			}
			before, err := database.ReadTerminalSnapshot(runID)
			if err != nil {
				t.Fatal(err)
			}

			if err := database.RestartAcceptedTerminal(context.Background(), before); err != nil {
				t.Fatal(err)
			}

			after, err := database.ReadTerminalSnapshot(runID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Run.Status != types.RunRunning || after.Run.Error != nil {
				t.Fatalf("replayed run retained a stale execution error: %+v", after.Run)
			}
			if !reflect.DeepEqual(after.Steps, before.Steps) || !reflect.DeepEqual(after.Rounds, before.Rounds) {
				t.Fatal("replay changed original validation history")
			}
			persisted, err := database.GetTerminalAdoption(runID)
			if err != nil || !reflect.DeepEqual(persisted, accepted) || persisted.Snapshot.Run.Error == nil || *persisted.Snapshot.Run.Error != "interrupted" {
				t.Fatalf("replay changed the original terminal audit: %+v, %v", persisted, err)
			}
			if err := database.UpdateRunStatus(runID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			completed, err := database.GetRun(runID)
			if err != nil || completed.Error != nil {
				t.Fatalf("completed replay retained a stale execution error: %+v, %v", completed, err)
			}
		})
	}
}
