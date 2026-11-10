package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A repair that reached its agent budget used to fail the whole run, with the
// finished Review and repair work going with it, even when the agent was still
// producing output. These tests pin the park that replaced that failure.

const (
	repairCutFindingOutput = `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"bug","action":"auto-fix"}],"summary":"1 issue","risk_level":"medium","risk_rationale":"r","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`
	repairCutCleanOutput   = `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt","helper.txt"]}`
	repairCutAskOutput     = `{"findings":[{"id":"f-2","severity":"warning","file":"helper.txt","line":1,"description":"a person's call","action":"ask-user"}],"summary":"1 issue","risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt","helper.txt"]}`
)

// repairCutLoop scripts a review whose first repair turn runs into its budget
// after leaving a tracked edit and a new file. Later repair turns finish.
type repairCutLoop struct {
	mu             sync.Mutex
	isStreaming    bool
	commitsWork    bool
	mergesWork     bool
	rereviewOutput string
	reviews        []string
	repairs        []string
}

func (l *repairCutLoop) agent(t *testing.T) *mockAgent {
	return &mockAgent{name: "repair-cut", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		switch opts.Purpose {
		case "review":
			l.reviews = append(l.reviews, opts.Prompt)
			if len(l.reviews) == 1 {
				return &agent.Result{Output: json.RawMessage(repairCutFindingOutput)}, nil
			}
			if l.rereviewOutput != "" {
				return &agent.Result{Output: json.RawMessage(l.rereviewOutput)}, nil
			}
			return &agent.Result{Output: json.RawMessage(repairCutCleanOutput)}, nil
		case "review-fix":
			l.repairs = append(l.repairs, opts.Prompt)
			if len(l.repairs) > 1 {
				return rewriteFeatureFix(t, opts), nil
			}
			return l.cutTurn(ctx, t, opts)
		default:
			t.Errorf("unexpected agent turn %q", opts.Purpose)
			return &agent.Result{}, nil
		}
	}}
}

func (l *repairCutLoop) cutTurn(ctx context.Context, t *testing.T, opts agent.RunOpts) (*agent.Result, error) {
	for name, content := range map[string]string{"feature.txt": "feature code, half fixed\n", "helper.txt": "half a helper\n"} {
		if err := os.WriteFile(filepath.Join(opts.CWD, name), []byte(content), 0o644); err != nil {
			t.Errorf("write partial work: %v", err)
		}
	}
	if l.commitsWork {
		gitCmd(t, opts.CWD, "add", "feature.txt", "helper.txt")
		gitCmd(t, opts.CWD, "commit", "-m", "half a repair\n\nAgent-Note: unshaped")
	}
	if l.mergesWork {
		start := gitCmd(t, opts.CWD, "rev-parse", "HEAD")
		gitCmd(t, opts.CWD, "add", "helper.txt")
		gitCmd(t, opts.CWD, "commit", "-m", "half a helper")
		side := gitCmd(t, opts.CWD, "commit-tree", start+"^{tree}", "-p", start, "-m", "side")
		gitCmd(t, opts.CWD, "merge", "--no-ff", "--no-edit", side)
	}
	if !l.isStreaming {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	// Each chunk ends a line, so the executor writes step activity to the
	// database for every one. A paced stream keeps that synchronous path from
	// stalling the turn on a slow runner past the idle window it must stay in.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			opts.OnChunk("repairing\n")
		}
	}
}

func (l *repairCutLoop) turns() (reviews, repairs []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reviews...), append([]string(nil), l.repairs...)
}

type repairCutRun struct {
	exec     *pipeline.Executor
	database *db.DB
	run      *db.Run
	workDir  string
	headSHA  string
	done     chan error
}

// startRepairCutRun runs the real review step under the executor until the
// first repair turn has been cut and the gate is parked. The worktree already
// holds scratch.txt, an untracked file no repair turn wrote.
func startRepairCutRun(t *testing.T, loop *repairCutLoop, cfg *config.Config) *repairCutRun {
	t.Helper()
	workDir, baseSHA, headSHA := twoFileRepo(t)
	ensureHermeticOrigin(t, workDir)
	if err := os.WriteFile(filepath.Join(workDir, "scratch.txt"), []byte("left by an earlier step\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	cfg.Agent = types.AgentClaude
	cfg.AutoFix = config.AutoFix{Review: 3}
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, loop.agent(t), []pipeline.Step{&ReviewStep{}}, nil)

	// No deadline on this context: an agent turn honors a parent's deadline and
	// binds no budget of its own.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	waitForReviewStatus(t, database, run.ID, types.StepStatusFixReview)
	return &repairCutRun{exec: exec, database: database, run: run, workDir: workDir, headSHA: headSHA, done: done}
}

func (r *repairCutRun) gateFindings(t *testing.T) types.Findings {
	t.Helper()
	steps, err := r.database.GetStepsByRun(r.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview && step.FindingsJSON != nil {
			findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			return findings
		}
	}
	t.Fatal("the review step holds no findings")
	return types.Findings{}
}

func (r *repairCutRun) finish(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Minute):
		t.Fatal("executor did not finish")
	}
}

