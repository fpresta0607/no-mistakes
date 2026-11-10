package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewLoop_NativeSafetyRefusalPreservesWorkAndParks(t *testing.T) {
	_, database, run, repo, workDir := reviewSessionHarness(t, &sessionMockAgent{}, nil)
	originalHead := run.HeadSHA
	originalFeature, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(pipeline.SessionRoleFixer), "codex", "original-fixer"); err != nil {
		t.Fatal(err)
	}
	refusal := "This content was flagged for possible cybersecurity risk."
	review := `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","description":"needs a decision","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"needs a decision","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`
	reviewText, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	reviewEvent := fmt.Sprintf(`{"type":"item.completed","item":{"type":"agent_message","text":%s}}`, reviewText)
	refusalEvent := fmt.Sprintf(`{"type":"error","message":%q}`, refusal)
	script := fmt.Sprintf("#!/bin/sh\necho call >> invocations.txt\nif [ \"$2\" = resume ]; then\n  echo retained > retained.txt\n  echo retained >> feature.txt\n  echo '%s'\n  exit 1\nfi\necho '%s'\n", refusalEvent, reviewEvent)
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.cmd"
		script = fmt.Sprintf("@echo off\r\necho call>>invocations.txt\r\nif \"%%2\"==\"resume\" goto refused\r\necho %s\r\nexit /b 0\r\n:refused\r\necho retained>retained.txt\r\necho retained>>feature.txt\r\necho %s\r\nexit /b 1\r\n", reviewEvent, refusalEvent)
	}
	bin := filepath.Join(t.TempDir(), name)
	writeFakeExecutable(t, bin, script)
	native, err := agent.New(types.AgentCodex, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	appPaths := paths.WithRoot(t.TempDir())
	executor := pipeline.NewExecutor(database, appPaths, &config.Config{SessionReuse: true, AutoFix: config.AutoFix{Review: 3}}, native,
		[]pipeline.Step{&ReviewStep{}, &TestStep{}, &PushStep{}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if t.Failed() {
				log, _ := os.ReadFile(filepath.Join(appPaths.RunLogDir(run.ID), "review.log"))
				t.Logf("executor: %v\n%s", err, log)
			}
		case <-time.After(5 * time.Second):
			t.Error("executor did not stop")
		}
	})
	waitForReviewStatus(t, database, run.ID, types.StepStatusAwaitingApproval)
	if err := executor.Respond(types.StepReview, types.ActionFix, []string{"f-1"}); err != nil {
		t.Fatal(err)
	}
	waitForReviewStatus(t, database, run.ID, types.StepStatusFixReview)
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
		t.Fatal("approval advanced past a safety refusal")
	}
	storedRun, err := database.GetRun(run.ID)
	if err != nil || storedRun.Status != types.RunRunning || storedRun.AwaitingAgentSince == nil || storedRun.HeadSHA != originalHead {
		t.Fatalf("refusal must keep the original run parked: %+v, %v", storedRun, err)
	}
	if head := gitCmd(t, workDir, "rev-parse", "HEAD"); head != originalHead {
		t.Errorf("refusal committed work: got %s, want %s", head, originalHead)
	}
	stepResults, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range stepResults {
		if step.StepName == types.StepReview {
			if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, refusal) {
				t.Errorf("refusal is not visible in the gate: %+v", step)
			}
		} else if step.Status != types.StepStatusPending {
			t.Errorf("validation or publication advanced after refusal: %+v", step)
		}
	}
	invocations, err := os.ReadFile(filepath.Join(workDir, "invocations.txt"))
	if err != nil || len(strings.Fields(string(invocations))) != 2 {
		t.Errorf("expected only the review and refused resume, got %q, %v", invocations, err)
	}
	retained, err := os.ReadFile(filepath.Join(workDir, "retained.txt"))
	if err != nil || strings.TrimSpace(string(retained)) != "retained" {
		t.Errorf("refused process edits were lost: %q, %v", retained, err)
	}
	feature, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil || strings.ReplaceAll(string(feature), "\r\n", "\n") != strings.ReplaceAll(string(originalFeature), "\r\n", "\n")+"retained\n" {
		t.Errorf("refused process source edits were lost: %q, %v", feature, err)
	}
	if staged := gitCmd(t, workDir, "show", ":feature.txt"); staged != strings.TrimSpace(string(originalFeature)) {
		t.Errorf("refusal changed the index: got %q, want %q", staged, originalFeature)
	}
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "original-fixer" {
		t.Errorf("original fixer session was lost: %+v, %v", sessions, err)
	}
	log, err := os.ReadFile(filepath.Join(appPaths.RunLogDir(run.ID), "review.log"))
	if err != nil || !strings.Contains(string(log), refusal) || strings.Contains(string(log), "starting a fresh") {
		t.Errorf("refusal log was lost or announced a retry: %s, %v", log, err)
	}
}

