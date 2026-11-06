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

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
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
	params.Request.SourceAnchors, err = branchsync.TerminalValidationAnchors(context.Background(), gate, repository.WorkingPath, parent)
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
	request.CallerDir, err = filepath.EvalSymlinks(repo.WorkingPath)
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