func repairCutNotice(t *testing.T, findings types.Findings) types.Finding {
	t.Helper()
	for _, finding := range findings.Items {
		if finding.ID == types.FindingIDRepairAgentBudget {
			return finding
		}
	}
	t.Fatalf("findings %+v hold no repair budget notice", findings.Items)
	return types.Finding{}
}

func TestReviewLoop_RepairAtItsBudgetParksWithItsWorkKept(t *testing.T) {
	cases := []struct {
		name      string
		loop      *repairCutLoop
		cfg       *config.Config
		wantBound string
	}{
		{
			name:      "a silent repair",
			loop:      &repairCutLoop{},
			cfg:       &config.Config{ReviewAgentTimeout: 500 * time.Millisecond},
			wantBound: "silent budget",
		},
		{
			name:      "a repair still producing output",
			loop:      &repairCutLoop{isStreaming: true},
			cfg:       &config.Config{ReviewAgentTimeout: 4 * time.Second, ReviewAgentWorkingTimeout: 5 * time.Second},
			wantBound: "still-working cap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := startRepairCutRun(t, tc.loop, tc.cfg)

			findings := r.gateFindings(t)
			notice := repairCutNotice(t, findings)
			err := r.exec.Respond(types.StepReview, types.ActionApprove, nil)
			stored, getErr := r.database.GetRun(r.run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			status := gitStatusPorcelain(t, r.workDir)
			head := gitCmd(t, r.workDir, "rev-parse", "HEAD")
			abortErr := r.exec.Respond(types.StepReview, types.ActionAbort, nil)
			<-r.done

			if stored.Status != types.RunRunning || stored.Error != nil {
				t.Fatalf("run = %s, error %v, want it parked and not failed", stored.Status, stored.Error)
			}
			if !strings.Contains(notice.Description, tc.wantBound) {
				t.Errorf("notice = %q, want the bound %q that cut the turn", notice.Description, tc.wantBound)
			}
			if notice.Action != types.ActionAskUser || notice.Severity != types.FindingSeverityWarning {
				t.Errorf("notice = %+v, want an ask-user warning", notice)
			}
			for _, path := range []string{"feature.txt", "helper.txt"} {
				if !strings.Contains(notice.Description, path) {
					t.Errorf("notice = %q, want %s named as left work", notice.Description, path)
				}
				if !strings.Contains(status, path) {
					t.Errorf("worktree status = %q, want %s kept", status, path)
				}
			}
			if strings.Contains(notice.Description, "scratch.txt") {
				t.Errorf("notice = %q, want a file no repair turn wrote left out of its work", notice.Description)
			}
			if head != r.headSHA {
				t.Errorf("HEAD = %s, want the cut turn's work left uncommitted at %s", head, r.headSHA)
			}
			var hasOpenFinding bool
			for _, finding := range findings.Items {
				hasOpenFinding = hasOpenFinding || finding.ID == "f-1"
			}
			if !hasOpenFinding {
				t.Errorf("findings = %+v, want the finding the cut turn was sent to close still open", findings.Items)
			}
			if err == nil || !strings.Contains(err.Error(), "agent budget") {
				t.Errorf("approve = %v, want it refused at a cut", err)
			}
			if abortErr != nil {
				t.Errorf("abort: %v", abortErr)
			}
		})
	}
}

