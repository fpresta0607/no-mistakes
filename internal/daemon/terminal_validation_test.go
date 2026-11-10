package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func terminalValidationFixture(t *testing.T, factory StepFactory) (*RunManager, *db.DB, *db.Repo, ipc.ValidateAuthoredParams, string) {
	t.Helper()
	manager, database, repository, params, gate := authoredAdmissionFixture(t, factory)
	parent, err := database.GetRun(params.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repository.WorkingPath, "checkout", "-B", "feature", parent.HeadSHA)
	if err := database.SetRunsCustodyReturned([]string{parent.ID}); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, gate, "update-ref", custody.RecoveryRef(parent.ID), parent.HeadSHA)
	params.Request.CandidateSHA = parent.HeadSHA
	params.Request.CandidateTree = gitOutput(t, repository.WorkingPath, "rev-parse", "HEAD^{tree}")
	params.Request.TerminalGeneration = "independent-terminal-lane"
	params.Request.AcceptUnknownSource = true
	params.Request.LaunchAssertion = launchSelection(params.Request.LaunchAssertion.TrustedSHA, selectedCodex)
	params.Request.SourceAnchors, err = branchsync.TerminalValidationAnchors(context.Background(), database, gate, repository.WorkingPath, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return manager, database, repository, params, gate
}

func TestTerminalValidationRunsEveryStageOnTheUnchangedReturnedCandidate(t *testing.T) {
	var calls atomic.Int32
	manager, database, repo, params, gate := terminalValidationFixture(t, func() []pipeline.Step {
		var result []pipeline.Step
		for _, step := range steps.AllSteps() {
			result = append(result, &authoredFixtureStep{name: step.Name(), calls: &calls})
		}
		return result
	})
	before, err := database.GetRun(params.Request.PredecessorID)
	if err != nil || before.TerminalSourceState != nil || before.CustodyReturnedAt == nil {
		t.Fatalf("fixture must have returned custody and unknown historical source: %+v %v", before, err)
	}

	result, err := manager.validateAuthored(context.Background(), params, os.Getpid())

	if err != nil || !result.Started {
		t.Fatalf("terminal admission: %+v %v", result, err)
	}
	child := waitForRunTerminalState(t, database, result.Receipt.RunID)
	manager.wg.Wait()
	if child.Status != types.RunCompleted || child.HeadSHA != before.HeadSHA || calls.Load() != int32(len(steps.AllSteps())) {
		t.Fatalf("fresh stages: %+v calls=%d", child, calls.Load())
	}
	after, err := database.GetRun(before.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("predecessor mutated: %v", err)
	}
	if gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD") != before.HeadSHA || gitOutput(t, gate, "rev-parse", custody.RecoveryRef(before.ID)) != before.HeadSHA {
		t.Fatal("source or recovery anchor changed")
	}
	replay, err := manager.validateAuthored(context.Background(), params, os.Getpid())
	if err != nil || replay.Started || replay.Receipt.RunID != child.ID || calls.Load() != int32(len(steps.AllSteps())) {
		t.Fatalf("replay renewed validation: %+v %v", replay, err)
	}
}