const nativeSafetyRefusal = "codex exited: exit status 1: This content was flagged for possible cybersecurity risk."

func refusingAgent(calls *int) *mockAgent {
	return &mockAgent{name: "codex", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		*calls++
		return nil, errors.Join(agent.ErrSafetyRefusal, errors.New(nativeSafetyRefusal))
	}}
}

// A refused CI fixer is not an ordinary failure the next settled observation
// retries: the step returns the refusal for the executor to park on.
func TestCIStep_SafetyRefusedFixIsNotRetried(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	prURL := "https://github.com/test/repo/pull/3195"
	sctx := newTestContext(t, refusingAgent(&calls), dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"greptile","state":"FAILURE","bucket":"fail"}]`)
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		t.Fatal("a refused CI fix resumed monitoring")
		return nil
	}}

	_, err := driveCI(t, step, sctx)

	if !agent.IsSafetyRefusal(err) {
		t.Fatalf("CI step error = %v, want the refusal", err)
	}
	if calls != 1 {
		t.Fatalf("agent invocations = %d, want the refused fixer alone", calls)
	}
}

// A refused PR draft publishes nothing: fallback content would carry the run
// past the refusal.
func TestPRStep_SafetyRefusedDraftPublishesNothing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	calls := 0
	sctx := newTestContextWithDBRecords(t, refusingAgent(&calls), dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	_, err := (&PRStep{}).Execute(sctx)

	if !agent.IsSafetyRefusal(err) {
		t.Fatalf("PR step error = %v, want the refusal", err)
	}
	ghLog, readErr := os.ReadFile(logFile)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if strings.Contains(string(ghLog), "pr create") || strings.Contains(string(ghLog), "pr edit") {
		t.Fatalf("a refused draft was published:\n%s", ghLog)
	}
}

// A refused coverage completion is not an ordinary coverage gap, which an
// approval or --yes could clear.
func TestReviewStep_SafetyRefusedCoverageCompletionIsNotACoverageGap(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := &mockAgent{name: "codex", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, coverageCompletionMarker) {
			return nil, errors.Join(agent.ErrSafetyRefusal, errors.New(nativeSafetyRefusal))
		}
		return &agent.Result{Output: json.RawMessage(coverageFindingJSON([]string{"feature.txt"}))}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)

	if !agent.IsSafetyRefusal(err) {
		t.Fatalf("Execute() = %#v, %v; want the refusal", outcome, err)
	}
}

// Work refused beside the review is never thrown away for its step to run
// again, which would repeat the refused request. However the lane is reached,
// its step returns the refusal at its turn and the lane's copy, with its
// edits, is kept for the operator. A daemon restart before that turn leaves
// only the lane's record on the step's row, and the step stops on it as well.
func TestSideBySide_RefusedLaneIsKeptAndNotRunAgain(t *testing.T) {
	for _, exit := range []string{"taken", "head moved", "stopped", "refused while stopping", "daemon restarted"} {
		t.Run(exit, func(t *testing.T) {
			f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
			started := make(chan string, 1)
			var calls atomic.Int32
			f.agent.documentF = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if calls.Add(1) > 1 {
					return nil, errors.New("the refused work ran again")
				}
				if err := os.WriteFile(filepath.Join(opts.CWD, "refused.txt"), []byte("kept"), 0o644); err != nil {
					return nil, err
				}
				started <- opts.CWD
				if exit == "refused while stopping" {
					<-ctx.Done()
				}
				return nil, errors.Join(agent.ErrSafetyRefusal, errors.New(nativeSafetyRefusal))
			}
			startSideLanes(f.review, f.baseSHA)
			var laneDir string
			select {
			case laneDir = <-started:
			case <-time.After(sideBySideWait):
				t.Fatal("the documentation pass did not start beside the review")
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(laneDir)) })
			switch exit {
			case "head moved":
				f.lanesDone()
				if err := os.WriteFile(filepath.Join(f.dir, "fixed.go"), []byte("package fixed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "no-mistakes(review): fix a finding")
				if err := recordAgentFixHead(f.review, types.StepReview, gitCmd(t, f.dir, "rev-parse", "HEAD")); err != nil {
					t.Fatal(err)
				}
			case "stopped":
				f.lanesDone()
				f.review.Shared.SideLanes().Stop(f.review)
			case "refused while stopping":
				f.review.Shared.SideLanes().Stop(f.review)
			case "daemon restarted":
				f.lanesDone()
				f.review.Shared = &pipeline.RunShared{}
			default:
				f.lanesDone()
			}

			_, err := (&DocumentStep{}).Execute(f.documentTurn())

			if !agent.IsSafetyRefusal(err) {
				t.Fatalf("Execute() error = %v, want the lane's refusal", err)
			}
			if calls := f.agent.documentCalls(); len(calls) != 1 {
				t.Fatalf("documentation passes = %q, want the refused one alone", calls)
			}
			if data, err := os.ReadFile(filepath.Join(laneDir, "refused.txt")); err != nil || string(data) != "kept" {
				t.Fatalf("the refused lane's edit = %q, %v; want it kept", data, err)
			}
			if record, err := f.review.DB.TakeSideLane(f.documentID); err != nil || record != nil {
				t.Fatalf("record after the step stopped on it = %+v, %v; want it spent", record, err)
			}
		})
	}
}

// The documentation pass started beside the review is refused while the review
// waits at its gate, and the daemon restarts there. Once the recovered review
// gate is answered, the Document step stops on that refusal at its turn
// instead of repeating the refused request.
func TestSideBySide_RefusedLaneSurvivesADaemonRestart(t *testing.T) {
	dir, base, head := setupGitRepo(t)
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
	cfg := &config.Config{Agent: types.AgentClaude, SideBySide: true, TrustedConfigSHA: "trusted", Commands: config.Commands{Lint: "echo lint"}}
	ensureHermeticOrigin(t, dir)
	ag := &sideBySideAgent{}
	var passes atomic.Int32
	ag.documentF = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if passes.Add(1) > 1 {
			return nil, errors.New("the refused work ran again")
		}
		return nil, errors.Join(agent.ErrSafetyRefusal, errors.New(nativeSafetyRefusal))
	}
	review := &transitionStep{name: types.StepReview, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		startSideLanes(sctx, base)
		(&sideBySideFixture{t: t, review: sctx}).lanesDone()
		return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"R1","severity":"error","file":"feature.txt","description":"needs a decision","action":"ask-user"}]}`, ReviewApprovedHeadSHA: head}, nil
	}}
	steps := []pipeline.Step{review, &DocumentStep{}}
	parks := make(chan types.StepName, 4)
	emit := func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			parks <- *event.StepName
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
	waitForPark := func(want types.StepName) {
		t.Helper()
		select {
		case got := <-parks:
			if got != want {
				t.Fatalf("%s parked, want %s", got, want)
			}
		case err := <-done:
			t.Fatalf("%s did not park: %v", want, err)
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not park", want)
		}
	}
	waitForPark(types.StepReview)
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
	if database, err = db.Open(appPaths.DB()); err != nil {
		t.Fatal(err)
	}
	if run, err = database.GetRun(run.ID); err != nil {
		t.Fatal(err)
	}
	executor = pipeline.NewExecutor(database, appPaths, cfg, ag, steps, emit)
	ctx, cancel = context.WithCancelCause(context.Background())
	defer cancel(pipeline.ErrDaemonShutdown)
	done = make(chan error, 1)
	finished = make(chan struct{})
	go func() { defer close(finished); done <- executor.Resume(ctx, run, repo, dir) }()
	waitForPark(types.StepReview)

	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}

	waitForPark(types.StepDocument)
	if got := passes.Load(); got != 1 {
		t.Fatalf("documentation passes = %d, want the refused one alone", got)
	}
	records, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.StepName == types.StepDocument && (record.FindingsJSON == nil || !pipeline.HasSafetyRefusal(*record.FindingsJSON)) {
			t.Fatalf("Document parked on %v, want the native safety refusal", record.FindingsJSON)
		}
	}
}

