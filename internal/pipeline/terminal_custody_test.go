package pipeline

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func terminalSuccessorFixture(t *testing.T, name types.StepName, limit, spent int) (*db.DB, *paths.Paths, *db.Repo, *db.TerminalAdoption) {
	t.Helper()
	database, p, initial, repo := setupTest(t)
	if err := database.UpdateRunStatus(initial.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {profile}, "reviewer": {profile}, "fixer": {profile}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", strings.Repeat("b", 40), strings.Repeat("c", 40), nil, "nonce", "ordinary2", "intent", "", false, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(parent.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, limit); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(step.ID, 1, "initial", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < spent; index++ {
		trigger := "auto_fix"
		if index == 2 {
			trigger = "user_fix"
		}
		if _, err := database.InsertStepRound(step.ID, index+2, trigger, nil, nil, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunErrorStatus(parent.ID, "original cancelled", types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := database.AdoptTerminalCandidate(context.Background(), db.TerminalAdoption{Snapshot: *snapshot, CandidateSHA: parent.HeadSHA, CandidateTree: strings.Repeat("d", 40), Reason: "fresh validation", Operator: "fixture", Assertion: assertion, Proof: proof})
	if err != nil {
		t.Fatal(err)
	}
	return database, p, repo, event
}

func TestExecutorTerminalSuccessorDoesNotRenewRepairCapacity(t *testing.T) {
	for _, test := range []struct {
		name         types.StepName
		limit, spent int
	}{{types.StepReview, 2, 3}, {types.StepCI, 1, 1}} {
		t.Run(string(test.name), func(t *testing.T) {
			database, p, repo, event := terminalSuccessorFixture(t, test.name, test.limit, test.spent)
			child, err := database.GetRun(event.Successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			step := &mockStep{name: test.name, outcome: &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"fresh","severity":"error","file":"main.go","description":"fresh finding","action":"auto-fix"}]}`}}
			executor := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 2, CI: 1}}, nil, []Step{step}, nil)
			_, cancel := startExecutor(t, executor, child, repo, t.TempDir())
			waitForStepStatus(t, database, child.ID, test.name, types.StepStatusAwaitingApproval)
			if step.callCount() != 1 {
				t.Fatalf("spent family dispatched another fixer: %d", step.callCount())
			}
			budget, family, err := database.TerminalPhaseRemaining(child.ID, test.name, test.limit)
			if err != nil || !family || budget.Spent != test.spent || budget.Remaining != 0 {
				t.Fatalf("family budget=%+v err=%v", budget, err)
			}
			parent, err := database.ReadTerminalSnapshot(event.Snapshot.Run.ID)
			if err != nil || !reflect.DeepEqual(parent, &event.Snapshot) {
				t.Fatal("fresh finding changed predecessor")
			}
			cancel()
		})
	}
}

func TestExecutorTerminalSuccessorFailureLeavesNoPendingOrphan(t *testing.T) {
	database, p, repo, event := terminalSuccessorFixture(t, types.StepReview, 2, 3)
	child, err := database.GetRun(event.Successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	events := collectEvents(executor)
	if err := executor.ExecuteTerminalSuccessor(context.Background(), child, repo, t.TempDir(), event); err == nil {
		t.Fatal("missing candidate checkout accepted")
	}
	failed, err := database.GetRun(child.ID)
	if err != nil || failed.Status != types.RunFailed || failed.Error == nil {
		t.Fatalf("orphan child=%+v err=%v", failed, err)
	}
	if events.findRunEvent(ipc.EventRunCompleted) == nil {
		t.Fatal("failure omitted terminal event")
	}
	parent, err := database.ReadTerminalSnapshot(event.Snapshot.Run.ID)
	if err != nil || !reflect.DeepEqual(parent, &event.Snapshot) {
		t.Fatal("failure changed predecessor")
	}
}