func TestTerminalValidationReplacesAnExhaustedProfileWithoutRewritingItsReceipt(t *testing.T) {
	manager, root, database, repo, head, claudeMarker, codexMarker := selectionFixture(t, "codex", false, false)
	gate := root.RepoDir(repo.ID)
	gitCmd(t, gate, "config", "url."+filepath.ToSlash(gate)+".insteadOf", repo.UpstreamURL)
	gitCmd(t, gate, "remote", "set-url", "origin", repo.UpstreamURL)
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	assertion := launchSelection(head, selectedPi)
	proof, err := assertion.Verify(head, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := db.RunIntent{Summary: "validate the unchanged candidate", Source: db.RunIntentSourceAgent, Score: 1}
	parent, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", head, head, &intent, "quota-predecessor", "quota-generation", fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary))), "", true, nil, assertion, proof, &agentcfg.PiProfile{Model: selectedPi.Model, Effort: selectedPi.Effort})
	if err != nil {
		t.Fatal(err)
	}
	pinIntentCheck(t, database, parent.ID)
	if err := persistQuotaRoutingRecord(database, parent.ID, quotarouting.NewFailureRecord(false, []types.AgentName{types.AgentPi}, "provider quota exhausted")); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatus(parent.ID, "provider quota exhausted", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{Branch: "feature", TerminalGeneration: "available-harness-lane", AcceptUnknownSource: true, HistoricalSourceCompleteness: "unknown", Reason: "explicitly validate the committed candidate with unknown historical source", ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("a", 64), Class: "ordinary", ReviewCycles: 2}}
	request.PredecessorID, request.SnapshotDigest, request.RepoID = parent.ID, snapshot.Digest(), repo.ID
	request.CallerDir, err = realpath.Resolve(repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	request.CandidateSHA, request.CandidateTree = head, gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD^{tree}")
	request.LaunchNonce, request.ValidationGeneration, request.IntentDigest = *snapshot.Run.LaunchNonce, *snapshot.Run.LaunchValidationGeneration, *snapshot.Run.LaunchIntentDigest
	request.LaunchAssertion = launchSelection(head, selectedClaude)
	request.SourceAnchors = nil

	result, err := manager.validateAuthored(context.Background(), ipc.ValidateAuthoredParams{Request: request, CallerDir: request.CallerDir}, os.Getpid())

	if err != nil || !result.Started {
		t.Fatalf("explicit replacement selection: %+v %v", result, err)
	}
	child := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if child.Status != types.RunCompleted || child.LaunchAssertionProof.Check(request.LaunchAssertion) != nil || child.PiProfile != nil || child.QuotaRouting != "" || snapshot.Run.PiProfile == nil || snapshot.Run.QuotaRouting == "" || len(markerLines(t, claudeMarker)) != 3 || len(markerLines(t, codexMarker)) != 0 {
		t.Fatalf("successor did not use only the selected available harness: %+v", child)
	}
	after, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil || !reflect.DeepEqual(snapshot, after) {
		t.Fatalf("quota predecessor receipt/history changed: %v", err)
	}
}

func TestTerminalValidationRefusesChangedBindingsWithoutAdmission(t *testing.T) {
	expectedRefusals := map[string]string{
		"source acceptance": "explicit unknown-source", "generation": "distinct generation", "candidate": "exact uncompleted predecessor head",
		"tree": "tree differs", "snapshot": "predecessor snapshot", "dirty": "must be clean", "anchor missing": "recovery anchors changed",
		"anchor moved": "contradicts", "symbolic anchor": "is symbolic", "competing run": "newer or active run",
		"native safety refusal": "native agent safety refusal", "unreadable findings": "cannot inspect predecessor", "trusted source": "trusted source differs", "selection proof": "profiles differ",
		"queued predecessor": "no executor or publisher", "active publisher": "no active publisher",
	}
	for _, change := range []string{"source acceptance", "generation", "candidate", "tree", "snapshot", "dirty", "anchor missing", "anchor moved", "symbolic anchor", "competing run", "native safety refusal", "unreadable findings", "trusted source", "selection proof", "queued predecessor", "active publisher"} {
		t.Run(change, func(t *testing.T) {
			manager, database, repo, params, gate := terminalValidationFixture(t, nil)
			switch change {
			case "source acceptance":
				params.Request.AcceptUnknownSource = false
			case "queued predecessor":
				manager.mu.Lock()
				manager.runQueue = append(manager.runQueue, queuedRun{runID: params.Request.PredecessorID, spawn: func() {}})
				manager.mu.Unlock()
				defer func() {
					manager.mu.Lock()
					manager.runQueue = nil
					manager.mu.Unlock()
				}()
			case "active publisher":
				if err := database.SetRunPushActive(params.Request.PredecessorID, true); err != nil {
					t.Fatal(err)
				}
				snapshot, err := database.ReadTerminalSnapshot(params.Request.PredecessorID)
				if err != nil {
					t.Fatal(err)
				}
				params.Request.SnapshotDigest = snapshot.Digest()
			case "generation":
				params.Request.TerminalGeneration = params.Request.ValidationGeneration
			case "candidate":
				params.Request.CandidateSHA = strings.Repeat("a", 40)
			case "tree":
				params.Request.CandidateTree = strings.Repeat("a", 40)
			case "snapshot":
				params.Request.SnapshotDigest = strings.Repeat("a", 64)
			case "dirty":
				if err := os.WriteFile(filepath.Join(repo.WorkingPath, "untracked"), []byte("retained"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "anchor missing":
				gitCmd(t, gate, "update-ref", "-d", custody.RecoveryRef(params.Request.PredecessorID))
			case "anchor moved":
				gitCmd(t, gate, "update-ref", custody.RecoveryRef(params.Request.PredecessorID), gitOutput(t, gate, "rev-parse", params.Request.CandidateSHA+"^{tree}"))
			case "symbolic anchor":
				gitCmd(t, gate, "symbolic-ref", custody.RecoveryRef(params.Request.PredecessorID), "refs/heads/feature")
			case "competing run":
				if _, err := database.InsertRun(repo.ID, "feature", params.Request.CandidateSHA, params.Request.CandidateSHA); err != nil {
					t.Fatal(err)
				}
			case "native safety refusal", "unreadable findings":
				step, err := database.InsertStepResult(params.Request.PredecessorID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[{"id":"` + types.FindingIDAgentSafetyRefusal + `","description":"native refusal"}]}`
				if change == "unreadable findings" {
					findings = "{"
				}
				if err := database.SetStepFindings(step.ID, findings); err != nil {
					t.Fatal(err)
				}
				snapshot, err := database.ReadTerminalSnapshot(params.Request.PredecessorID)
				if err != nil {
					t.Fatal(err)
				}
				params.Request.SnapshotDigest = snapshot.Digest()
			case "trusted source":
				params.Request.LaunchAssertion.TrustedSHA = strings.Repeat("a", 40)
			case "selection proof":
				params.Request.LaunchAssertion.Profiles["primary"][0].ServiceTier = "priority"
			}
			before, err := database.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err = manager.validateAuthored(ctx, params, os.Getpid())

			if err == nil || !strings.Contains(err.Error(), expectedRefusals[change]) {
				t.Fatalf("wrong refusal for %s: %v", change, err)
			}
			after, readErr := database.GetRunsByRepo(repo.ID)
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed runs: %v", readErr)
			}
		})
	}
}

func TestTerminalValidationPreservesAdoptedGateOwnershipAndEveryBoundAnchor(t *testing.T) {
	for _, descendant := range []string{"none", "authored", "terminal", "adopted archive"} {
		changes := []string{"unchanged", "candidate moved", "candidate deleted", "ancestor recovery moved", "ancestor recovery deleted", "archive moved", "archive deleted", "symbolic candidate", "unrelated gate"}
		switch descendant {
		case "authored":
			changes = []string{"unchanged", "admission candidate moved", "admission candidate deleted"}
		case "terminal":
			changes = []string{"unchanged", "inherited recovery moved", "inherited recovery deleted", "admission candidate moved", "admission candidate deleted"}
		case "adopted archive":
			changes = []string{"unchanged", "adopted archive moved", "adopted archive deleted"}
		}
		for _, change := range changes {
			t.Run(descendant+"/"+change, func(t *testing.T) {
				var calls atomic.Int32
				manager, database, repo, params, gate := authoredAdmissionFixture(t, func() []pipeline.Step {
					var result []pipeline.Step
					for _, step := range steps.AllSteps() {
						result = append(result, &authoredFixtureStep{name: step.Name(), calls: &calls})
					}
					return result
				})
				review, err := database.InsertStepResult(params.Request.PredecessorID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				findings, selected := `{"findings":[{"id":"spent","description":"dispatched repair"}]}`, `["spent"]`
				round, err := database.InsertStepRound(review.ID, 1, "initial", &findings, nil, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
					t.Fatal(err)
				}
				original, err := database.ReadContinuationSnapshot(params.Request.PredecessorID)
				if err != nil {
					t.Fatal(err)
				}
				candidate := params.Request.CandidateSHA
				successorID := ulid.Make().String()
				candidateRef := "refs/no-mistakes/terminal-candidate/" + successorID
				recoveryRef := custody.RecoveryRef(original.Run.ID)
				archiveRef := "refs/heads/archive/retained-terminal-source"
				adoptedArchiveRef := "refs/heads/archive/adopted-terminal-source"
				localRecoveryRef := custody.RecoveryLocalRef(original.Run.ID)
				gitCmd(t, gate, "fetch", "--no-tags", repo.WorkingPath, candidate)
				gitCmd(t, gate, "update-ref", candidateRef, candidate)
				gitCmd(t, gate, "update-ref", recoveryRef, original.Run.HeadSHA)
				gitCmd(t, repo.WorkingPath, "update-ref", archiveRef, original.Run.HeadSHA)
				gitCmd(t, repo.WorkingPath, "update-ref", localRecoveryRef, original.Run.HeadSHA)
				if _, err := database.RecordRecoveryArchive(db.RecoveryArchive{RepoID: repo.ID, RunID: original.Run.ID, Branch: original.Run.Branch, RequiredHeadSHA: original.Run.HeadSHA, PreservedHeadSHA: original.Run.HeadSHA, ArchiveRef: archiveRef}); err != nil {
					t.Fatal(err)
				}
				adoption, err := database.AdoptTerminalCandidate(context.Background(), db.TerminalAdoption{Snapshot: *original, SuccessorID: successorID, CandidateSHA: candidate, CandidateTree: params.Request.CandidateTree, RetainedSHA: candidate, SourceRef: candidateRef, GateHead: original.Run.HeadSHA, RecoveryHead: original.Run.HeadSHA, CallerDir: params.CallerDir, CallerHead: original.Run.HeadSHA, Reason: "accept the retained committed correction", Operator: "fixture", Assertion: original.Run.LaunchAssertion, Proof: original.Run.LaunchAssertionProof})
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunErrorStatus(adoption.SuccessorID, "provider quota exhausted", types.RunFailed); err != nil {
					t.Fatal(err)
				}
				if descendant == "adopted archive" {
					gitCmd(t, repo.WorkingPath, "update-ref", adoptedArchiveRef, candidate)
					gitCmd(t, gate, "update-ref", custody.RecoveryRef(adoption.SuccessorID), candidate)
					if _, err := database.RecordRecoveryArchive(db.RecoveryArchive{RepoID: repo.ID, RunID: adoption.SuccessorID, Branch: original.Run.Branch, RequiredHeadSHA: original.Run.HeadSHA, PreservedHeadSHA: candidate, ArchiveRef: adoptedArchiveRef}); err != nil {
						t.Fatal(err)
					}
				}
				parentID := adoption.SuccessorID
				hasValidationDescendant := descendant == "authored" || descendant == "terminal"
				if hasValidationDescendant {
					snapshot, err := database.ReadTerminalSnapshot(parentID)
					if err != nil {
						t.Fatal(err)
					}
					if descendant == "authored" {
						if err := os.WriteFile(filepath.Join(repo.WorkingPath, "next.txt"), []byte("authored follow-up\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, repo.WorkingPath, "add", "next.txt")
						gitCmd(t, repo.WorkingPath, "commit", "-m", "author follow-up")
					} else {
						params.Request.TerminalGeneration, params.Request.AcceptUnknownSource = "first-terminal-lane", true
						params.Request.LaunchAssertion = launchSelection(params.Request.LaunchAssertion.TrustedSHA, selectedCodex)
						params.Request.SourceAnchors = map[string]string{
							"gate/" + candidateRef: candidate,
							"gate/" + recoveryRef:  original.Run.HeadSHA, "caller/" + archiveRef: original.Run.HeadSHA,
							"caller/" + localRecoveryRef: original.Run.HeadSHA,
						}
					}
					params.Request.PredecessorID, params.Request.SnapshotDigest = parentID, snapshot.Digest()
					params.Request.LaunchNonce = *snapshot.Run.LaunchNonce
					params.Request.CandidateSHA = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
					params.Request.CandidateTree = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD^{tree}")
					proof, err := params.Request.LaunchAssertion.Verify(params.Request.LaunchAssertion.TrustedSHA, params.Request.LaunchAssertion.Profiles)
					if err != nil {
						t.Fatal(err)
					}
					admission, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: params.Request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: filepath.Dir(manager.paths.WorktreeDir(repo.ID, "pending")), Gates: "[]", Steps: types.AllSteps(), Operator: "fixture"})
					if err != nil {
						t.Fatal(err)
					}
					gitCmd(t, gate, "fetch", "--no-tags", repo.WorkingPath, params.Request.CandidateSHA)
					gitCmd(t, gate, "update-ref", "refs/no-mistakes/authored-candidate/"+admission.ID, params.Request.CandidateSHA)
					if err := database.UpdateRunErrorStatus(admission.ChildID, "provider quota exhausted", types.RunFailed); err != nil {
						t.Fatal(err)
					}
					parentID = admission.ChildID
				}
				snapshot, err := database.ReadTerminalSnapshot(parentID)
				if err != nil {
					t.Fatal(err)
				}
				params.Request.PredecessorID, params.Request.SnapshotDigest = parentID, snapshot.Digest()
				params.Request.CandidateSHA, params.Request.LaunchNonce = snapshot.Run.HeadSHA, *snapshot.Run.LaunchNonce
				params.Request.ValidationGeneration = *snapshot.Run.LaunchValidationGeneration
				params.Request.TerminalGeneration, params.Request.AcceptUnknownSource = "independent-adopted-lane", true
				params.Request.LaunchAssertion = launchSelection(params.Request.LaunchAssertion.TrustedSHA, selectedCodex)
				params.Request.SourceAnchors = map[string]string{
					"gate/" + candidateRef: candidate,
					"gate/" + recoveryRef:  original.Run.HeadSHA, "caller/" + archiveRef: original.Run.HeadSHA,
					"caller/" + localRecoveryRef: original.Run.HeadSHA,
				}
				if descendant == "adopted archive" {
					params.Request.SourceAnchors["caller/"+adoptedArchiveRef] = candidate
					params.Request.SourceAnchors["gate/"+custody.RecoveryRef(adoption.SuccessorID)] = candidate
				}
				admissionCandidateRef := ""
				if hasValidationDescendant {
					admission, err := database.GetValidationPredecessor(parentID)
					if err != nil {
						t.Fatal(err)
					}
					admissionCandidateRef = "refs/no-mistakes/authored-candidate/" + admission.ID
					params.Request.SourceAnchors["gate/"+admissionCandidateRef] = snapshot.Run.HeadSHA
				}
				directory, ref, replacement := gate, candidateRef, original.Run.HeadSHA
				switch change {
				case "admission candidate moved", "admission candidate deleted":
					ref = admissionCandidateRef
				case "ancestor recovery moved", "ancestor recovery deleted":
					ref, replacement = recoveryRef, candidate
				case "archive moved", "archive deleted":
					directory, ref, replacement = repo.WorkingPath, archiveRef, candidate
				case "adopted archive moved", "adopted archive deleted":
					directory, ref, replacement = repo.WorkingPath, adoptedArchiveRef, original.Run.HeadSHA
				case "inherited recovery moved", "inherited recovery deleted":
					directory, ref, replacement = repo.WorkingPath, localRecoveryRef, candidate
				case "unrelated gate":
					ref, replacement = "refs/heads/feature", gitOutput(t, repo.WorkingPath, "commit-tree", params.Request.CandidateTree, "-m", "unrelated history")
					gitCmd(t, gate, "fetch", "--no-tags", repo.WorkingPath, replacement)
				}
				switch {
				case change == "symbolic candidate":
					gitCmd(t, gate, "symbolic-ref", candidateRef, "refs/heads/feature")
				case strings.HasSuffix(change, "deleted"):
					gitCmd(t, directory, "update-ref", "-d", ref)
					name := "gate"
					if directory == repo.WorkingPath {
						name = "caller"
					}
					delete(params.Request.SourceAnchors, name+"/"+ref)
				case change != "unchanged":
					gitCmd(t, directory, "update-ref", ref, replacement)
					name := "gate"
					if directory == repo.WorkingPath {
						name = "caller"
					}
					if ref != "refs/heads/feature" {
						params.Request.SourceAnchors[name+"/"+ref] = replacement
					}
				}
				before, err := database.GetRunsByRepo(repo.ID)
				if err != nil {
					t.Fatal(err)
				}

				result, err := manager.validateAuthored(context.Background(), params, os.Getpid())

				if change != "unchanged" {
					expected := "terminal recovery anchor"
					if change == "unrelated gate" {
						expected = "gate branch moved outside the immutable predecessor lineage"
					}
					if err == nil || !strings.Contains(err.Error(), expected) {
						t.Fatalf("wrong refusal for changed bound provenance: %+v %v", result, err)
					}
					if strings.HasPrefix(change, "admission candidate") && !strings.Contains(err.Error(), admissionCandidateRef) {
						t.Fatalf("refusal did not identify the changed admission candidate ref: %v", err)
					}
					after, readErr := database.GetRunsByRepo(repo.ID)
					if readErr != nil || !reflect.DeepEqual(before, after) || calls.Load() != 0 {
						t.Fatalf("refusal mutated history or launched a stage: %v", readErr)
					}
					return
				}
				if err != nil || !result.Started {
					t.Fatalf("retained adoption candidate refused: %+v %v", result, err)
				}
				if spending := result.Family.Spending[types.StepReview]; spending.Automatic != 1 || spending.Manual != 0 {
					t.Fatalf("adoption ancestry renewed spent repair capacity: %+v", spending)
				}
				child := waitForRunTerminalState(t, database, result.Receipt.RunID)
				manager.wg.Wait()
				if child.Status != types.RunCompleted || child.HeadSHA != snapshot.Run.HeadSHA || calls.Load() != int32(len(steps.AllSteps())) {
					t.Fatalf("candidate did not receive fresh validation: %+v calls=%d", child, calls.Load())
				}
				for _, previous := range before {
					after, err := database.GetRun(previous.ID)
					if err != nil || !reflect.DeepEqual(previous, after) {
						t.Fatalf("admission rewrote predecessor %s: %v", previous.ID, err)
					}
				}
				if gitOutput(t, gate, "rev-parse", "refs/heads/feature") != original.Run.HeadSHA || gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD") != snapshot.Run.HeadSHA {
					t.Fatal("admission moved the preserved gate or caller branch")
				}
			})
		}
	}
}
