package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func admissionFixture(t *testing.T) (*DB, ValidationAdmission) {
	t.Helper()
	database := openTestDB(t)
	repository, err := database.InsertRepo(t.TempDir(), "https://example.test/owner/repository.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := RunIntent{Summary: "Validate authored changes", Source: RunIntentSourceAgent, Score: 1}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
	parent, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", strings.Repeat("b", 40), strings.Repeat("c", 40), &intent, "original-nonce", "frozen-generation", digest, "", true, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(parent.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, 2); err != nil {
		t.Fatal(err)
	}
	findings, selected := `{"findings":[{"id":"r1","description":"repair"}]}`, `["r1"]`
	for index, source := range []string{RoundSelectionSourceAutoFix, RoundSelectionSourceUser, RoundSelectionSourceUser} {
		round, err := database.InsertStepRound(step.ID, index+1, "initial", &findings, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.SetStepRoundSelection(round.ID, &selected, source); err != nil {
			t.Fatal(err)
		}
		if index < 2 {
			if _, err := database.InsertStepRound(step.ID, index+10, "auto_fix", nil, nil, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Keep the rounds in execution order while retaining the final dispatched selection.
	if _, err := database.sql.Exec(`UPDATE step_rounds SET round = CASE round WHEN 1 THEN 1 WHEN 10 THEN 2 WHEN 2 THEN 3 WHEN 11 THEN 4 WHEN 3 THEN 5 END WHERE step_result_id = ?`, step.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatus(parent.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repository.ID, Branch: parent.Branch,
		CallerDir: repository.WorkingPath, CandidateSHA: strings.Repeat("d", 40), CandidateTree: strings.Repeat("e", 40), LaunchNonce: "original-nonce", ValidationGeneration: "frozen-generation", IntentDigest: digest,
		ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("f", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate a complete declared authored candidate"}
	return database, ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, Gates: "[]", WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), Operator: "authenticated-fixture"}
}

func TestValidationAdmissionPreservesThePredecessorAndStartsWithNoCertification(t *testing.T) {
	database, proposal := admissionFixture(t)
	event, created, err := database.AcceptValidationAdmission(context.Background(), proposal)
	if err != nil || !created {
		t.Fatalf("accept: created=%v error=%v", created, err)
	}
	parent, err := database.ReadTerminalSnapshot(proposal.Request.PredecessorID)
	if err != nil || !reflect.DeepEqual(*parent, proposal.Snapshot) {
		t.Fatalf("predecessor changed: %v", err)
	}
	child, err := database.GetRun(event.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != types.RunPending || child.HeadSHA != proposal.Request.CandidateSHA || child.ReviewApprovedHeadSHA != nil || child.CIReadyAt != nil {
		t.Fatalf("child inherited validation authority: %+v", child)
	}
	if child.LaunchNonce == nil || *child.LaunchNonce == proposal.Request.LaunchNonce {
		t.Fatal("child copied the original receipt")
	}
	steps, err := database.GetStepsByRun(child.ID)
	if err != nil || len(steps) != 0 {
		t.Fatalf("child copied old stages: %v %v", steps, err)
	}
	family, err := database.ValidationFamily(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	spending := family.Spending[types.StepReview]
	if spending.Automatic != 1 || spending.Manual != 2 || spending.Remaining != 0 {
		t.Fatalf("family spending = %+v", spending)
	}
	if family.PolicyOrigin != "controller_assertion" || family.HistoricalSourceCompleteness != "unknown" {
		t.Fatalf("provenance = %+v", family)
	}
	replay, created, err := database.AcceptValidationAdmission(context.Background(), proposal)
	if err != nil || created || replay.ChildID != child.ID {
		t.Fatalf("replay reset the admission: %+v %v %v", replay, created, err)
	}
	proposal.Request.ControllerPolicy.ReviewCycles++
	if _, _, err := database.AcceptValidationAdmission(context.Background(), proposal); err == nil {
		t.Fatal("replay replaced the frozen controller assertion")
	}
}

func TestValidationAdmissionRejectsOutstandingNativeSafetyRefusalWithoutMutation(t *testing.T) {
	database, proposal := admissionFixture(t)
	step := proposal.Snapshot.Steps[0]
	findings := `{"findings":[{"id":"agent-safety-refusal","severity":"error","description":"native provider refused execution","action":"ask-user"}]}`
	if err := database.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(proposal.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	proposal.Snapshot, proposal.Request.SnapshotDigest = *snapshot, snapshot.Digest()
	before, err := database.GetRunsByRepo(proposal.Request.RepoID)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = database.AcceptValidationAdmission(context.Background(), proposal)

	if err == nil || !strings.Contains(err.Error(), "native agent safety refusal") {
		t.Fatalf("authored admission did not refuse the native provider refusal: %v", err)
	}
	after, err := database.GetRunsByRepo(proposal.Request.RepoID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refusal changed run history: %v", err)
	}
	event, err := database.GetValidationAdmission(proposal.Request.PredecessorID)
	if err != nil || event != nil {
		t.Fatalf("refusal recorded an admission: %+v %v", event, err)
	}
}

func TestValidationFamilyDetectsSourceChangesInNormalRounds(t *testing.T) {
	for _, change := range []string{"recorded head", "starting head", "reviewed head"} {
		t.Run(change, func(t *testing.T) {
			database, proposal := admissionFixture(t)
			event, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
			if err != nil {
				t.Fatal(err)
			}
			changedHead := strings.Repeat("9", 40)
			if change == "recorded head" {
				err = database.UpdateRunHeadSHA(event.ChildID, changedHead)
			} else {
				step, stepErr := database.InsertStepResult(event.ChildID, types.StepDocument)
				if stepErr != nil {
					t.Fatal(stepErr)
				}
				round, roundErr := database.InsertStepRound(step.ID, 1, "initial", nil, nil, 1)
				if roundErr != nil {
					t.Fatal(roundErr)
				}
				column := "starting_head_sha"
				if change == "reviewed head" {
					column = "reviewed_head_sha"
				}
				_, err = database.sql.Exec(`UPDATE step_rounds SET `+column+` = ? WHERE id = ?`, changedHead, round.ID)
			}
			if err != nil {
				t.Fatal(err)
			}

			family, err := database.ValidationFamily(event.ChildID)

			if err != nil || !family.HasChildRepairs || family.Spending[types.StepReview].Remaining != 0 {
				t.Fatalf("normal-round source change escaped zero-repair authority: %+v %v", family, err)
			}
		})
	}
}

func TestValidationAdmissionRejectsChangedHistoryAndCompetingRunsAtomically(t *testing.T) {
	for _, test := range []string{"history", "active", "newer", "historical candidate"} {
		t.Run(test, func(t *testing.T) {
			database, proposal := admissionFixture(t)
			switch test {
			case "historical candidate":
				proposal.Request.CandidateSHA = proposal.Snapshot.Run.HeadSHA
			case "history":
				if err := database.UpdateRunError(proposal.Request.PredecessorID, "changed"); err != nil {
					t.Fatal(err)
				}
			case "active":
				if err := database.UpdateRunStatus(proposal.Request.PredecessorID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			case "newer":
				if _, err := database.InsertRun(proposal.Request.RepoID, proposal.Request.Branch, "newer", "base"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := database.AcceptValidationAdmission(context.Background(), proposal); err == nil {
				t.Fatal("accepted stale predecessor evidence")
			}
			after, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal mutated runs: %v", err)
			}
		})
	}
}

func TestValidationAdmissionDescendantsKeepSpendingAndTheControllerAssertion(t *testing.T) {
	database, proposal := admissionFixture(t)
	first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(first.ChildID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(first.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	descendant := proposal
	descendant.Snapshot = *snapshot
	descendant.Request.PredecessorID, descendant.Request.SnapshotDigest = first.ChildID, snapshot.Digest()
	descendant.Request.LaunchNonce = *snapshot.Run.LaunchNonce
	descendant.Request.CandidateSHA = strings.Repeat("9", 40)
	broader := descendant
	broader.Request.ControllerPolicy.ReviewCycles++
	if _, _, err := database.AcceptValidationAdmission(context.Background(), broader); err == nil {
		t.Fatal("descendant changed the controller policy assertion")
	}
	second, _, err := database.AcceptValidationAdmission(context.Background(), descendant)
	if err != nil {
		t.Fatal(err)
	}
	family, err := database.ValidationFamily(second.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if family.Spending[types.StepReview].Automatic != 1 || family.Spending[types.StepReview].Manual != 2 || family.Spending[types.StepReview].Remaining != 0 || family.HasChildRepairs {
		t.Fatalf("descendant spending = %+v", family)
	}
	if err := database.RefuseUnlinkedValidation(proposal.Request.RepoID, proposal.Request.Branch); err == nil {
		t.Fatal("ordinary launch could drop the family")
	}
}

func TestValidationAdmissionReplayRefusesDriftWithoutCreatingAReplacement(t *testing.T) {
	database, proposal := admissionFixture(t)
	first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunError(proposal.Request.PredecessorID, "historical record changed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.AcceptValidationAdmission(context.Background(), proposal); err == nil {
		t.Fatal("accepted a replay with changed immutable predecessor history")
	}
	child, err := database.GetRun(first.ChildID)
	if err != nil || child.Status != types.RunPending {
		t.Fatalf("replay altered the original child: %+v %v", child, err)
	}
}

func TestValidationBindingCoversValidationHistoryButNotRegistrationOrCustody(t *testing.T) {
	for _, test := range []struct {
		name      string
		isCovered bool
		mutate    func(database *DB, proposal ValidationAdmission, childID string) error
	}{
		{name: "repository URLs", mutate: func(database *DB, proposal ValidationAdmission, _ string) error {
			_, err := database.ReplaceRepoURLs(proposal.Request.RepoID, "https://example.test/owner/renamed.git", "https://example.test/fork/renamed.git")
			return err
		}},
		{name: "working path", mutate: func(database *DB, proposal ValidationAdmission, _ string) error {
			_, err := database.UpdateRepoWorkingPath(proposal.Request.RepoID, filepath.Join(proposal.WorktreeRoot, "moved-checkout"))
			return err
		}},
		{name: "default branch", mutate: func(database *DB, proposal ValidationAdmission, _ string) error {
			_, err := database.UpdateRepoMetadata(proposal.Request.RepoID, "https://example.test/owner/repository.git", "trunk")
			return err
		}},
		{name: "custody returned", mutate: func(database *DB, proposal ValidationAdmission, childID string) error {
			return database.SetRunsCustodyReturned([]string{proposal.Request.PredecessorID, childID})
		}},
		{name: "step round", isCovered: true, mutate: func(database *DB, proposal ValidationAdmission, _ string) error {
			_, err := database.sql.Exec(`UPDATE step_rounds SET fix_summary = 'rewritten' WHERE step_result_id IN (SELECT id FROM step_results WHERE run_id = ?)`, proposal.Request.PredecessorID)
			return err
		}},
		{name: "run status", isCovered: true, mutate: func(database *DB, proposal ValidationAdmission, _ string) error {
			return database.UpdateRunStatus(proposal.Request.PredecessorID, types.RunFailed)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, proposal := admissionFixture(t)
			first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(first.ChildID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(first.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			descendant := proposal
			descendant.Snapshot = *snapshot
			descendant.Request.PredecessorID, descendant.Request.SnapshotDigest = first.ChildID, snapshot.Digest()
			descendant.Request.LaunchNonce = *snapshot.Run.LaunchNonce
			descendant.Request.CandidateSHA = strings.Repeat("9", 40)
			if err := test.mutate(database, proposal, first.ChildID); err != nil {
				t.Fatal(err)
			}

			_, familyErr := database.ValidationFamily(first.ChildID)
			second, _, admissionErr := database.AcceptValidationAdmission(context.Background(), descendant)

			if test.isCovered {
				if familyErr == nil || admissionErr == nil {
					t.Fatalf("changed validation history was accepted: family=%v admission=%v", familyErr, admissionErr)
				}
				return
			}
			if familyErr != nil || admissionErr != nil {
				t.Fatalf("registration or custody bookkeeping broke the lineage: family=%v admission=%v", familyErr, admissionErr)
			}
			if _, err := database.ValidationFamily(second.ChildID); err != nil {
				t.Fatalf("descendant family unreadable: %v", err)
			}
		})
	}
}

func TestValidationAdmissionContinuesOnlyACompletedAuthoredChild(t *testing.T) {
	for _, test := range []struct {
		name            string
		isAuthoredChild bool
	}{
		{name: "completed authored child", isAuthoredChild: true},
		{name: "completed ordinary run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, proposal := admissionFixture(t)
			predecessorID := proposal.Request.PredecessorID
			if test.isAuthoredChild {
				first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
				if err != nil {
					t.Fatal(err)
				}
				predecessorID = first.ChildID
			}
			if err := database.UpdateRunStatus(predecessorID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(predecessorID)
			if err != nil {
				t.Fatal(err)
			}
			successor := proposal
			successor.Snapshot = *snapshot
			successor.Request.PredecessorID, successor.Request.SnapshotDigest = predecessorID, snapshot.Digest()
			successor.Request.LaunchNonce = *snapshot.Run.LaunchNonce
			successor.Request.CandidateSHA = strings.Repeat("9", 40)
			before, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil {
				t.Fatal(err)
			}

			event, created, err := database.AcceptValidationAdmission(context.Background(), successor)

			if !test.isAuthoredChild {
				after, listErr := database.GetRunsByRepo(proposal.Request.RepoID)
				if err == nil || !strings.Contains(err.Error(), "did not complete") || listErr != nil || len(after) != len(before) {
					t.Fatalf("completed ordinary run became a predecessor: %v", err)
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("completed authored child refused a successor: created=%v error=%v", created, err)
			}
			family, err := database.ValidationFamily(event.ChildID)
			if err != nil || family.PredecessorID != predecessorID || family.HasChildRepairs {
				t.Fatalf("successor family = %+v %v", family, err)
			}
			if spending := family.Spending[types.StepReview]; spending.Automatic != 1 || spending.Manual != 2 || spending.Remaining != 0 {
				t.Fatalf("successor renewed or lost spending: %+v", spending)
			}
		})
	}
}

// Admissions and adoptions store their snapshot as JSON and compare it with a
// fresh read, so a field JSON drops reads as changed history on every real run.
func TestStoredSnapshotsKeepEveryFieldTheyAreComparedOn(t *testing.T) {
	for _, record := range []any{Run{}, StepResult{}, StepRound{}} {
		kind := reflect.TypeOf(record)
		for index := 0; index < kind.NumField(); index++ {
			if field := kind.Field(index); field.Tag.Get("json") == "-" {
				t.Errorf("%s.%s is left out of stored snapshots", kind.Name(), field.Name)
			}
		}
	}
}
