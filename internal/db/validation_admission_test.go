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
	setFixtureTestOutputDirs(t, database, parent.ID)
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
	if !reflect.DeepEqual(child.TestOutputDirs, fixtureTestOutputDirs) {
		t.Fatalf("admitted child test output dirs = %q, want the predecessor's %q", child.TestOutputDirs, fixtureTestOutputDirs)
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
	if spending.Automatic != 1 || spending.Manual != 2 {
		t.Fatalf("family spending = %+v", spending)
	}
	if family.PolicyOrigin != "controller_assertion" || family.HistoricalSourceCompleteness != "unknown" {
		t.Fatalf("provenance = %+v", family)
	}
}

func TestValidationAdmissionRejectsASecondAdmissionWithoutMutation(t *testing.T) {
	database, proposal := admissionFixture(t)
	first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	before, err := database.GetRunsByRepo(proposal.Request.RepoID)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = database.AcceptValidationAdmission(context.Background(), proposal)

	if err == nil {
		t.Fatal("accepted a second admission for the same predecessor")
	}
	after, err := database.GetRunsByRepo(proposal.Request.RepoID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("second admission changed runs: %v", err)
	}
	event, err := database.GetValidationAdmission(proposal.Request.PredecessorID)
	if err != nil || event == nil || event.ID != first.ID {
		t.Fatalf("second admission changed the ledger: %+v %v", event, err)
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

			if err != nil || !family.HasChildRepairs {
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
	if family.Spending[types.StepReview].Automatic != 1 || family.Spending[types.StepReview].Manual != 2 || family.HasChildRepairs {
		t.Fatalf("descendant spending = %+v", family)
	}
	if err := database.RefuseUnlinkedValidation(proposal.Request.RepoID, proposal.Request.Branch); err == nil {
		t.Fatal("ordinary launch could drop the family")
	}
}

func TestRefuseUnlinkedValidationEndsWithTheNewestChildsMergedOrClosedPR(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        types.RunStatus
		prState       string
		shouldRefuse  bool
		hasDescendant bool
	}{
		{name: "active child", status: types.RunRunning, prState: "open", shouldRefuse: true},
		{name: "active child with a merged PR", status: types.RunRunning, prState: "merged", shouldRefuse: true},
		{name: "completed child with an open PR", status: types.RunCompleted, prState: "open", shouldRefuse: true},
		{name: "failed unpublished child", status: types.RunFailed, prState: "none", shouldRefuse: true},
		{name: "completed child with a merged PR", status: types.RunCompleted, prState: "merged"},
		{name: "completed child with a closed PR", status: types.RunCompleted, prState: "closed"},
		{name: "cancelled child with a closed PR", status: types.RunCancelled, prState: "closed"},
		{name: "older child ended but its descendant is active", status: types.RunCompleted, prState: "merged", shouldRefuse: true, hasDescendant: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, proposal := admissionFixture(t)
			first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
			if err != nil {
				t.Fatal(err)
			}
			if test.hasDescendant {
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
				if _, _, err := database.AcceptValidationAdmission(context.Background(), descendant); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.sql.Exec(`UPDATE runs SET status = ?, pr_state = ? WHERE id = ?`, test.status, test.prState, first.ChildID); err != nil {
				t.Fatal(err)
			}

			err = database.RefuseUnlinkedValidation(proposal.Request.RepoID, proposal.Request.Branch)

			if test.shouldRefuse && (err == nil || !strings.Contains(err.Error(), "validation-only authored lineage")) {
				t.Fatalf("ordinary launch could drop a live lineage: %v", err)
			}
			if !test.shouldRefuse && err != nil {
				t.Fatalf("ended lineage still refuses an ordinary launch: %v", err)
			}
		})
	}
}

func TestValidationAdmissionRejectsEveryEarlierFamilyHead(t *testing.T) {
	for _, test := range []struct {
		name        string
		candidate   string
		shouldAdmit bool
	}{
		{name: "submitted", candidate: strings.Repeat("b", 40)},
		{name: "terminal", candidate: strings.Repeat("1", 40)},
		{name: "published", candidate: strings.Repeat("2", 40)},
		{name: "round starting", candidate: strings.Repeat("3", 40)},
		{name: "round reviewed", candidate: strings.Repeat("4", 40)},
		{name: "review approved", candidate: strings.Repeat("5", 40)},
		{name: "caller recovery local", candidate: strings.Repeat("6", 40)},
		{name: "gate recovery local", candidate: strings.Repeat("7", 40)},
		{name: "caller recovery rewritten", candidate: strings.Repeat("8", 40)},
		{name: "unbound base", candidate: strings.Repeat("c", 40), shouldAdmit: true},
		{name: "fresh", candidate: strings.Repeat("9", 40), shouldAdmit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, proposal := admissionFixture(t)
			rootID := proposal.Request.PredecessorID
			if _, err := database.sql.Exec(`UPDATE runs SET head_sha = ?, last_pushed_sha = ?, review_approved_head_sha = ?, push_generation = 1, push_target_fingerprint = 'fixture-target' WHERE id = ?`, strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("5", 40), rootID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.sql.Exec(`UPDATE step_rounds SET starting_head_sha = ?, reviewed_head_sha = ? WHERE id = ?`, strings.Repeat("3", 40), strings.Repeat("4", 40), proposal.Snapshot.Rounds[0][0].ID); err != nil {
				t.Fatal(err)
			}
			root, err := database.ReadTerminalSnapshot(rootID)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Snapshot, proposal.Request.SnapshotDigest = *root, root.Digest()
			proposal.Request.PublishedHeadSHA, proposal.Request.PushGeneration, proposal.Request.PushTargetFingerprint = *root.Run.LastPushedSHA, *root.Run.PushGeneration, *root.Run.PushTargetFingerprint
			first, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunErrorStatus(first.ChildID, "daemon restarted", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			if _, err := database.sql.Exec(`UPDATE runs SET last_pushed_sha = ?, push_generation = 2 WHERE id = ?`, first.Request.CandidateSHA, first.ChildID); err != nil {
				t.Fatal(err)
			}
			child, err := database.ReadTerminalSnapshot(first.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Snapshot, proposal.Request.PredecessorID, proposal.Request.SnapshotDigest = *child, first.ChildID, child.Digest()
			proposal.Request.LaunchNonce = *child.Run.LaunchNonce
			proposal.Request.PublishedHeadSHA, proposal.Request.PushGeneration = *child.Run.LastPushedSHA, *child.Run.PushGeneration
			proposal.Request.TerminalGeneration, proposal.Request.AcceptUnknownSource = "terminal-retry", true
			proposal.Request.SourceAnchors = map[string]string{
				"caller/refs/no-mistakes/recover-local/" + rootID:            strings.Repeat("6", 40),
				"gate/refs/no-mistakes/recover-local/" + rootID:              strings.Repeat("7", 40),
				"caller/refs/no-mistakes/recover-rewritten/" + rootID + "/1": strings.Repeat("8", 40),
			}
			assertion := *proposal.Request.LaunchAssertion
			assertion.Apply = true
			proposal.Request.LaunchAssertion = &assertion
			proposal.AssertionProof, err = assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			terminal, _, err := database.AcceptValidationAdmission(context.Background(), proposal)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunErrorStatus(terminal.ChildID, "daemon restarted", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			child, err = database.ReadTerminalSnapshot(terminal.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Snapshot, proposal.Request.PredecessorID, proposal.Request.SnapshotDigest = *child, terminal.ChildID, child.Digest()
			proposal.Request.LaunchNonce, proposal.Request.ValidationGeneration = *child.Run.LaunchNonce, *child.Run.LaunchValidationGeneration
			proposal.Request.TerminalGeneration, proposal.Request.AcceptUnknownSource, proposal.Request.SourceAnchors = "", false, nil
			proposal.Request.CandidateSHA = test.candidate
			before, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil {
				t.Fatal(err)
			}

			event, _, err := database.AcceptValidationAdmission(context.Background(), proposal)

			if test.shouldAdmit {
				if err != nil || event == nil {
					t.Fatalf("unbound candidate refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "family") {
				t.Fatalf("earlier %s head admitted: %v", test.name, err)
			}
			after, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed runs: %v", err)
			}
		})
	}
}

func TestValidationAdmissionRejectsLinkedAdoptionHeads(t *testing.T) {
	for _, candidate := range []string{strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40), strings.Repeat("4", 40)} {
		t.Run(candidate[:1], func(t *testing.T) {
			database, proposal := admissionFixture(t)
			source, err := database.ReadContinuationSnapshot(proposal.Request.PredecessorID)
			if err != nil {
				t.Fatal(err)
			}
			adoption, err := database.AdoptTerminalCandidate(context.Background(), TerminalAdoption{
				Snapshot: *source, CandidateSHA: proposal.Request.CandidateSHA, CandidateTree: proposal.Request.CandidateTree,
				RetainedSHA: strings.Repeat("1", 40), GateHead: strings.Repeat("2", 40), RecoveryHead: strings.Repeat("3", 40), CallerHead: strings.Repeat("4", 40),
				WorktreeRoot: proposal.WorktreeRoot, Reason: "accept retained source", Operator: "fixture", Assertion: source.Run.LaunchAssertion, Proof: source.Run.LaunchAssertionProof,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(adoption.SuccessorID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(adoption.SuccessorID)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Snapshot, proposal.Request.PredecessorID, proposal.Request.SnapshotDigest = *snapshot, snapshot.Run.ID, snapshot.Digest()
			proposal.Request.LaunchNonce, proposal.Request.CandidateSHA = *snapshot.Run.LaunchNonce, candidate
			before, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil {
				t.Fatal(err)
			}

			_, _, err = database.AcceptValidationAdmission(context.Background(), proposal)

			if err == nil || !strings.Contains(err.Error(), "head bound in the validation family") {
				t.Fatalf("adoption-bound head admitted: %v", err)
			}
			after, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed runs: %v", err)
			}
		})
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
			if spending := family.Spending[types.StepReview]; spending.Automatic != 1 || spending.Manual != 2 {
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