func TestReviewLoop_FixOfOnlyTheCutChecksItsWorkWithoutARepairTurn(t *testing.T) {
	loop := &repairCutLoop{}
	r := startRepairCutRun(t, loop, &config.Config{ReviewAgentTimeout: 500 * time.Millisecond})

	if err := r.exec.Respond(types.StepReview, types.ActionFix, []string{types.FindingIDRepairAgentBudget}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	r.finish(t)

	reviews, repairs := loop.turns()
	if len(repairs) != 1 {
		t.Fatalf("repair turns = %d, want only the cut one", len(repairs))
	}
	if len(reviews) != 2 {
		t.Fatalf("review turns = %d, want the first and one over the kept work", len(reviews))
	}
	if strings.Contains(reviews[1], carriedVerdictMarker) {
		t.Fatalf("the rereview carried a verdict over work no round read:\n%s", reviews[1])
	}
	if strings.Contains(reviews[1], types.FindingIDRepairAgentBudget) {
		t.Fatalf("the rereview was handed the budget notice as work:\n%s", reviews[1])
	}
	if strings.Contains(reviews[1], "declined: ") {
		t.Fatalf("the rereview was told the operator declined a finding nobody ruled on:\n%s", reviews[1])
	}
	committed := gitCmd(t, r.workDir, "show", "--name-only", "--format=", "HEAD")
	for _, path := range []string{"feature.txt", "helper.txt"} {
		if !strings.Contains(committed, path) {
			t.Fatalf("HEAD holds %q, want the kept %s in it", committed, path)
		}
	}
	if status := gitStatusPorcelain(t, r.workDir); status != "?? scratch.txt" {
		t.Fatalf("worktree status = %q, want only the file no repair turn wrote left out", status)
	}
}

// The notice reports one turn. Once a round has read the kept work, a later gate
// answers for that round's findings alone and Approve is open again, even when
// the operator declined the notice, which no coverage record would ever clear.
func TestReviewLoop_ANoticeDoesNotOutliveTheRoundThatCheckedTheWork(t *testing.T) {
	notice := types.FindingIDRepairAgentBudget
	cases := []struct {
		name   string
		fix    []string
		ignore []string
	}{
		{name: "the notice selected", fix: []string{notice}},
		{name: "the notice declined beside a fix", fix: []string{"f-1"}, ignore: []string{notice}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := &repairCutLoop{rereviewOutput: repairCutAskOutput}
			r := startRepairCutRun(t, loop, &config.Config{ReviewAgentTimeout: 500 * time.Millisecond})

			if _, err := r.exec.RespondWithOverrides(types.StepReview, types.ActionFix, tc.fix, tc.ignore, nil, nil, ""); err != nil {
				t.Fatalf("fix: %v", err)
			}
			deadline := time.Now().Add(time.Minute)
			for reviews, _ := loop.turns(); len(reviews) < 2; reviews, _ = loop.turns() {
				if time.Now().After(deadline) {
					t.Fatal("the kept work was never reviewed")
				}
				time.Sleep(10 * time.Millisecond)
			}
			waitForReviewStatus(t, r.database, r.run.ID, types.StepStatusFixReview)
			findings := r.gateFindings(t)
			approveErr := r.exec.Respond(types.StepReview, types.ActionApprove, nil)
			if approveErr != nil {
				_ = r.exec.Respond(types.StepReview, types.ActionAbort, nil)
			}
			<-r.done

			var hasRoundFinding bool
			for _, finding := range findings.Items {
				if finding.ID == notice {
					t.Fatalf("findings = %+v, want the notice gone once a round read the work", findings.Items)
				}
				hasRoundFinding = hasRoundFinding || finding.Description == "a person's call"
			}
			if !hasRoundFinding {
				t.Fatalf("findings = %+v, want the rereview's own finding", findings.Items)
			}
			if approveErr != nil {
				t.Fatalf("approve = %v, want it open once a round read the work", approveErr)
			}
		})
	}
}

