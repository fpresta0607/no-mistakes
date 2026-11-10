package steps

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A repair round's rereview used to judge the whole branch again: in one
// measured week of gate records, rereviews after a fix round were 45% of the
// review spend and cost as much as a first review. These tests pin the
// narrower rule: the rereview judges what the fix changed, the files of the
// open findings, and whatever the round before left unjudged,
// and every other changed file keeps that round's verdict.

const carriedVerdictMarker = "Changed files whose verdict carries from the previous review round"

func TestFixRoundCarriedPaths(t *testing.T) {
	t.Parallel()
	reviewable := []string{"a.go", "b.go", "c.go"}
	cases := []struct {
		name               string
		previouslyReviewed []string
		changedSinceStart  []string
		sentToFix          []Finding
		deferred           []Finding
		want               []string
	}{
		{
			name:               "keeps every covered file the fix left alone",
			previouslyReviewed: []string{"a.go", "b.go", "c.go"},
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "a.go"}},
			want:               []string{"b.go", "c.go"},
		},
		{
			name:               "judges again a finding's file the fix did not change",
			previouslyReviewed: []string{"a.go", "b.go", "c.go"},
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "./b.go"}},
			want:               []string{"c.go"},
		},
		{
			name:               "judges again a file the previous round left uncovered",
			previouslyReviewed: []string{"a.go", "b.go"},
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "a.go"}},
			want:               []string{"b.go"},
		},
		{
			name:               "a finding with no file keeps nothing",
			previouslyReviewed: []string{"a.go", "b.go", "c.go"},
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "a.go"}, {File: ""}},
			want:               nil,
		},
		{
			name:               "judges a deferred finding's file",
			previouslyReviewed: []string{"a.go", "b.go", "c.go"},
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "a.go"}},
			deferred:           []Finding{{File: "b.go"}},
			want:               []string{"c.go"},
		},
		{
			name:               "a fileless deferred finding keeps nothing",
			previouslyReviewed: []string{"a.go", "b.go", "c.go"},
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "a.go"}},
			deferred:           []Finding{{}},
		},
		{
			name:               "an unknown previous record keeps nothing",
			previouslyReviewed: nil,
			changedSinceStart:  []string{"a.go"},
			sentToFix:          []Finding{{File: "a.go"}},
			want:               nil,
		},
		{
			name:               "a round with nothing of its own to judge reviews in full",
			previouslyReviewed: []string{"a.go", "b.go", "c.go"},
			changedSinceStart:  []string{"ignored.lock"},
			sentToFix:          []Finding{{File: "ignored.lock"}},
			want:               nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fixRoundCarriedPaths(reviewable, tc.previouslyReviewed, tc.changedSinceStart, append(tc.sentToFix, tc.deferred...))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fixRoundCarriedPaths() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fixRoundScopeHarness runs the real review step under the executor on a
// branch that changes two files, feature.txt and feature2.txt.
func fixRoundScopeHarness(t *testing.T, mock *sessionMockAgent) (*pipeline.Executor, *db.DB, *db.Run, *db.Repo, string) {
	t.Helper()
	workDir, baseSHA, headSHA := twoFileRepo(t)
	return fixRoundScopeHarnessOn(t, mock, workDir, baseSHA, headSHA)
}

func fixRoundScopeHarnessOn(t *testing.T, mock *sessionMockAgent, workDir, baseSHA, headSHA string) (*pipeline.Executor, *db.DB, *db.Run, *db.Repo, string) {
	t.Helper()
	ensureHermeticOrigin(t, workDir)
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	cfg := &config.Config{Agent: types.AgentClaude, AutoFix: config.AutoFix{Review: 3}}
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, mock, []pipeline.Step{&ReviewStep{}}, nil)
	return exec, database, run, repo, workDir
}

// executeWithoutAPark runs the executor and fails the test, aborting the run,
// when the review parks for a decision instead of completing on its own. Abort
// is refused until a gate is waiting, so its acceptance is the park itself and a
// slow machine reads as slow.
func executeWithoutAPark(t *testing.T, exec *pipeline.Executor, run *db.Run, repo *db.Repo, workDir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			return
		case <-time.After(50 * time.Millisecond):
			if exec.Respond(types.StepReview, types.ActionAbort, nil) == nil {
				<-done
				t.Fatal("the review parked for a decision instead of completing")
			}
		}
	}
}

