package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func validationOnlyFixture(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo) {
	t.Helper()
	database, root, existing, repo := setupTest(t)
	if err := database.UpdateRunStatus(existing.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := git.Run(context.Background(), workDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	repo, err = database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := db.RunIntent{Summary: "Validate the declared authored candidate", Source: db.RunIntentSourceAgent, Score: 1}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
	parent, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", strings.Repeat("b", 40), strings.Repeat("c", 40), &intent, "original", "generation", digest, "", true, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(parent.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repo.ID, Branch: "feature", CallerDir: workDir, CandidateSHA: head, CandidateTree: tree, LaunchNonce: "original", ValidationGeneration: "generation", IntentDigest: digest,
		ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("f", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate authored source"}
	event, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), Operator: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := database.GetRun(event.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	return database, root, child, repo
}

func TestValidationOnlyNeverDispatchesAutomaticOrManualRepairs(t *testing.T) {
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI, "gate.review.extra"} {
		t.Run(string(name), func(t *testing.T) {
			database, root, child, repo := validationOnlyFixture(t)
			var calls atomic.Int32
			step := &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls.Add(1)
				if sctx.Fixing {
					t.Error("validation-only run invoked a fixer")
				}
				return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"r1","severity":"error","file":"file.go","description":"repair needed","action":"auto-fix"}]}`}, nil
			}}
			configuration := &config.Config{AutoFix: config.AutoFix{Review: 5, Test: 5, Document: 5, Lint: 5, Rebase: 5, CI: 5}}
			executor := NewExecutor(database, root, configuration, nil, []Step{step}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, child, repo, repo.WorkingPath) }()
			isParked := false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				executor.mu.Lock()
				isParked = executor.waiting
				executor.mu.Unlock()
				if isParked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !isParked {
				cancel()
				<-done
				t.Fatal("fresh validation did not park")
			}
			resultRows, err := database.GetStepsByRun(child.ID)
			if err != nil || len(resultRows) != 1 || resultRows[0].AutoFixLimit == nil || *resultRows[0].AutoFixLimit != 0 {
				t.Errorf("zero repair authority was not explicitly recorded: %+v %v", resultRows, err)
			}
			for _, action := range []types.ApprovalAction{types.ActionFix, types.ActionSkip} {
				if err := executor.Respond(name, action, []string{"r1"}); err == nil {
					t.Errorf("accepted %s despite zero repair authority", action)
				}
			}
			if calls.Load() != 1 {
				t.Errorf("dispatched %d turns instead of one fresh validation", calls.Load())
			}
			cancel()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Fatal("executor did not stop")
			}
		})
	}
}

type handbackCountingCIStep struct {
	handbacks atomic.Int32
}

func (s *handbackCountingCIStep) Name() types.StepName { return types.StepCI }
func (s *handbackCountingCIStep) Execute(*StepContext) (*StepOutcome, error) {
	return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"ci-1","category":"ci-check","severity":"error","check":"tests","action":"auto-fix","description":"failed"}]}`}, nil
}
func (s *handbackCountingCIStep) ReconcileApprovalGate(*StepContext) (bool, error) {
	return false, nil
}
func (s *handbackCountingCIStep) HandbackCI(*StepContext, ipc.CIHandbackParams) (*ipc.CIHandbackResult, error) {
	s.handbacks.Add(1)
	return &ipc.CIHandbackResult{}, nil
}

func TestValidationOnlyRefusesCIHandbackBeforeRelease(t *testing.T) {
	for _, isResume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", isResume), func(t *testing.T) {
			database, root, child, repo := validationOnlyFixture(t)
			step := &handbackCountingCIStep{}
			executor := NewExecutor(database, root, &config.Config{}, nil, []Step{step}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, child, repo, repo.WorkingPath) }()
			isParked := false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				executor.mu.Lock()
				isParked = executor.waiting && executor.waitingStep == types.StepCI
				executor.mu.Unlock()
				if isParked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !isParked {
				cancel()
				<-done
				t.Fatal("fresh validation did not park at CI")
			}

			_, err := executor.HandbackCI(ctx, ipc.CIHandbackParams{RunID: child.ID, RepoID: repo.ID, Branch: child.Branch, Resume: isResume})

			if err == nil || !strings.Contains(err.Error(), "zero automatic/manual repair authority") {
				t.Errorf("CI handback error = %v, want the zero repair authority refusal", err)
			}
			if step.handbacks.Load() != 0 {
				t.Error("validation-only CI gate released or accepted a handback")
			}
			executor.mu.Lock()
			isStillParked, isReleased := executor.waiting && executor.waitingStep == types.StepCI, executor.ciHandbackReleased
			executor.mu.Unlock()
			if !isStillParked || isReleased {
				t.Errorf("refused handback changed the CI gate: parked=%t released=%t", isStillParked, isReleased)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Fatal("executor did not stop")
			}
		})
	}
}

func TestValidationOnlyRejectsPendingRepairSelectionsBeforeAnyDispatch(t *testing.T) {
	database, root, child, repo := validationOnlyFixture(t)
	step, err := database.InsertStepResult(child.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings, selected := `{"findings":[{"id":"r1","description":"repair"}]}`, `["r1"]`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	executor := NewExecutor(database, root, nil, nil, []Step{&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) { calls.Add(1); return &StepOutcome{}, nil }}}, nil)
	err = executor.Execute(context.Background(), child, repo, repo.WorkingPath)
	if err == nil || !strings.Contains(err.Error(), "forbidden repair selection") || calls.Load() != 0 {
		t.Fatalf("pending repair reached dispatch: calls=%d error=%v", calls.Load(), err)
	}
}