func TestReviewLoop_FixOfAFindingAtACutContinuesFromTheKeptWork(t *testing.T) {
	loop := &repairCutLoop{}
	r := startRepairCutRun(t, loop, &config.Config{ReviewAgentTimeout: 500 * time.Millisecond})

	if err := r.exec.Respond(types.StepReview, types.ActionFix, []string{"f-1", types.FindingIDRepairAgentBudget}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	r.finish(t)

	_, repairs := loop.turns()
	if len(repairs) != 2 {
		t.Fatalf("repair turns = %d, want a second one for f-1", len(repairs))
	}
	if strings.Contains(repairs[1], types.FindingIDRepairAgentBudget) {
		t.Fatalf("the repair turn was handed the budget notice as work:\n%s", repairs[1])
	}
	if !strings.Contains(repairs[1], "f-1") {
		t.Fatalf("the repair turn lost the finding it was sent to close:\n%s", repairs[1])
	}
	committed := gitCmd(t, r.workDir, "show", "--name-only", "--format=", "HEAD")
	if !strings.Contains(committed, "helper.txt") {
		t.Fatalf("HEAD holds %q, want the file the cut turn made shipped with the repair", committed)
	}
}

func TestReviewLoop_ACommitTheCutTurnMadeBecomesTheRunHead(t *testing.T) {
	loop := &repairCutLoop{commitsWork: true}
	r := startRepairCutRun(t, loop, &config.Config{
		ReviewAgentTimeout: 500 * time.Millisecond,
		Commit:             config.Commit{FixMessage: "repair({{.Step}}): {{.Summary}}", Trailers: []string{"Assisted-by: {{.Agent}}"}},
	})

	notice := repairCutNotice(t, r.gateFindings(t))
	stored, err := r.database.GetRun(r.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, r.workDir, "rev-parse", "HEAD")
	if err := r.exec.Respond(types.StepReview, types.ActionFix, []string{types.FindingIDRepairAgentBudget}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	r.finish(t)

	if head == r.headSHA {
		t.Fatal("the cut turn's commit was lost")
	}
	if stored.HeadSHA != head {
		t.Fatalf("run head = %s, want the cut turn's commit %s recorded", stored.HeadSHA, head)
	}
	if !strings.Contains(notice.Description, "log -p "+r.headSHA+".."+head) {
		t.Fatalf("notice = %q, want the commits named with how to inspect them", notice.Description)
	}
	// No invocation reported which agent made the commit, so the documented
	// trailer rule adds none rather than a guessed agent.
	if got, want := gitCmd(t, r.workDir, "log", "--format=%B", r.headSHA+".."+head), "repair(review): keep repair work cut at its agent budget"; got != want {
		t.Fatalf("cut turn's commit message = %q, want the commit settings' %q", got, want)
	}
}

func TestReviewLoop_AMergeTheCutTurnMadeStaysUnrecorded(t *testing.T) {
	loop := &repairCutLoop{mergesWork: true}
	r := startRepairCutRun(t, loop, &config.Config{ReviewAgentTimeout: 500 * time.Millisecond})

	notice := repairCutNotice(t, r.gateFindings(t))
	stored, err := r.database.GetRun(r.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, r.workDir, "rev-parse", "HEAD")
	parents := strings.Fields(gitCmd(t, r.workDir, "rev-list", "--parents", "-n", "1", "HEAD"))
	abortErr := r.exec.Respond(types.StepReview, types.ActionAbort, nil)
	<-r.done

	if len(parents) != 3 {
		t.Fatalf("HEAD parents = %v, want the cut turn's merge commit left in the worktree", parents)
	}
	if stored.HeadSHA != r.headSHA {
		t.Fatalf("run head = %s, want the merge left unrecorded at %s", stored.HeadSHA, r.headSHA)
	}
	if !strings.Contains(notice.Description, "not recorded as the run head") || !strings.Contains(notice.Description, "log -p "+r.headSHA+".."+head) {
		t.Fatalf("notice = %q, want the unrecorded commits named with how to inspect them", notice.Description)
	}
	if abortErr != nil {
		t.Fatalf("abort: %v", abortErr)
	}
}

func TestRepairTurnAtItsBudgetIsCutWithoutCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		step pipeline.Step
		cmds config.Commands
	}{
		{name: "lint", step: &LintStep{}, cmds: config.Commands{Lint: "exit 0"}},
		{name: "custom gate", step: &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepTest, Command: "exit 0"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)
			ag := &mockAgent{name: "cut-repair", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("half"), 0o644); err != nil {
					return nil, err
				}
				<-ctx.Done()
				return &agent.Result{Output: json.RawMessage(`{"summary":"finished late"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, tc.cmds)
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"id":"g-1","severity":"error","description":"failed"}],"summary":"failed"}`
			sctx.Config.AgentTimeout = 20 * time.Millisecond

			_, err := tc.step.Execute(sctx)

			outcome := pipeline.RepairBudgetOutcome(err)
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("Execute() = %v, want a cut the executor parks", err)
			}
			if !strings.Contains(outcome.Findings, "repair.txt") {
				t.Fatalf("findings = %s, want the left file named", outcome.Findings)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
			}
		})
	}
}

// Only a budget stops a repair this way. Any other failure of the turn keeps
// failing the step, and a Test repair keeps its own park.
func TestRepairTurnFailingForAnotherReasonIsNoCut(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "broken-repair", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, os.ErrPermission
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Fixing = true

	_, err := (&LintStep{}).Execute(sctx)

	if err == nil || pipeline.RepairBudgetOutcome(err) != nil {
		t.Fatalf("Execute() = %v, want a failure the executor does not park", err)
	}
}