// rewriteFeatureFix is a fixer turn that really changes feature.txt.
func rewriteFeatureFix(t *testing.T, opts agent.RunOpts) *agent.Result {
	if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("feature code, fixed\n"), 0o644); err != nil {
		t.Errorf("write fix: %v", err)
	}
	return &agent.Result{Output: []byte(`{"summary":"fix the bug"}`)}
}

func TestReviewLoop_RepairRoundReviewsOnlyWhatTheFixChanged(t *testing.T) {
	reviewTurn := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			if reviewTurn == 1 {
				return &agent.Result{Output: []byte(`{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"bug","action":"auto-fix"}],"summary":"1 issue","risk_level":"medium","risk_rationale":"bug","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`)}
			}
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`)}
		case "review-fix":
			return rewriteFeatureFix(t, opts)
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	exec, database, run, repo, workDir := fixRoundScopeHarness(t, mock)

	executeWithoutAPark(t, exec, run, repo, workDir)

	calls := mock.snapshot()
	if got := len(fixCalls(calls)); got != 1 {
		t.Fatalf("fix turns = %d, want 1", got)
	}
	reviews := reviewCalls(calls)
	if len(reviews) != 2 {
		t.Fatalf("review turns = %d, want the initial review and one rereview", len(reviews))
	}
	for _, call := range calls {
		if call.Purpose == "review-coverage" {
			t.Fatalf("the rereview covered everything it was held to, yet a coverage completion turn ran:\n%s", call.Prompt)
		}
	}
	rereview := reviews[1].Prompt
	held, carried, ok := strings.Cut(rereview, carriedVerdictMarker)
	if !ok {
		t.Fatalf("the rereview names no carried verdicts:\n%s", rereview)
	}
	if !strings.Contains(held, "- feature.txt\n") || strings.Contains(held, "- feature2.txt\n") {
		t.Fatalf("the rereview must be held to the fixed file alone:\n%s", held)
	}
	if !strings.Contains(carried, "- feature2.txt\n") {
		t.Fatalf("the rereview must carry the untouched file's verdict:\n%s", carried)
	}
	if strings.Contains(reviews[0].Prompt, carriedVerdictMarker) {
		t.Fatalf("the initial review must judge the whole branch:\n%s", reviews[0].Prompt)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("review step = %+v, want completed without a park", steps)
	}
}

func TestReviewLoop_RepairRoundJudgesWhatThePreviousRoundLeftUncovered(t *testing.T) {
	reviewTurn := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			if reviewTurn == 1 {
				// A round with a blocking finding skips the coverage completion,
				// so its record can stay partial: feature2.txt was never judged.
				return &agent.Result{Output: []byte(`{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"bug","action":"auto-fix"}],"summary":"1 issue","risk_level":"medium","risk_rationale":"bug","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`)}
			}
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`)}
		case "review-fix":
			return rewriteFeatureFix(t, opts)
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	exec, _, run, repo, workDir := fixRoundScopeHarness(t, mock)

	executeWithoutAPark(t, exec, run, repo, workDir)

	reviews := reviewCalls(mock.snapshot())
	if len(reviews) != 2 {
		t.Fatalf("review turns = %d, want the initial review and one rereview", len(reviews))
	}
	rereview := reviews[1].Prompt
	if strings.Contains(rereview, carriedVerdictMarker) {
		t.Fatalf("no verdict may carry for a file the previous round never judged:\n%s", rereview)
	}
	for _, want := range []string{"- feature.txt\n", "- feature2.txt\n"} {
		if !strings.Contains(rereview, want) {
			t.Fatalf("the rereview must be held to %q:\n%s", strings.TrimSpace(want), rereview)
		}
	}
}

func TestReviewLoop_DeferredFindingKeepsPendingFindingUnverified(t *testing.T) {
	reviewTurn, fixTurn := 0, 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			switch reviewTurn {
			case 1:
				return &agent.Result{Output: []byte(`{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"original bug","action":"auto-fix"}],"summary":"bug","risk_level":"medium","risk_rationale":"bug","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`)}
			case 2:
				return &agent.Result{Output: []byte(`{"findings":[{"id":"note","severity":"info","file":"feature.txt","line":1,"description":"still relevant","action":"no-op"},{"id":"f-2","severity":"error","file":"feature2.txt","line":1,"description":"second bug","action":"auto-fix","failing_input":"an empty name","wrong_result":"the record is lost"}],"summary":"issues","risk_level":"medium","risk_rationale":"issues","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`)}
			default:
				return &agent.Result{Output: []byte(`{"findings":[{"id":"decision","severity":"warning","file":"feature2.txt","line":1,"description":"needs decision","action":"ask-user"}],"summary":"decision","risk_level":"medium","risk_rationale":"decision","risk_scope":"source-or-external","reviewed_paths":["feature2.txt"]}`)}
			}
		case "review-fix":
			fixTurn++
			if fixTurn == 1 {
				return rewriteFeatureFix(t, opts)
			}
			if err := os.WriteFile(filepath.Join(opts.CWD, "feature2.txt"), []byte("second fix\n"), 0o644); err != nil {
				t.Errorf("write second fix: %v", err)
			}
			return &agent.Result{Output: []byte(`{"summary":"fix second bug"}`)}
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	exec, database, run, repo, workDir := fixRoundScopeHarness(t, mock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	var parked *db.StepResult
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(run.ID)
		if err == nil && len(steps) == 1 && (steps[0].Status == types.StepStatusFixReview || steps[0].Status == types.StepStatusAwaitingApproval) {
			parked = steps[0]
			break
		}
		select {
		case err := <-done:
			t.Fatalf("executor ended before parking: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if parked == nil {
		t.Fatal("review did not park")
	}
	cancel()
	<-done
	reviews := reviewCalls(mock.snapshot())
	if len(reviews) != 3 {
		t.Fatalf("review turns = %d, want 3", len(reviews))
	}
	held, carried, _ := strings.Cut(reviews[2].Prompt, carriedVerdictMarker)
	if !strings.Contains(held, "- feature.txt\n") || strings.Contains(carried, "- feature.txt\n") {
		t.Fatalf("deferred finding's file must be held for review, not carried: %s", reviews[2].Prompt)
	}
	if !strings.Contains(reviews[2].Prompt, "f-1: feature.txt:1: original bug") {
		t.Fatalf("a round that can clear an earlier selection must be asked to check it: %s", reviews[2].Prompt)
	}
	if strings.Contains(reviews[2].Prompt, "note: feature.txt:1: still relevant") {
		t.Fatalf("a finding no repair was sent to close is not a check for this round: %s", reviews[2].Prompt)
	}
	if parked.FindingsJSON == nil || !strings.Contains(*parked.FindingsJSON, "original bug") {
		t.Fatalf("unverified original finding disappeared: %+v", parked.FindingsJSON)
	}
}

func TestReviewLoop_RepairScopeIncludesUnchangedHeldFile(t *testing.T) {
	reviewTurn := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			if reviewTurn == 1 {
				return &agent.Result{Output: []byte(`{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"bug","action":"auto-fix"}],"summary":"bug","risk_level":"medium","risk_rationale":"bug","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature3.txt"]}`)}
			}
			return &agent.Result{Output: []byte(`{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`)}
		case "review-fix":
			return rewriteFeatureFix(t, opts)
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	exec, _, run, repo, workDir := fixRoundScopeHarness(t, mock)
	if err := os.WriteFile(filepath.Join(workDir, "feature3.txt"), []byte("third feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "feature3.txt")
	gitCmd(t, workDir, "commit", "-m", "add third feature")
	run.HeadSHA = gitCmd(t, workDir, "rev-parse", "HEAD")
	startingHead := run.HeadSHA
	executeWithoutAPark(t, exec, run, repo, workDir)
	reviews := reviewCalls(mock.snapshot())
	if len(reviews) != 2 {
		t.Fatalf("review turns = %d, want 2", len(reviews))
	}
	prompt := reviews[1].Prompt
	held, carried, ok := strings.Cut(prompt, carriedVerdictMarker)
	if !ok || !strings.Contains(held, "- feature2.txt\n") || !strings.Contains(carried, "- feature3.txt\n") {
		t.Fatalf("uncovered file must be held and covered untouched file carried: %s", prompt)
	}
	want := "only this repair's change since starting head " + startingHead + ", plus the branch changes relative to base commit " + run.BaseSHA + " in the files no earlier round judged"
	if !strings.Contains(prompt, want) {
		t.Fatalf("review scope must name the repair and the files never judged, want %q in %s", want, prompt)
	}
}
