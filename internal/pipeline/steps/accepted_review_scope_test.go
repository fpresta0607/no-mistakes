package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAcceptedReviewScopeReachesLaterStagesAfterRestart(t *testing.T) {
	const originalIntent = "Scale command, startup, run-wait and shutdown budgets."
	const correction = "Scale command, startup and run-wait budgets; retain fixed cleanup, ownership/reaper and fallback stop bounds."
	const originalFinding = `{"findings":[{"id":"R1","severity":"error","file":"harness.go","description":"Shutdown scaling is required; expand cleanup bounds.","action":"ask-user"}]}`
	for _, stage := range []string{"test evidence", "test repair", "document", "test new regression"} {
		t.Run(stage, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			callCount := 0
			ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				callCount++
				if stage == "document" {
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"fixed bounds remain documented"}`)}, nil
				}
				if stage == "test repair" && callCount == 1 {
					return &agent.Result{Output: json.RawMessage(`{"summary":"repair actual failure"}`)}, nil
				}
				if stage == "test new regression" {
					return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"actual-failure","severity":"error","file":"harness.go","description":"Starting a command at scale 4 expires before the configured run wait; observed timeout in the existing fixture.","action":"auto-fix"}],"summary":"observed timeout","scenarios":[{"name":"command deadline","result":"fail","live":true,"evidence":"existing fixture returned timeout at scale 4","reason":"observed failure"}],"verdict":"no-go","tested":["command deadline"],"artifacts":[],"testing_summary":"observed timeout"}`)}, nil
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ok","scenarios":[{"name":"command scaling","result":"pass","live":true,"evidence":"fixture","reason":"drove existing fixture"}],"verdict":"go","tested":["command scaling"],"artifacts":[],"testing_summary":"ok"}`)}, nil
			}}
			sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			databasePath := filepath.Join(t.TempDir(), "decisions.db")
			database, err := db.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			repo, err := database.InsertRepo(dir, "https://example.invalid/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", headSHA, baseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunIntent(run.ID, db.RunIntent{Summary: originalIntent, Source: db.RunIntentSourceAgent}); err != nil {
				t.Fatal(err)
			}
			review, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			findings := originalFinding
			round, err := database.InsertReviewStepRoundWithProvenance(review.ID, 1, "initial", &findings, nil, headSHA, headSHA, "trusted", nil, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			selected := `["R1"]`
			payload, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{ID: "R1", Description: "Shutdown scaling is required; expand cleanup bounds.", UserInstructions: correction}}})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := types.ParseFindingsJSON(findings)
			if err != nil {
				t.Fatal(err)
			}
			decision := types.ReviewScopeDecision{Version: 1, DecisionID: "operator-decision-1017625", Authority: "controller_assertion", RunID: run.ID, StepResultID: review.ID, RoundID: round.ID, SourceHeadSHA: headSHA, SourceBaseSHA: baseSHA, TrustedConfigSHA: "trusted", Finding: parsed.Items[0], AcceptedScope: correction}
			if err := database.SetReviewScopeDecision(decision, selected, payload, types.ResponseOriginInteractive); err != nil {
				t.Fatal(err)
			}
			clean := `{"findings":[],"reviewed_paths":["harness.go"]}`
			if _, err := database.InsertReviewStepRound(review.ID, 2, "auto_fix", &clean, nil, headSHA, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			database, err = db.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			sctx.DB, sctx.Repo = database, repo
			sctx.Run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			sctx.UserIntent, sctx.IntentSource = *sctx.Run.Intent, *sctx.Run.IntentSource
			if stage == "document" {
				bindStepResult(t, sctx, types.StepDocument)
				_, err = (&DocumentStep{}).Execute(sctx)
			} else {
				bindStepResult(t, sctx, types.StepTest)
				sctx.Fixing = stage == "test repair"
				sctx.PreviousFindings = `{"findings":[{"id":"actual-failure","severity":"error","description":"command deadline expires before the configured wait","action":"auto-fix"}]}`
				outcome, testErr := (&TestStep{}).Execute(sctx)
				err = testErr
				if stage == "test new regression" && (outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "observed timeout") || !strings.Contains(outcome.Findings, "no-go")) {
					t.Fatalf("accepted scope suppressed actual failed evidence: outcome=%+v err=%v", outcome, err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) == 0 {
				t.Fatal("later stage never prepared an agent turn")
			}
			if stage == "test repair" && len(ag.calls) != 2 {
				t.Fatalf("want repair and evidence turns, got %d", len(ag.calls))
			}
			for _, call := range ag.calls {
				for _, want := range []string{originalIntent, correction, run.ID, review.ID, round.ID, headSHA, decision.DecisionID, decision.Authority} {
					if !strings.Contains(call.Prompt, want) {
						t.Errorf("later stage lost scope decision content or identity %q", want)
					}
				}
			}
			rounds, err := database.GetRoundsByStep(review.ID)
			if err != nil || len(rounds) != 2 || *rounds[0].FindingsJSON != originalFinding || !rounds[1].IsFixRound() {
				t.Fatalf("handoff changed historical findings or repair spending: rounds=%+v err=%v", rounds, err)
			}
		})
	}
}

func TestAcceptedReviewScopeDiscardsSpeculativeTestEvidence(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Test: "count"})
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()
	if calls := f.agent.evidenceCalls(); len(calls) != 1 {
		t.Fatalf("want one speculative turn, got %v", calls)
	}
	database := f.review.DB
	if err := database.UpdateRunStatus(f.review.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(f.review.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw := declinedDedupFindings
	round, err := database.InsertReviewStepRoundWithProvenance(review.ID, 1, "initial", &raw, nil, f.headSHA, f.headSHA, "trusted", nil, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	decision := types.ReviewScopeDecision{Version: 1, DecisionID: "decision", Authority: "controller_assertion", RunID: f.review.Run.ID, StepResultID: review.ID, RoundID: round.ID, SourceHeadSHA: f.headSHA, SourceBaseSHA: f.baseSHA, TrustedConfigSHA: "trusted", Finding: findings.Items[0], AcceptedScope: "retain the existing bounded loop"}
	if err := database.SetReviewScopeDecision(decision, `["journal-version-deduplication"]`, raw, types.ResponseOriginUnknown); err != nil {
		t.Fatal(err)
	}
	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatal(err)
	}
	if calls := f.agent.evidenceCalls(); len(calls) != 2 || !samePath(calls[1], f.dir) || f.baselineRuns() != 2 {
		t.Fatalf("stale speculative proof was reused: calls=%v baselines=%d", calls, f.baselineRuns())
	}
	if !strings.Contains(f.logText(), "Review accepted an explicit scope decision") {
		t.Fatal("discarded evidence did not name the changed scope input")
	}
}