// A daemon started after work beside the review ended knows only that work's
// record. The step stops on a recorded refusal, and on work that never
// recorded how it ended, such as a refusal whose record could not be written,
// instead of doing that work again; and nothing new starts beside a review
// while either is outstanding. Once the step has stopped on it the record is
// spent, so the step's next turn does its work.
func TestSideBySide_RecordedLaneStopsItsStepAfterARestart(t *testing.T) {
	for _, outcome := range []string{"refused", "never recorded how it ended"} {
		t.Run(outcome, func(t *testing.T) {
			f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
			if err := f.review.DB.StartSideLane(f.documentID, f.headSHA); err != nil {
				t.Fatal(err)
			}
			if outcome == "refused" {
				if err := f.review.DB.RecordSideLaneRefusal(f.documentID, f.headSHA, "native agent safety refusal: "+nativeSafetyRefusal); err != nil {
					t.Fatal(err)
				}
			}

			startSideLanes(f.review, f.baseSHA)
			f.lanesDone()
			_, err := (&DocumentStep{}).Execute(f.documentTurn())

			if !agent.IsSafetyRefusal(err) {
				t.Fatalf("Execute() error = %v, want the recorded refusal", err)
			}
			if calls := f.agent.documentCalls(); len(calls) != 0 {
				t.Fatalf("documentation passes = %q, want none", calls)
			}
			if !strings.Contains(f.logText(), "nothing starts beside the review") {
				t.Fatalf("log = %q, want nothing started beside the review", f.logText())
			}
			if _, err := (&DocumentStep{}).Execute(f.documentTurn()); err != nil {
				t.Fatalf("next turn error = %v, want the step's own pass", err)
			}
			if calls := f.agent.documentCalls(); len(calls) != 1 || !samePath(calls[0], f.dir) {
				t.Fatalf("documentation passes = %q, want one in the run's worktree", calls)
			}
		})
	}
}

