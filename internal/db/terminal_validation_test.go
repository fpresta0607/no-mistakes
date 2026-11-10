package db

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalValidationAdmitsTheUnchangedReturnedCandidateWithFreshSelection(t *testing.T) {
	database, proposal := admissionFixture(t)
	if err := database.SetRunsCustodyReturned([]string{proposal.Request.PredecessorID}); err != nil {
		t.Fatal(err)
	}
	proposal.Request.CandidateSHA = proposal.Snapshot.Run.HeadSHA
	payload, err := json.Marshal(proposal.Request)
	if err != nil {
		t.Fatal(err)
	}
	request, err := authoredvalidation.Decode(payload)
	if err != nil {
		t.Fatalf("explicit terminal request: %v", err)
	}
	request.TerminalGeneration, request.AcceptUnknownSource = "replacement-lane", true
	request.SourceAnchors = map[string]string{}
	request.LaunchAssertion.Apply = true
	for role, chain := range request.LaunchAssertion.Profiles {
		chain[0].Model = "available-successor"
		request.LaunchAssertion.Profiles[role] = chain
	}
	proposal.Request = *request
	proposal.AssertionProof, err = request.LaunchAssertion.Verify(request.LaunchAssertion.TrustedSHA, request.LaunchAssertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}

	event, created, err := database.AcceptValidationAdmission(context.Background(), proposal)

	if err != nil || !created {
		t.Fatalf("unchanged terminal admission: %v %v", created, err)
	}
	parent, err := database.ReadTerminalSnapshot(request.PredecessorID)
	if err != nil || !reflect.DeepEqual(parent, &proposal.Snapshot) {
		t.Fatalf("terminal predecessor changed: %v", err)
	}
	child, err := database.GetRun(event.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if child.HeadSHA != parent.Run.HeadSHA || child.LaunchValidationGeneration == nil || *child.LaunchValidationGeneration != "replacement-lane" || child.LaunchNonce == nil || *child.LaunchNonce == request.LaunchNonce || !child.LaunchAssertion.Matches(request.LaunchAssertion) || child.PiProfile != nil || child.QuotaRouting != "" || child.ReviewApprovedHeadSHA != nil || child.CIReadyAt != nil {
		t.Fatalf("successor inherited authority or lost fresh selection: %+v", child)
	}
	family, err := database.ValidationFamily(child.ID)
	if err != nil || family == nil || family.HasChildRepairs || family.Spending[types.StepReview].Automatic != 1 || family.Spending[types.StepReview].Manual != 2 {
		t.Fatalf("cumulative spending: %+v %v", family, err)
	}
}

func TestTerminalValidationPreservesSourceAdoptionSpendingAndRefusesUnsafeAncestors(t *testing.T) {
	for _, hasNativeRefusal := range []bool{false, true} {
		t.Run(fmt.Sprint(hasNativeRefusal), func(t *testing.T) {
			database, proposal := admissionFixture(t)
			if hasNativeRefusal {
				if err := database.SetStepFindings(proposal.Snapshot.Steps[0].ID, `{"findings":[{"id":"`+types.FindingIDAgentSafetyRefusal+`","description":"native safety refusal"}]}`); err != nil {
					t.Fatal(err)
				}
			}
			source, err := database.ReadContinuationSnapshot(proposal.Request.PredecessorID)
			if err != nil {
				t.Fatal(err)
			}
			adoption, _, err := database.AdoptTerminalCandidate(context.Background(), TerminalAdoption{Snapshot: *source, CandidateSHA: source.Run.HeadSHA, CandidateTree: proposal.Request.CandidateTree, SourceRef: "refs/no-mistakes/recover/" + source.Run.ID, WorktreeRoot: proposal.WorktreeRoot, Reason: "accept retained source", Operator: "fixture", Assertion: source.Run.LaunchAssertion, Proof: source.Run.LaunchAssertionProof})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(adoption.Successor.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(adoption.Successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Snapshot = *snapshot
			proposal.Request.PredecessorID, proposal.Request.SnapshotDigest = snapshot.Run.ID, snapshot.Digest()
			proposal.Request.CandidateSHA, proposal.Request.LaunchNonce = snapshot.Run.HeadSHA, *snapshot.Run.LaunchNonce
			proposal.Request.TerminalGeneration, proposal.Request.AcceptUnknownSource = "source-adoption-successor", true
			proposal.Request.LaunchAssertion = &launchassert.Expectation{TrustedSHA: snapshot.Run.LaunchAssertion.TrustedSHA, Profiles: snapshot.Run.LaunchAssertion.Profiles, Apply: true}
			proposal.AssertionProof, err = proposal.Request.LaunchAssertion.Verify(proposal.Request.LaunchAssertion.TrustedSHA, proposal.Request.LaunchAssertion.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			before, err := database.GetRunsByRepo(proposal.Request.RepoID)
			if err != nil {
				t.Fatal(err)
			}

			event, _, err := database.AcceptValidationAdmission(context.Background(), proposal)

			if hasNativeRefusal {
				if err == nil {
					t.Fatal("admitted a native safety-refusal descendant")
				}
				after, readErr := database.GetRunsByRepo(proposal.Request.RepoID)
				if readErr != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("unsafe admission was not atomic: %v", readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			family, err := database.ValidationFamily(event.ChildID)
			if err != nil || family.HasChildRepairs || family.Spending[types.StepReview].Automatic != 1 || family.Spending[types.StepReview].Manual != 2 {
				t.Fatalf("source ancestry spending: %+v %v", family, err)
			}
			history, err := database.ValidationHistory(event.ChildID)
			if err != nil || len(history) != 3 || history[0].Snapshot.Run.ID != event.ChildID || history[0].Admission == nil || history[1].Snapshot.Run.ID != adoption.SuccessorID || history[1].Adoption == nil || history[1].Adoption.GateHead != adoption.GateHead || history[2].Snapshot.Run.ID != source.Run.ID {
				t.Fatalf("validation and source adoption must share one verified ancestry: %+v %v", history, err)
			}
			if err := database.UpdateRunHeadSHA(source.Run.ID, proposal.Request.CandidateTree); err == nil {
				t.Fatal("source predecessor mutation was allowed")
			}
			after, err := database.ValidationHistory(event.ChildID)
			if err != nil || !reflect.DeepEqual(history, after) {
				t.Fatalf("refused source mutation changed verified ancestry: %v", err)
			}
		})
	}
}
