package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

func TestAcceptedReviewScopeRunsOneBaselineAfterReview(t *testing.T) {
	for _, baseline := range []string{"repository", "machine-local"} {
		for _, mode := range []string{"approve", "scope", "recovered approve", "recovered scope"} {
			t.Run(baseline+"/"+mode, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				counter := filepath.Join(t.TempDir(), "baseline-runs.txt")
				appPaths := paths.WithRoot(t.TempDir())
				if err := appPaths.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				database, err := db.Open(appPaths.DB())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { database.Close() })
				repo, err := database.InsertRepo(dir, "https://example.invalid/repo", "main")
				if err != nil {
					t.Fatal(err)
				}
				run, err := database.InsertRun(repo.ID, "refs/heads/feature", head, base)
				if err != nil {
					t.Fatal(err)
				}
				command := "echo ran>> " + counter
				cfg := &config.Config{Agent: types.AgentClaude, SideBySide: true, TrustedConfigSHA: "trusted", Commands: config.Commands{Test: command, Lint: "echo lint"}}
				if baseline == "machine-local" {
					cfg.Commands.Test = ""
					cfg.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: []string{command}}}
				}
				ensureHermeticOrigin(t, dir)
				ag := &sideBySideAgent{}
				const findings = `{"findings":[{"id":"R1","severity":"error","file":"feature.txt","description":"Expand cleanup bounds","action":"ask-user"}]}`
				review := &transitionStep{name: types.StepReview, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
					startSideLanes(sctx, base)
					(&sideBySideFixture{t: t, review: sctx}).lanesDone()
					if sctx.Fixing {
						return &pipeline.StepOutcome{Findings: `{"findings":[],"reviewed_paths":["feature.txt"]}`, ReviewedPaths: []string{"feature.txt"}, ReviewablePaths: []string{"feature.txt"}, ReviewApprovedHeadSHA: head}, nil
					}
					return &pipeline.StepOutcome{NeedsApproval: true, Findings: findings, ReviewApprovedHeadSHA: head}, nil
				}}
				steps := []pipeline.Step{review, &TestStep{}, &DocumentStep{}}
				parks := make(chan struct{}, 4)
				emit := func(event ipc.Event) {
					if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepReview && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
						parks <- struct{}{}
					}
				}
				executor := pipeline.NewExecutor(database, appPaths, cfg, ag, steps, emit)
				ctx, cancel := context.WithCancelCause(context.Background())
				done := make(chan error, 1)
				finished := make(chan struct{})
				go func() { defer close(finished); done <- executor.Execute(ctx, run, repo, dir) }()
				t.Cleanup(func() {
					cancel(pipeline.ErrDaemonShutdown)
					select {
					case <-finished:
					case <-time.After(10 * time.Second):
						t.Error("executor did not stop")
					}
				})
				waitForPark := func() {
					t.Helper()
					select {
					case <-parks:
					case err := <-done:
						t.Fatalf("Review did not park: %v", err)
					case <-time.After(10 * time.Second):
						t.Fatal("Review did not park")
					}
				}
				waitForPark()
				if data, err := os.ReadFile(counter); !os.IsNotExist(err) {
					t.Fatalf("baseline ran before Review settled: %q, %v", data, err)
				}
				if strings.HasPrefix(mode, "recovered") {
					cancel(pipeline.ErrDaemonShutdown)
					select {
					case err := <-done:
						if !errors.Is(err, pipeline.ErrParkedAtShutdown) {
							t.Fatalf("park was not retained: %v", err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("executor did not stop at its park")
					}
					<-finished
					if err := database.Close(); err != nil {
						t.Fatal(err)
					}
					database, err = db.Open(appPaths.DB())
					if err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					executor = pipeline.NewExecutor(database, appPaths, cfg, ag, steps, emit)
					ctx, cancel = context.WithCancelCause(context.Background())
					done = make(chan error, 1)
					finished = make(chan struct{})
					go func() { defer close(finished); done <- executor.Resume(ctx, run, repo, dir) }()
					waitForPark()
				}
				records, err := database.GetStepsByRun(run.ID)
				if err != nil || len(records) != 3 {
					t.Fatalf("steps=%v err=%v", records, err)
				}
				rounds, err := database.GetRoundsByStep(records[0].ID)
				if err != nil || len(rounds) != 1 {
					t.Fatalf("Review spending changed at recovery: %v %v", rounds, err)
				}
				originalFindings := *rounds[0].FindingsJSON
				if strings.HasSuffix(mode, "scope") {
					parsed, err := types.ParseFindingsJSON(originalFindings)
					if err != nil {
						t.Fatal(err)
					}
					decision := &types.ReviewScopeDecision{Version: 1, DecisionID: "decision", Authority: "controller_assertion", RunID: run.ID, StepResultID: records[0].ID, RoundID: rounds[0].ID, SourceHeadSHA: head, SourceBaseSHA: base, TrustedConfigSHA: "trusted", Finding: parsed.Items[0], AcceptedScope: "retain fixed cleanup bounds"}
					if _, err := executor.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, nil, nil, nil, "", pipeline.ResponseOptions{ScopeDecision: decision}); err != nil {
						t.Fatal(err)
					}
				} else if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("settled Review did not reach Test")
				}
				data, err := os.ReadFile(counter)
				if err != nil || len(strings.Fields(string(data))) != 1 {
					t.Fatalf("baseline must run once after settled Review: %q %v", data, err)
				}
				if calls := ag.evidenceCalls(); len(calls) != 1 || !samePath(calls[0], dir) {
					t.Fatalf("Test must run once in the run worktree: %v", calls)
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
					t.Fatalf("same-source disposition changed source: %s", got)
				}
				rounds, err = database.GetRoundsByStep(records[0].ID)
				expectedRounds := 1
				if strings.HasSuffix(mode, "scope") {
					expectedRounds = 2
				}
				if err != nil || len(rounds) != expectedRounds || rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != originalFindings {
					t.Fatalf("Review evidence or spending changed: %v %v", rounds, err)
				}
				if expectedRounds == 2 && !rounds[1].IsFixRound() {
					t.Fatal("scope rereview lost its spent repair round")
				}
				documentCalls := ag.documentCalls()
				expectedDocumentCalls := 2
				if mode == "approve" {
					expectedDocumentCalls = 1
				}
				if len(documentCalls) != expectedDocumentCalls || (mode != "approve" && !samePath(documentCalls[len(documentCalls)-1], dir)) {
					t.Fatalf("Document did not refresh outdated scope evidence: %v", documentCalls)
				}
			})
		}
	}
}
