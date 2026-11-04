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
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/intent"
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
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
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
		return nil, errors.New(nativeSafetyRefusal)
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
			return nil, errors.New(nativeSafetyRefusal)
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
// again, which would repeat the refused request. The step returns the refusal
// at its turn, whether or not the head moved since.
func TestSideBySide_RefusedLaneIsNotRunAgain(t *testing.T) {
	for _, headMoved := range []bool{false, true} {
		t.Run(fmt.Sprintf("head moved %v", headMoved), func(t *testing.T) {
			f := newSideBySideFixture(t, config.Commands{})
			f.agent.evidenceF = func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return nil, errors.New(nativeSafetyRefusal)
			}
			startSideLanes(f.review, f.baseSHA)
			f.lanesDone()
			if headMoved {
				if err := os.WriteFile(filepath.Join(f.dir, "fixed.go"), []byte("package fixed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "no-mistakes(review): fix a finding")
				if err := recordAgentFixHead(f.review, types.StepReview, gitCmd(t, f.dir, "rev-parse", "HEAD")); err != nil {
					t.Fatal(err)
				}
			}

			_, err := (&TestStep{}).Execute(f.turn())

			if !agent.IsSafetyRefusal(err) {
				t.Fatalf("Execute() error = %v, want the lane's refusal", err)
			}
			if calls := f.agent.evidenceCalls(); len(calls) != 1 {
				t.Fatalf("live checks = %q, want the refused one alone", calls)
			}
		})
	}
}

// Parking on a refusal stops the work beside the review and keeps its log.
func TestSideBySide_StopEndsTheLanesAndKeepsTheirLogs(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{})
	started := make(chan string, 1)
	f.agent.evidenceF = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		started <- opts.CWD
		<-ctx.Done()
		return nil, ctx.Err()
	}
	startSideLanes(f.review, f.baseSHA)
	var laneDir string
	select {
	case laneDir = <-started:
	case <-time.After(sideBySideWait):
		t.Fatal("the live check did not start beside the review")
	}

	f.review.Shared.SideLanes().Stop(f.review)

	if _, err := os.Stat(laneDir); !os.IsNotExist(err) {
		t.Fatalf("the stopped copy %s is still there (stat error %v)", laneDir, err)
	}
	if lanes, _ := f.review.Shared.SideLanes().(*sideLanes); lanes == nil || lanes.claim(types.StepTest) != nil {
		t.Fatal("the stopped work was still left for the Test step")
	}
	if !strings.Contains(f.logText(), "log of the stopped test work") || !strings.Contains(f.logText(), "a native agent safety refusal stopped automatic execution") {
		t.Fatalf("log = %q, want the lane's log and the reason it stopped", f.logText())
	}
}

func TestSideBySide_RefusalPreservesLaneAtEveryExit(t *testing.T) {
	for _, mode := range []string{"take", "stale", "stop", "stop while running"} {
		t.Run(mode, func(t *testing.T) {
			f := newSideBySideFixture(t, config.Commands{})
			started := make(chan string, 1)
			scratch := make(chan string, 1)
			f.agent.evidenceF = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(filepath.Join(opts.CWD, "refused.txt"), []byte("preserved"), 0o644); err != nil {
					return nil, err
				}
				for _, entry := range opts.Env {
					if path, ok := strings.CutPrefix(entry, "TMP="); ok {
						if err := os.WriteFile(filepath.Join(path, "refused-scratch.txt"), []byte("preserved"), 0o644); err != nil {
							return nil, err
						}
						scratch <- path
						break
					}
				}
				started <- opts.CWD
				if mode == "stop while running" {
					<-ctx.Done()
				}
				return nil, errors.New(nativeSafetyRefusal)
			}
			startSideLanes(f.review, f.baseSHA)
			var dir string
			select {
			case dir = <-started:
			case <-time.After(sideBySideWait):
				t.Fatal("lane did not start")
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(dir)) })
			if mode != "stop while running" {
				f.lanesDone()
			}
			switch mode {
			case "stale":
				stopStaleSideLanes(f.review, "new-head")
			case "stop", "stop while running":
				f.review.Shared.SideLanes().Stop(f.review)
			}
			_, err := (&TestStep{}).Execute(f.turn())
			if !agent.IsSafetyRefusal(err) {
				t.Fatalf("step error = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "refused.txt"))
			if err != nil || string(data) != "preserved" {
				t.Fatalf("lane edits = %q, %v", data, err)
			}
			select {
			case path := <-scratch:
				data, err = os.ReadFile(filepath.Join(path, "refused-scratch.txt"))
				if err != nil || string(data) != "preserved" {
					t.Fatalf("scratch edits = %q, %v", data, err)
				}
			default:
				t.Fatal("agent did not receive scratch directory")
			}
			if len(f.agent.evidenceCalls()) != 1 {
				t.Fatal("refused work ran again")
			}
		})
	}
}