// Parking on a refusal stops the work beside the review and keeps its log.
func TestSideBySide_StopEndsTheLanesAndKeepsTheirLogs(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
	started := make(chan string, 1)
	longLog := "beginning of lane log " + strings.Repeat("x", 1<<20) + " end of lane log"
	f.agent.documentF = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnChunk(longLog)
		started <- opts.CWD
		<-ctx.Done()
		return nil, ctx.Err()
	}
	startSideLanes(f.review, f.baseSHA)
	var laneDir string
	select {
	case laneDir = <-started:
	case <-time.After(sideBySideWait):
		t.Fatal("the documentation pass did not start beside the review")
	}

	f.review.Shared.SideLanes().Stop(f.review)

	if _, err := os.Stat(laneDir); !os.IsNotExist(err) {
		t.Fatalf("the stopped copy %s is still there (stat error %v)", laneDir, err)
	}
	if lanes, _ := f.review.Shared.SideLanes().(*sideLanes); lanes == nil || lanes.claim(types.StepDocument) != nil {
		t.Fatal("the stopped work was still left for the Document step")
	}
	if !strings.Contains(f.logText(), "log of the stopped document work") || !strings.Contains(f.logText(), "a native agent safety refusal stopped automatic execution") {
		t.Fatalf("log = %q, want the lane's log and the reason it stopped", f.logText())
	}
	if strings.Contains(f.logText(), "beginning of lane log") || !strings.Contains(f.logText(), "end of lane log") || !strings.Contains(f.logText(), "[earlier lane log truncated]") {
		t.Fatal("the parked step did not report the truncated lane log tail")
	}
}

// A refusal given as the analyzer's answer is not a schema slip: neither the
// review nor the test analyzer asks again.
func TestAnalyzersDoNotRetryStructuredSafetyRefusals(t *testing.T) {
	for _, name := range []string{"review", "test"} {
		t.Run(name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "codex", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, errors.Join(agent.ErrSafetyRefusal, rejectedStructuredOutputError{message: nativeSafetyRefusal})
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			var err error
			if name == "review" {
				_, err = (&ReviewStep{}).Execute(sctx)
			} else {
				_, err = runTestAnalyzer(sctx.Ctx, sctx, "test", "")
			}
			if !agent.IsSafetyRefusal(err) || calls != 1 {
				t.Fatalf("error = %v, calls = %d; want the refusal from one call", err, calls)
			}
		})
	}
}

// A refused intent extraction stops the run instead of being skipped.
func TestIntentStepPropagatesSafetyRefusal(t *testing.T) {
	sctx := newIntentStepContext(t)
	step := &IntentStep{runIntent: func(context.Context, *pipeline.StepContext) (*intent.Result, error) {
		return nil, errors.Join(agent.ErrSafetyRefusal, errors.New(nativeSafetyRefusal))
	}}

	_, err := step.Execute(sctx)

	if !agent.IsSafetyRefusal(err) {
		t.Fatalf("Execute() error = %v, want the refusal", err)
	}
}
