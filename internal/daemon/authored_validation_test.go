package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type authoredFixtureStep struct {
	name  types.StepName
	calls *atomic.Int32
}

func TestAuthoredPublicationProofChecksLiveProviderFactsAndRefusesRaces(t *testing.T) {
	_, database, repo, request, gate := authoredAdmissionFixture(t, nil)
	parent, err := database.GetRun(request.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "config", "url."+filepath.ToSlash(gate)+".insteadOf", repo.UpstreamURL)
	prURL := "https://github.com/test/repo/pull/17"
	publicHead, ref, fingerprint, kind, state := parent.HeadSHA, "refs/heads/feature", branchsync.TargetFingerprint(repo.PushURL()), "upstream", "open"
	generation, publishedAt := int64(1), int64(1)
	parent.PRURL, parent.PRState, parent.LastPushedSHA, parent.PushRef, parent.PushTargetFingerprint, parent.PushTargetKind, parent.PushGeneration, parent.LastPushedAt = &prURL, &state, &publicHead, &ref, &fingerprint, &kind, &generation, &publishedAt
	originalOutput := authoredPRCommandOutput
	t.Cleanup(func() { authoredPRCommandOutput = originalOutput })
	for _, change := range []string{"matching", "generation", "fingerprint", "kind", "ref", "closed PR", "repository", "head", "branch", "base", "provider failure", "head racing PR"} {
		t.Run(change, func(t *testing.T) {
			candidate := *parent
			provider := fmt.Sprintf(`{"html_url":%q,"state":"open","head":{"sha":%q,"ref":"feature","repo":{"full_name":"test/repo"}},"base":{"ref":"main","repo":{"full_name":"test/repo"}}}`, prURL, publicHead)
			switch change {
			case "generation":
				value := int64(0)
				candidate.PushGeneration = &value
			case "fingerprint":
				value := "other"
				candidate.PushTargetFingerprint = &value
			case "kind":
				value := "fork"
				candidate.PushTargetKind = &value
			case "ref":
				value := "refs/heads/other"
				candidate.PushRef = &value
			case "closed PR":
				provider = strings.Replace(provider, `"state":"open"`, `"state":"closed"`, 1)
			case "repository":
				provider = strings.ReplaceAll(provider, `"full_name":"test/repo"`, `"full_name":"other/repo"`)
			case "head":
				provider = strings.Replace(provider, publicHead, request.Request.CandidateSHA, 1)
			case "branch":
				provider = strings.Replace(provider, `"ref":"feature"`, `"ref":"other"`, 1)
			case "base":
				provider = strings.Replace(provider, `"ref":"main"`, `"ref":"other"`, 1)
			}
			authoredPRCommandOutput = func(command *exec.Cmd) ([]byte, error) {
				if !strings.Contains(strings.Join(command.Args, " "), "repos/test/repo/pulls/17") {
					t.Fatalf("queried an unrelated provider target: %v", command.Args)
				}
				if change == "provider failure" {
					return nil, errors.New("fixture provider refused")
				}
				if change == "head racing PR" {
					gitCmd(t, gate, "fetch", "--no-tags", repo.WorkingPath, request.Request.CandidateSHA)
					gitCmd(t, gate, "update-ref", ref, request.Request.CandidateSHA)
					t.Cleanup(func() { gitCmd(t, gate, "update-ref", ref, publicHead) })
				}
				return []byte(provider), nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := proveAuthoredPublication(ctx, &candidate, repo, repo.WorkingPath, &config.Config{}, nil)
			if (change == "matching") != (err == nil) {
				t.Fatalf("publication proof %s: %v", change, err)
			}
		})
	}
}

func TestAuthoredAdmissionRunsNewTrustedCustomGates(t *testing.T) {
	var calls atomic.Int32
	factory := func() []pipeline.Step {
		var result []pipeline.Step
		for _, step := range steps.AllSteps() {
			result = append(result, &authoredFixtureStep{name: step.Name(), calls: &calls})
		}
		return result
	}
	manager, database, repo, request, _ := authoredAdmissionFixture(t, factory)
	newTrusted := commitDefaultBranchConfig(t, repo.WorkingPath, strings.Replace(oneGateYAML, `command: "true"`, `command: "exit 0"`, 1))
	request.Request.LaunchAssertion.TrustedSHA = newTrusted
	request.Request.CandidateSHA = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	request.Request.CandidateTree = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD^{tree}")
	result, err := manager.validateAuthored(context.Background(), request, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	child := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if child.Status != types.RunCompleted {
		t.Fatalf("fresh trusted gate did not pass: %+v", child)
	}
	rows, err := database.GetStepsByRun(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	var hasFreshGate bool
	for _, step := range rows {
		if step.StepName == "gate.review.arch-fitness" {
			hasFreshGate = step.Status == types.StepStatusCompleted
		}
	}
	if !hasFreshGate || len(rows) != len(steps.AllSteps())+1 {
		t.Fatalf("new trusted gate was dropped: %+v", rows)
	}
}

func TestAuthoredPendingAdmissionStaysIdleAcrossStartupAndCanBeCancelled(t *testing.T) {
	var calls atomic.Int32
	manager, database, repo, request, _ := authoredAdmissionFixture(t, func() []pipeline.Step {
		return []pipeline.Step{&authoredFixtureStep{name: types.StepReview, calls: &calls}}
	})
	snapshot, err := database.ReadTerminalSnapshot(request.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := request.Request.LaunchAssertion.Verify(request.Request.LaunchAssertion.TrustedSHA, request.Request.LaunchAssertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request.Request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: filepath.Dir(manager.paths.WorktreeDir(repo.ID, "pending")), Gates: "[]", Steps: []types.StepName{types.StepReview}, Operator: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(manager.paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	layout, err := validatedWorktreeLayout(database, manager.paths, global)
	if err != nil {
		t.Fatal(err)
	}
	recoverOnStartup(database, manager.paths, manager, layout)
	child, err := database.GetRun(event.ChildID)
	if err != nil || child.Status != types.RunPending || calls.Load() != 0 {
		t.Fatalf("startup ran or terminalized untouched acceptance: %+v calls=%d error=%v", child, calls.Load(), err)
	}
	subscription, err := manager.Subscribe(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	eventContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	subscription.Next(eventContext)
	if err := manager.HandleCancel(child.ID); err != nil {
		t.Fatalf("cancel pending authored acceptance: %v", err)
	}
	cancelEvent, ok := subscription.Next(eventContext)
	if !ok || cancelEvent.Type != ipc.EventRunCompleted || cancelEvent.Status == nil || *cancelEvent.Status != string(types.RunCancelled) || cancelEvent.Error == nil || *cancelEvent.Error != types.RunCancelReasonAbortedByUser {
		t.Fatalf("pending cancel event = %#v, open = %v", cancelEvent, ok)
	}
	if _, ok := subscription.Next(eventContext); ok {
		t.Fatal("cancelled pending acceptance left its subscription open")
	}
	repeated, err := manager.validateAuthored(context.Background(), request, os.Getpid())
	if err != nil || repeated.Started || repeated.Receipt.RunID != child.ID || calls.Load() != 0 {
		t.Fatalf("cancelled child restarted: %+v %v", repeated, err)
	}
}

func TestAuthoredAdmissionContinuesAnUnpublishedCancelledChild(t *testing.T) {
	var calls atomic.Int32
	manager, database, repo, request, gate := authoredAdmissionFixture(t, func() []pipeline.Step {
		return []pipeline.Step{&authoredFixtureStep{name: types.StepReview, calls: &calls}}
	})
	gateHead := gitOutput(t, gate, "rev-parse", "refs/heads/feature")
	if err := database.UpdateRunHeadSHA(request.Request.PredecessorID, request.Request.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(request.Request.PredecessorID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings, selected := `{"findings":[{"id":"r1","description":"repair"}]}`, `["r1"]`
	for index, source := range []string{db.RoundSelectionSourceAutoFix, db.RoundSelectionSourceUser} {
		round, err := database.InsertStepRound(step.ID, index*2+1, "initial", &findings, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.SetStepRoundSelection(round.ID, &selected, source); err != nil {
			t.Fatal(err)
		}
		if _, err := database.InsertStepRound(step.ID, index*2+2, "auto_fix", nil, nil, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "authored.txt"), []byte("first new authored candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "author next candidate")
	request.Request.CandidateSHA = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	request.Request.CandidateTree = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD^{tree}")
	rootSnapshot, err := database.ReadTerminalSnapshot(request.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	request.Request.SnapshotDigest = rootSnapshot.Digest()
	proof, err := request.Request.LaunchAssertion.Verify(request.Request.LaunchAssertion.TrustedSHA, request.Request.LaunchAssertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request.Request, Snapshot: *rootSnapshot, AssertionProof: proof, WorktreeRoot: filepath.Dir(manager.paths.WorktreeDir(repo.ID, "pending")), Gates: "[]", Steps: []types.StepName{types.StepReview}, Operator: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.HandleCancel(event.ChildID); err != nil {
		t.Fatal(err)
	}
	childSnapshot, err := database.ReadTerminalSnapshot(event.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if childSnapshot.Run.Status != types.RunCancelled || childSnapshot.Run.LastPushedSHA != nil || childSnapshot.Run.HeadSHA == gateHead {
		t.Fatal("fixture did not leave an unpublished cancelled child")
	}
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "authored.txt"), []byte("second new authored candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "author descendant candidate")
	request.Request.PredecessorID, request.Request.SnapshotDigest, request.Request.LaunchNonce = event.ChildID, childSnapshot.Digest(), *childSnapshot.Run.LaunchNonce
	request.Request.CandidateSHA = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	request.Request.CandidateTree = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD^{tree}")

	result, err := manager.validateAuthored(context.Background(), request, os.Getpid())

	if err != nil || !result.Started {
		t.Fatalf("unchanged historical gate rejected an unpublished predecessor: %+v %v", result, err)
	}
	child := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if child.Status != types.RunCompleted || calls.Load() != 1 {
		t.Fatalf("descendant was not freshly validated: %+v calls=%d", child, calls.Load())
	}
	spending := result.Family.Spending[types.StepReview]
	if spending.Automatic != 1 || spending.Manual != 1 || spending.Remaining != 0 || result.Family.HasChildRepairs || result.Family.PolicyOrigin != "controller_assertion" || result.Family.HistoricalSourceCompleteness != "unknown" {
		t.Fatalf("descendant renewed spending or provenance: %+v", result.Family)
	}
	for _, before := range []*db.TerminalSnapshot{rootSnapshot, childSnapshot} {
		after, err := database.ReadTerminalSnapshot(before.Run.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("immutable predecessor changed: %v", err)
		}
	}
	if gitOutput(t, gate, "rev-parse", "refs/heads/feature") != gateHead || gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD") != request.Request.CandidateSHA {
		t.Fatal("continuation overwrote gate or author source")
	}
}

func TestAuthoredAdmissionAcceptsTheSubmittedHeadAfterUnpublishedPredecessorChanges(t *testing.T) {
	var calls atomic.Int32
	manager, database, repo, request, gate := authoredAdmissionFixture(t, func() []pipeline.Step {
		return []pipeline.Step{&authoredFixtureStep{name: types.StepReview, calls: &calls}}
	})
	gateHead := gitOutput(t, gate, "rev-parse", "refs/heads/feature")
	if err := database.UpdateRunHeadSHA(request.Request.PredecessorID, request.Request.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "authored.txt"), []byte("authored after unpublished repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "author fresh candidate")
	request.Request.CandidateSHA = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	request.Request.CandidateTree = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD^{tree}")
	snapshot, err := database.ReadTerminalSnapshot(request.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	request.Request.SnapshotDigest = snapshot.Digest()

	result, err := manager.validateAuthored(context.Background(), request, os.Getpid())

	if err != nil || !result.Started {
		t.Fatalf("ledger-bound submitted gate head was rejected: %+v %v", result, err)
	}
	child := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if child.Status != types.RunCompleted || gitOutput(t, gate, "rev-parse", "refs/heads/feature") != gateHead {
		t.Fatal("validation failed or moved the unchanged gate")
	}
}

func TestAuthoredAdmissionRefusesGateMovementDuringAdmission(t *testing.T) {
	var gate, candidate string
	manager, database, repo, request, gateDir := authoredAdmissionFixture(t, func() []pipeline.Step {
		gitCmd(t, gate, "update-ref", "refs/heads/feature", candidate)
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	gate, candidate = gateDir, request.Request.CandidateSHA

	_, err := manager.validateAuthored(context.Background(), request, os.Getpid())

	if err == nil || !strings.Contains(err.Error(), "gate branch changed during authored admission") {
		t.Fatalf("gate race was accepted: %v", err)
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 1 || gitOutput(t, gate, "rev-parse", "refs/heads/feature") != candidate {
		t.Fatalf("gate race rewrote history or discarded its owner's head: %+v %v", runs, err)
	}
}

func (step *authoredFixtureStep) Name() types.StepName { return step.name }
func (step *authoredFixtureStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	step.calls.Add(1)
	return &pipeline.StepOutcome{}, nil
}

func authoredAdmissionFixture(t *testing.T, factory StepFactory) (*RunManager, *db.DB, *db.Repo, ipc.ValidateAuthoredParams, string) {
	t.Helper()
	manager, root, database, repo, head, _, _ := launchAssertionFixture(t, factory)
	gitCmd(t, root.RepoDir(repo.ID), "config", "url."+filepath.ToSlash(root.RepoDir(repo.ID))+".insteadOf", repo.UpstreamURL)
	gitCmd(t, root.RepoDir(repo.ID), "remote", "set-url", "origin", repo.UpstreamURL)
	assertion := launchExpectation(head)
	proof, err := assertion.Verify(head, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := db.RunIntent{Summary: "Validate the declared authored source", Source: db.RunIntentSourceAgent, Score: 1}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
	parent, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", head, head, &intent, "original-launch", "original-generation", digest, "", true, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatus(parent.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "authored.txt"), []byte("declared candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "authored.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "author correction")
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := filepath.EvalSymlinks(repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repo.ID, Branch: "feature", CallerDir: caller,
		CandidateSHA: gitOutput(t, caller, "rev-parse", "HEAD"), CandidateTree: gitOutput(t, caller, "rev-parse", "HEAD^{tree}"), LaunchNonce: "original-launch", ValidationGeneration: "original-generation", IntentDigest: digest,
		ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("a", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate this authored candidate without certifying historical lost source"}
	return manager, database, repo, ipc.ValidateAuthoredParams{Request: request, CallerDir: caller}, root.RepoDir(repo.ID)
}

func TestAuthoredAdmissionSchedulesEveryStageFreshWithoutMovingSourceOrPredecessor(t *testing.T) {
	var calls atomic.Int32
	factory := func() []pipeline.Step {
		var result []pipeline.Step
		for _, step := range steps.AllSteps() {
			result = append(result, &authoredFixtureStep{name: step.Name(), calls: &calls})
		}
		return result
	}
	manager, database, repo, request, gate := authoredAdmissionFixture(t, factory)
	before, err := database.ReadTerminalSnapshot(request.Request.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	gateHead := gitOutput(t, gate, "rev-parse", "refs/heads/feature")
	result, err := manager.validateAuthored(context.Background(), request, os.Getpid())
	if err != nil || !result.Started {
		t.Fatalf("admission: %+v %v", result, err)
	}
	child := waitForRunTerminalState(t, database, result.Receipt.RunID)
	if child.Status != types.RunCompleted || calls.Load() != int32(len(steps.AllSteps())) {
		t.Fatalf("fresh stages: status=%s calls=%d", child.Status, calls.Load())
	}
	after, err := database.ReadTerminalSnapshot(request.Request.PredecessorID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("predecessor was changed: %v", err)
	}
	if gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD") != request.Request.CandidateSHA || gitOutput(t, gate, "rev-parse", "refs/heads/feature") != gateHead {
		t.Fatal("admission overwrote an author or gate branch")
	}
	status, err := git.Run(context.Background(), repo.WorkingPath, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || status != "" {
		t.Fatal("admission dirtied the author source")
	}
	replay, err := manager.validateAuthored(context.Background(), request, os.Getpid())
	if err != nil || replay.Started || replay.Receipt.RunID != child.ID || calls.Load() != int32(len(steps.AllSteps())) {
		t.Fatalf("repeat restarted completed validation: %+v %v", replay, err)
	}
	if _, err := manager.startRun(context.Background(), repo, request.Request.Branch, child.HeadSHA, child.BaseSHA, "fresh", nil, "fresh budget", "", true, "", nil); err == nil {
		t.Fatal("ordinary fresh launch escaped validation-only lineage")
	}
}

func TestAuthoredAdmissionRefusesSourceAndLaunchMismatchesWithoutMutation(t *testing.T) {
	for _, change := range []string{"dirty", "untracked hidden by config", "wrong tree", "wrong branch", "wrong repository", "wrong trusted repository", "unrelated gate head", "stale snapshot", "wrong generation", "wrong launch", "unauthenticated"} {
		t.Run(change, func(t *testing.T) {
			manager, database, repo, request, gate := authoredAdmissionFixture(t, nil)
			operator := os.Getpid()
			switch change {
			case "dirty":
				if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("dirty"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "untracked hidden by config":
				gitCmd(t, repo.WorkingPath, "config", "status.showUntrackedFiles", "no")
				if err := os.WriteFile(filepath.Join(repo.WorkingPath, "untracked.txt"), []byte("untracked"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "wrong tree":
				request.Request.CandidateTree = strings.Repeat("b", 40)
			case "wrong branch":
				request.Request.Branch = "other"
			case "wrong repository":
				request.Request.RepoID = "other"
			case "wrong trusted repository":
				gitCmd(t, gate, "remote", "set-url", "origin", "https://example.invalid/wrong.git")
			case "unrelated gate head":
				gitCmd(t, gate, "fetch", "--no-tags", repo.WorkingPath, request.Request.CandidateSHA)
				gitCmd(t, gate, "update-ref", "refs/heads/feature", request.Request.CandidateSHA)
			case "stale snapshot":
				request.Request.SnapshotDigest = strings.Repeat("b", 64)
			case "wrong generation":
				request.Request.ValidationGeneration = "other"
			case "wrong launch":
				request.Request.LaunchAssertion.Profiles["primary"][0].Model = "other"
			case "unauthenticated":
				operator = 0
			}
			before, err := database.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			gateHead := gitOutput(t, gate, "rev-parse", "refs/heads/feature")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := manager.validateAuthored(ctx, request, operator); err == nil {
				t.Fatal("accepted mismatched admission")
			}
			after, err := database.GetRunsByRepo(repo.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed run history: %v", err)
			}
			if gitOutput(t, gate, "rev-parse", "refs/heads/feature") != gateHead {
				t.Fatal("refusal moved the gate branch")
			}
		})
	}
}