func TestSideBySide_RefusedDocumentLaneSurvivesStop(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Lint: "true"})
	started := make(chan string, 1)
	f.agent.documentF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "refused-doc.txt"), []byte("preserved"), 0o644); err != nil {
			return nil, err
		}
		started <- opts.CWD
		return nil, errors.New(nativeSafetyRefusal)
	}
	startSideLanes(f.review, f.baseSHA)
	var dir string
	select {
	case dir = <-started:
	case <-time.After(sideBySideWait):
		t.Fatal("document lane did not start")
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(dir)) })
	f.lanesDone()
	f.review.Shared.SideLanes().Stop(f.review)
	_, err := (&DocumentStep{}).Execute(f.turn())
	if !agent.IsSafetyRefusal(err) {
		t.Fatalf("document error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "refused-doc.txt"))
	if err != nil || string(data) != "preserved" {
		t.Fatalf("document edits = %q, %v", data, err)
	}
	if len(f.agent.documentCalls()) != 1 {
		t.Fatal("refused document work ran again")
	}
}

func TestAnalyzersDoNotRetryStructuredSafetyRefusals(t *testing.T) {
	for _, name := range []string{"review", "test"} {
		t.Run(name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "codex", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, rejectedStructuredOutputError{message: nativeSafetyRefusal}
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			var err error
			if name == "review" {
				_, err = (&ReviewStep{}).Execute(sctx)
			} else {
				_, err = runTestAnalyzer(sctx.Ctx, sctx, "test", "")
			}
			if !agent.IsSafetyRefusal(err) || calls != 1 {
				t.Fatalf("error = %v, calls = %d", err, calls)
			}
		})
	}
}

func TestIntentStepPropagatesSafetyRefusal(t *testing.T) {
	sctx := newIntentStepContext(t)
	step := &IntentStep{runIntent: func(context.Context, *pipeline.StepContext) (*intent.Result, error) {
		return nil, errors.New(nativeSafetyRefusal)
	}}
	_, err := step.Execute(sctx)
	if !agent.IsSafetyRefusal(err) {
		t.Fatalf("error = %v", err)
	}
}

func TestConflictResolverPreservesRefusedEdits(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		t.Run(strategy, func(t *testing.T) {
			dir := t.TempDir()
			gitCmd(t, dir, "init")
			gitCmd(t, dir, "config", "user.name", "test")
			gitCmd(t, dir, "config", "user.email", "test@example.com")
			gitCmd(t, dir, "checkout", "-b", "base")
			path := filepath.Join(dir, "conflict.txt")
			if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "base")
			base := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "-b", "feature")
			if err := os.WriteFile(path, []byte("feature\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "feature")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "checkout", "base")
			if err := os.WriteFile(path, []byte("upstream\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "upstream")
			gitCmd(t, dir, "checkout", "feature")
			ag := &mockAgent{name: "codex", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(path, []byte("resolver edit\n"), 0o644); err != nil {
					return nil, err
				}
				gitCmd(t, opts.CWD, "add", "conflict.txt")
				return nil, errors.New(nativeSafetyRefusal)
			}}
			sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
			var err error
			if strategy == "merge" {
				err = mergeWithAgent(sctx.Ctx, sctx, "base")
			} else {
				err = rebaseWithAgent(sctx.Ctx, sctx, "base")
			}
			if !agent.IsSafetyRefusal(err) {
				t.Fatalf("error = %v", err)
			}
			if got := gitCmd(t, dir, "show", ":conflict.txt"); got != "resolver edit" {
				t.Fatalf("staged edit = %q", got)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "resolver edit\n" {
				t.Fatalf("worktree edit = %q, %v", data, err)
			}
		})
	}
}
