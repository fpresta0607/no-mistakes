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
