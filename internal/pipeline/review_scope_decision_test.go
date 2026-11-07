package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewScopeDecisionWithoutEvalProvenance(t *testing.T) {
	database, root, run, repo := setupTest(t)
	workDir := coverageWorkdir(t, database, run)
	cfg := &config.Config{TrustedConfigSHA: "trusted", ReplayGlobalYAML: []byte("global snapshot"), ReplayRepoYAML: []byte("repo snapshot")}
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree, ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	executor := NewExecutor(database, root, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, executor, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("read Review step: steps=%v err=%v", steps, err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("read Review round: rounds=%v err=%v", rounds, err)
	}
	round := rounds[0]
	if round.TrustedConfigSHA == nil || *round.TrustedConfigSHA != "trusted" || len(round.GlobalConfigYAML) != 0 || len(round.RepoConfigYAML) != 0 {
		t.Fatalf("minimal source binding was lost or optional snapshots were captured: %+v", round)
	}
	findings, err := types.ParseFindingsJSON(gateFindingsThree)
	if err != nil {
		t.Fatal(err)
	}
	decision := types.ReviewScopeDecision{Version: 1, DecisionID: "decision", Authority: "controller_assertion", RunID: run.ID, StepResultID: steps[0].ID, RoundID: round.ID, SourceHeadSHA: run.HeadSHA, SourceBaseSHA: run.BaseSHA, TrustedConfigSHA: "trusted", Finding: findings.Items[0], AcceptedScope: "retain fixed cleanup bounds"}
	if err := database.SetReviewScopeDecision(decision, `["R1"]`, gateFindingsThree, types.ResponseOriginUnknown); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetReviewScopeDecisions(run.ID)
	if err != nil || len(stored) != 1 || stored[0] != decision {
		t.Fatalf("decision could not be reloaded without eval snapshots: stored=%v err=%v", stored, err)
	}
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}

func TestExecutorScopeDecisionIsBoundBeforeDispatch(t *testing.T) {
	for _, state := range []string{"accepted", "stale binding", "dirty source", "stale trusted config"} {
		t.Run(state, func(t *testing.T) {
			database, paths, run, _ := setupTest(t)
			workDir := coverageWorkdir(t, database, run)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			findings := gateFindingsThree
			round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, run.HeadSHA, run.HeadSHA, "trusted", nil, nil, 9)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := types.ParseFindingsJSON(findings)
			if err != nil {
				t.Fatal(err)
			}
			decision := &types.ReviewScopeDecision{Version: 1, DecisionID: "decision", Authority: "controller_assertion", RunID: run.ID, StepResultID: step.ID, RoundID: round.ID, SourceHeadSHA: run.HeadSHA, SourceBaseSHA: run.BaseSHA, TrustedConfigSHA: "trusted", Finding: parsed.Items[0], AcceptedScope: "retain fixed cleanup bounds"}
			if state == "stale binding" {
				decision.SourceHeadSHA = "other-source"
			}
			if state == "dirty source" {
				if err := os.WriteFile(filepath.Join(workDir, "untracked.txt"), []byte("unreviewed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if state == "stale trusted config" {
				decision.TrustedConfigSHA = "old-trusted"
			}
			executor := NewExecutor(database, paths, &config.Config{TrustedConfigSHA: "trusted"}, nil, nil, nil)
			executor.workDir = workDir
			executor.waiting, executor.waitingStep = true, types.StepReview
			executor.waitingRoundID, executor.waitingStepResultID = round.ID, step.ID
			executor.waitingFindings = findings
			executor.approvalCh = make(chan approvalResponse, 1)
			_, err = executor.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, "", ResponseOptions{ScopeDecision: decision})
			if state != "accepted" {
				if err == nil || !executor.AwaitingApproval() || len(executor.approvalCh) != 0 {
					t.Fatalf("stale scope released the gate: err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetReviewScopeDecisions(run.ID)
			if err != nil || len(stored) != 1 || stored[0] != *decision {
				t.Fatalf("scope was not durable before dispatch: stored=%v err=%v", stored, err)
			}
			select {
			case response := <-executor.approvalCh:
				if response.scopeDecision == nil || *response.scopeDecision != *decision {
					t.Fatal("dispatch lost its bound scope decision")
				}
			default:
				t.Fatal("accepted response was never dispatched")
			}
		})
	}
}

func TestRunAgentRefusesUnavailableAcceptedScope(t *testing.T) {
	database, _, run, _ := setupTest(t)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	ag := &hangingAgent{name: "test"}
	sctx := &StepContext{Ctx: context.Background(), Run: run, DB: database, Agent: ag}
	_, err := sctx.RunAgent(agent.RunOpts{Prompt: "Test must retain its evidence"})
	if err == nil || !strings.Contains(err.Error(), "accepted Review scope unavailable") || ag.calls != 0 {
		t.Fatalf("unreadable authority must refuse the turn with an actionable state: err=%v calls=%d", err, ag.calls)
	}
}
