package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const sideBySideWait = 2 * time.Minute

// sideBySideAgent is a stand-in harness that is safe to call from the run and
// from its side lanes at once. It tells the evidence turn from the
// documentation pass by their prompts and records where each call ran.
type sideBySideAgent struct {
	mu        sync.Mutex
	evidence  []string // CWD of each evidence turn
	document  []string // CWD of each documentation pass
	prompts   []string
	onCall    func(kind, cwd string)
	evidenceF func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
	documentF func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)
}

func (a *sideBySideAgent) Name() string { return "claude" }
func (a *sideBySideAgent) Close() error { return nil }

func (a *sideBySideAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	kind := "document"
	if strings.Contains(opts.Prompt, "You are validating a code change by driving the product itself") {
		kind = "evidence"
	}
	a.mu.Lock()
	a.prompts = append(a.prompts, opts.Prompt)
	if kind == "evidence" {
		a.evidence = append(a.evidence, opts.CWD)
	} else {
		a.document = append(a.document, opts.CWD)
	}
	onCall, evidenceF, documentF := a.onCall, a.evidenceF, a.documentF
	a.mu.Unlock()
	if onCall != nil {
		onCall(kind, opts.CWD)
	}
	if kind == "evidence" {
		if evidenceF != nil {
			return evidenceF(ctx, opts)
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}
	if documentF != nil {
		return documentF(ctx, opts)
	}
	return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document the feature"}`)}, nil
}

func (a *sideBySideAgent) evidenceCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.evidence...)
}

func (a *sideBySideAgent) documentCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.document...)
}

// sideBySideFixture is a run's worktree with side_by_side on, the context the
// review step would have, and a counter of how often the baseline ran.
type sideBySideFixture struct {
	t       *testing.T
	dir     string
	baseSHA string
	headSHA string
	agent   *sideBySideAgent
	review  *pipeline.StepContext
	counter string
	logMu   sync.Mutex
	logs    []string
}

func newSideBySideFixture(t *testing.T, cmds config.Commands) *sideBySideFixture {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	f := &sideBySideFixture{t: t, dir: dir, baseSHA: baseSHA, headSHA: headSHA, agent: &sideBySideAgent{}}
	f.counter = filepath.Join(t.TempDir(), "baseline-runs.txt")
	if cmds.Test == "count" {
		// Unquoted: on Windows the command reaches cmd.exe /c with any inner
		// quotes escaped, which cmd.exe reads as part of the file name.
		cmds.Test = "echo ran>> " + f.counter
	}
	sctx := newTestContextWithDBRecords(t, f.agent, dir, baseSHA, headSHA, cmds)
	sctx.Config.SideBySide = true
	sctx.Shared = &pipeline.RunShared{}
	t.Cleanup(sctx.Shared.Close)
	sctx.Log = f.log
	sctx.LogFile = f.log
	stepIDs := map[types.StepName]string{}
	for _, name := range []types.StepName{types.StepTest, types.StepDocument} {
		sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		stepIDs[name] = sr.ID
	}
	// What the executor offers the review step under side_by_side.
	sctx.SideLaneContext = func(name types.StepName) (*pipeline.StepContext, bool) {
		id, ok := stepIDs[name]
		if !ok {
			return nil, false
		}
		runCopy := *sctx.Run
		return &pipeline.StepContext{
			Ctx: sctx.Ctx, Run: &runCopy, Repo: sctx.Repo, Agent: sctx.Agent, Config: sctx.Config, DB: sctx.DB,
			StepResultID: id, Shared: &pipeline.RunShared{}, EvidenceDir: sctx.EvidenceDir, SideLane: true,
			Log: func(string) {}, LogChunk: func(string) {}, LogFile: func(string) {},
		}, true
	}
	f.review = sctx
	return f
}

func (f *sideBySideFixture) log(text string) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	f.logs = append(f.logs, text)
}

func (f *sideBySideFixture) logText() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return strings.Join(f.logs, "\n")
}

// turn is the context a later step has at its own turn: the run's worktree
// and the run's shared state.
func (f *sideBySideFixture) turn() *pipeline.StepContext {
	sctx := *f.review
	sctx.SideLaneContext = nil
	return &sctx
}

func (f *sideBySideFixture) baselineRuns() int {
	data, err := os.ReadFile(f.counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return len(strings.Fields(string(data)))
}

// lanesDone waits until every lane the review started has finished its step.
func (f *sideBySideFixture) lanesDone() {
	f.t.Helper()
	lanes, _ := f.review.Shared.SideLanes().(*sideLanes)
	if lanes == nil {
		return
	}
	lanes.mu.Lock()
	var pending []*sideLane
	for _, lane := range lanes.lanes {
		pending = append(pending, lane)
	}
	lanes.mu.Unlock()
	for _, lane := range pending {
		select {
		case <-lane.done:
		case <-time.After(sideBySideWait):
			f.t.Fatal("a side lane did not finish")
		}
	}
}

// commitInRun makes a commit in the run's worktree the way a review fix
// round does, and records it as the run's head.
func (f *sideBySideFixture) commitInRun(name, content, message string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.dir, name)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	gitCmd(f.t, f.dir, "add", "-A")
	gitCmd(f.t, f.dir, "commit", "-m", message)
	head := gitCmd(f.t, f.dir, "rev-parse", "HEAD")
	f.review.Run.HeadSHA = head
	return head
}

func samePath(a, b string) bool {
	aa, _ := filepath.EvalSymlinks(a)
	bb, _ := filepath.EvalSymlinks(b)
	if aa == "" {
		aa = a
	}
	if bb == "" {
		bb = b
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// The Test step's work starts when the review starts, in a copy of the
// worktree, and the step takes that work at its own turn: the baseline ran
// once and the live check once, both before the step's turn began.
func TestSideBySide_TestWorkStartsBesideTheReviewAndIsTakenAtItsTurn(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Test: "count"})
	started := make(chan string, 1)
	f.agent.onCall = func(kind, cwd string) {
		if kind == "evidence" {
			select {
			case started <- cwd:
			default:
			}
		}
	}

	startSideLanes(f.review, f.baseSHA)
	// The review is still "running" here: nothing has called the Test step.
	var laneDir string
	select {
	case laneDir = <-started:
	case <-time.After(sideBySideWait):
		t.Fatal("the live check did not start beside the review")
	}
	if samePath(laneDir, f.dir) {
		t.Fatalf("the live check started in the run's own worktree %s, want a copy", laneDir)
	}
	if got := f.baselineRuns(); got != 1 {
		t.Fatalf("baseline runs before the Test step's turn = %d, want 1", got)
	}
	f.lanesDone()

	outcome, err := (&TestStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := f.baselineRuns(); got != 1 {
		t.Fatalf("baseline runs = %d, want 1: the step takes the work already done", got)
	}
	if calls := f.agent.evidenceCalls(); len(calls) != 1 {
		t.Fatalf("live check calls = %d, want 1", len(calls))
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the passing result of the work started early", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if findings.Verdict != types.TestVerdictGo || findings.TestedHeadSHA != f.headSHA {
		t.Fatalf("verdict = %q tested head = %q, want go on %s", findings.Verdict, findings.TestedHeadSHA, f.headSHA)
	}
	if head, passed, recorded := f.review.Shared.Baseline(); !recorded || !passed || head != f.headSHA {
		t.Fatalf("recorded baseline = %q, %v, %v, want the tested head as passed", head, passed, recorded)
	}
	if !strings.Contains(f.logText(), "took the test result") {
		t.Fatalf("log = %q, want it to say the result was taken", f.logText())
	}
	if _, err := os.Stat(laneDir); !os.IsNotExist(err) {
		t.Fatalf("the copy %s is still there after its result was taken (stat error %v)", laneDir, err)
	}
	// The run itself was never touched by the lane.
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != f.headSHA {
		t.Fatalf("run worktree HEAD = %s, want %s", got, f.headSHA)
	}
	if status := gitStatusPorcelain(t, f.dir); status != "" {
		t.Fatalf("run worktree status = %q, want clean", status)
	}
}

// With the setting absent nothing starts early: today's behavior.
func TestSideBySide_OffStartsNothingBesideTheReview(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Test: "count"})
	f.review.Config.SideBySide = false

	startSideLanes(f.review, f.baseSHA)
	if f.review.Shared.SideLanes() != nil {
		t.Fatal("side lanes were set up with side_by_side off")
	}
	if got := f.baselineRuns(); got != 0 {
		t.Fatalf("baseline runs before the Test step's turn = %d, want 0", got)
	}
	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	calls := f.agent.evidenceCalls()
	if len(calls) != 1 || !samePath(calls[0], f.dir) {
		t.Fatalf("live check ran in %q, want once, in the run's worktree", calls)
	}
	if got := f.baselineRuns(); got != 1 {
		t.Fatalf("baseline runs = %d, want 1", got)
	}
	if strings.Contains(f.logText(), "side by side") {
		t.Fatalf("log = %q, want no word of side by side", f.logText())
	}
}

// A review fix moves the head. Work done on the commit before it is thrown
// away and the Test step runs on the new head, as it does today.
func TestSideBySide_TestWorkIsDiscardedWhenTheHeadMoved(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Test: "count"})
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()
	early := f.agent.evidenceCalls()
	if len(early) != 1 {
		t.Fatalf("live checks before the head moved = %q, want one beside the review", early)
	}
	laneDir := early[0]
	newHead := f.commitInRun("fixed.go", "package fixed\n", "no-mistakes(review): fix a finding")

	outcome, err := (&TestStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	calls := f.agent.evidenceCalls()
	if len(calls) != 2 || !samePath(calls[1], f.dir) {
		t.Fatalf("live check ran in %q, want a second time, in the run's worktree", calls)
	}
	if got := f.baselineRuns(); got != 2 {
		t.Fatalf("baseline runs = %d, want 2: once early and once on the new head", got)
	}
	findings := liveCheckFindings(t, outcome)
	if findings.TestedHeadSHA != newHead {
		t.Fatalf("tested head = %q, want the new head %q", findings.TestedHeadSHA, newHead)
	}
	if head, _, _ := f.review.Shared.Baseline(); head != newHead {
		t.Fatalf("recorded baseline head = %q, want the new head %q", head, newHead)
	}
	if !strings.Contains(f.logText(), "is not used: the run's head has moved") {
		t.Fatalf("log = %q, want the reason the early work was not used", f.logText())
	}
	if _, err := os.Stat(laneDir); !os.IsNotExist(err) {
		t.Fatalf("the discarded copy %s is still there (stat error %v)", laneDir, err)
	}
	if _, err := os.Stat(filepath.Join(f.review.EvidenceDir, sideLaneDirName, string(types.StepTest))); !os.IsNotExist(err) {
		t.Fatalf("the discarded work's evidence folder is still there (stat error %v)", err)
	}
}

// The early work is stopped and removed as soon as the head moves, not left
// to hold the machine until its step's turn.
func TestSideBySide_StaleWorkStopsAsSoonAsTheHeadMoves(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{})
	started := make(chan string, 1)
	stopped := make(chan struct{})
	f.agent.evidenceF = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		started <- opts.CWD
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	startSideLanes(f.review, f.baseSHA)
	var laneDir string
	select {
	case laneDir = <-started:
	case <-time.After(sideBySideWait):
		t.Fatal("the live check did not start beside the review")
	}

	// A review fix round commits in the run's worktree and records the head.
	if err := os.WriteFile(filepath.Join(f.dir, "fixed.go"), []byte("package fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "no-mistakes(review): fix a finding")
	newHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if err := recordAgentFixHead(f.review, types.StepReview, newHead); err != nil {
		t.Fatalf("recordAgentFixHead() error = %v", err)
	}

	select {
	case <-stopped:
	default:
		t.Fatal("the live check started on the old head was still running after the head moved")
	}
	if _, err := os.Stat(laneDir); !os.IsNotExist(err) {
		t.Fatalf("the stopped copy %s is still there (stat error %v)", laneDir, err)
	}
	if lanes, _ := f.review.Shared.SideLanes().(*sideLanes); lanes == nil || lanes.claim(types.StepTest) != nil {
		t.Fatal("the stopped work was still left for the Test step")
	}
	if !strings.Contains(f.logText(), "is stopped and removed, because the run's head moved to "+shortObjectID(newHead)) {
		t.Fatalf("log = %q, want the stop and its reason", f.logText())
	}
}

func TestSideBySide_TestWorkIsDiscardedWhenItFailedOrTheWorktreeIsDirty(t *testing.T) {
	t.Run("the early work failed", func(t *testing.T) {
		f := newSideBySideFixture(t, config.Commands{Test: "count"})
		f.agent.evidenceF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if !samePath(opts.CWD, f.dir) {
				return nil, errors.New("harness crashed in the copy")
			}
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		}
		startSideLanes(f.review, f.baseSHA)
		f.lanesDone()

		outcome, err := (&TestStep{}).Execute(f.turn())
		if err != nil {
			t.Fatalf("Execute() error = %v, want the step run again in the run's worktree", err)
		}
		if outcome.NeedsApproval || len(f.agent.evidenceCalls()) != 2 {
			t.Fatalf("outcome = %#v after %d live checks, want a pass from the second", outcome, len(f.agent.evidenceCalls()))
		}
		if !strings.Contains(f.logText(), "is not used: it failed") {
			t.Fatalf("log = %q, want the failure named", f.logText())
		}
	})
	t.Run("the run's worktree holds changes", func(t *testing.T) {
		f := newSideBySideFixture(t, config.Commands{Test: "count"})
		startSideLanes(f.review, f.baseSHA)
		f.lanesDone()
		if err := os.WriteFile(filepath.Join(f.dir, "stray.txt"), []byte("stray\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if len(f.agent.evidenceCalls()) != 2 || !strings.Contains(f.logText(), "holds uncommitted changes") {
			t.Fatalf("live checks = %d log = %q, want the early work discarded over the dirty worktree", len(f.agent.evidenceCalls()), f.logText())
		}
	})
}

// A test file the evidence turn wrote is left in the worktree today, to be
// committed by a later step. Taken from a copy, it is brought over.
func TestSideBySide_TestFilesTheEarlyLiveCheckWroteAreBroughtOver(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{})
	// A file at the root: git status names a file in a new folder by its
	// folder, and the step notes it as a test file only by its own name.
	f.agent.evidenceF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "feature_test.go"), []byte("package main\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()

	outcome, err := (&TestStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := f.agent.evidenceCalls(); len(calls) != 1 || samePath(calls[0], f.dir) {
		t.Fatalf("live check ran in %q, want once, in a copy, and taken", calls)
	}
	content, err := os.ReadFile(filepath.Join(f.dir, "feature_test.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("feature_test.go in the run's worktree = %q, %v, want the file the live check wrote", content, err)
	}
	if status := gitStatusPorcelain(t, f.dir); !strings.Contains(status, "feature_test.go") {
		t.Fatalf("run worktree status = %q, want the new test file uncommitted, as the step leaves it today", status)
	}
	var noted bool
	for _, item := range liveCheckFindings(t, outcome).Items {
		noted = noted || strings.Contains(item.Description, "new test file written by agent: feature_test.go")
	}
	if !noted {
		t.Fatalf("findings = %#v, want the new test file noted as today", liveCheckFindings(t, outcome).Items)
	}
}

// A commit made in the copy by the evidence agent was validated by no step.
func TestSideBySide_TestWorkThatCommittedIsDiscarded(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{})
	f.agent.evidenceF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !samePath(opts.CWD, f.dir) {
			if err := os.WriteFile(filepath.Join(opts.CWD, "sneaked.go"), []byte("package sneaked\n"), 0o644); err != nil {
				return nil, err
			}
			gitCmd(t, opts.CWD, "add", "-A")
			gitCmd(t, opts.CWD, "-c", "user.name=agent", "-c", "user.email=agent@example.com", "commit", "-m", "agent commit")
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()

	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(f.agent.evidenceCalls()) != 2 || !strings.Contains(f.logText(), "left a commit") {
		t.Fatalf("live checks = %d log = %q, want the early work discarded over its commit", len(f.agent.evidenceCalls()), f.logText())
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != f.headSHA {
		t.Fatalf("run worktree HEAD = %s, want %s: the copy's commit must not reach the run", got, f.headSHA)
	}
}

// The documentation pass starts beside the review where the repository names
// a lint command. Its commit is made in a copy and reaches the run only when
// the documentation step takes it, as a fast-forward of the unchanged head.
func TestSideBySide_DocumentationCommitIsBroughtOverAtItsTurn(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
	f.agent.documentF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "GUIDE.md"), []byte("# Guide\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document the feature"}`)}, nil
	}
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()

	// Until the step's turn the run is exactly where the review left it.
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != f.headSHA {
		t.Fatalf("run worktree HEAD = %s before the documentation step's turn, want %s", got, f.headSHA)
	}
	if f.review.Run.HeadSHA != f.headSHA {
		t.Fatalf("run head = %s before the documentation step's turn, want %s", f.review.Run.HeadSHA, f.headSHA)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "GUIDE.md")); !os.IsNotExist(err) {
		t.Fatalf("GUIDE.md reached the run's worktree before the step took it (stat error %v)", err)
	}

	// The Test step takes its own early work first, as in a real run.
	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Test Execute() error = %v", err)
	}
	outcome, err := (&DocumentStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Document Execute() error = %v", err)
	}
	if calls := f.agent.documentCalls(); len(calls) != 1 || samePath(calls[0], f.dir) {
		t.Fatalf("documentation pass ran in %q, want once, in a copy", calls)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if head == f.headSHA {
		t.Fatal("the run's worktree did not take the documentation commit")
	}
	if parent := gitCmd(t, f.dir, "rev-parse", "HEAD^"); parent != f.headSHA {
		t.Fatalf("documentation commit's parent = %s, want the head the review read, %s", parent, f.headSHA)
	}
	if f.review.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want the documentation commit %s recorded", f.review.Run.HeadSHA, head)
	}
	if content, err := os.ReadFile(filepath.Join(f.dir, "GUIDE.md")); err != nil || string(content) != "# Guide\n" {
		t.Fatalf("GUIDE.md = %q, %v", content, err)
	}
	if status := gitStatusPorcelain(t, f.dir); status != "" {
		t.Fatalf("run worktree status = %q, want clean", status)
	}
	if !strings.Contains(lastCommitMessage(t, f.dir), "document the feature") {
		t.Fatalf("commit message = %q, want the documentation pass's own", lastCommitMessage(t, f.dir))
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the pass's own clean result", outcome)
	}
}

// The Test step leaves its new test files uncommitted, as it does today. The
// documentation commit made beside the review is still taken over them when
// it changes none of their paths, and they stay for a later step to commit.
func TestSideBySide_DocumentationCommitIsTakenOverTheTestStepsNewFiles(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
	f.agent.evidenceF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.MkdirAll(filepath.Join(opts.CWD, "pkg"), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(opts.CWD, "pkg", "feature_test.go"), []byte("package pkg\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}
	f.agent.documentF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "GUIDE.md"), []byte("# Guide\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document the feature"}`)}, nil
	}
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()

	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Test Execute() error = %v", err)
	}
	if _, err := (&DocumentStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Document Execute() error = %v", err)
	}
	if calls := f.agent.documentCalls(); len(calls) != 1 || samePath(calls[0], f.dir) {
		t.Fatalf("documentation pass ran in %q, want once, in a copy", calls)
	}
	if parent := gitCmd(t, f.dir, "rev-parse", "HEAD^"); parent != f.headSHA {
		t.Fatalf("documentation commit's parent = %s, want the head the review read, %s", parent, f.headSHA)
	}
	if committed := gitCmd(t, f.dir, "show", "--name-only", "--format=", "HEAD"); committed != "GUIDE.md" {
		t.Fatalf("documentation commit changes %q, want GUIDE.md only", committed)
	}
	if status := gitStatusPorcelain(t, f.dir); !strings.Contains(status, "pkg/") {
		t.Fatalf("run worktree status = %q, want the new test file still uncommitted", status)
	}
}

// A documentation commit that changes a path the run's worktree holds
// uncommitted changes to is not taken, and the pass runs in the worktree.
func TestSideBySide_DocumentationCommitOverAnUncommittedPathIsNotTaken(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
	f.agent.documentF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "GUIDE.md"), []byte("# Guide\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document the feature"}`)}, nil
	}
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()
	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Test Execute() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "GUIDE.md"), []byte("left by the Test step\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (&DocumentStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Document Execute() error = %v", err)
	}
	if calls := f.agent.documentCalls(); len(calls) != 2 || !samePath(calls[1], f.dir) {
		t.Fatalf("documentation pass ran in %q, want a second time, in the run's worktree", calls)
	}
	if !strings.Contains(f.logText(), "its commit changes paths the run's worktree holds uncommitted changes to: GUIDE.md") {
		t.Fatalf("log = %q, want the overlap named", f.logText())
	}
}

// With no lint command the documentation pass also fixes lint and may change
// code, so it keeps its place after the Test step.
func TestSideBySide_DocumentationPassThatAlsoLintsKeepsItsPlace(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{})
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()
	if calls := f.agent.documentCalls(); len(calls) != 0 {
		t.Fatalf("documentation pass started early in %q, want it left for its own turn", calls)
	}
	if !strings.Contains(f.logText(), "the documentation pass keeps its place after the Test step") {
		t.Fatalf("log = %q, want the reason said", f.logText())
	}
	if _, err := (&TestStep{}).Execute(f.turn()); err != nil {
		t.Fatal(err)
	}
	turn := f.turn()
	if _, err := (&DocumentStep{}).Execute(turn); err != nil {
		t.Fatalf("Document Execute() error = %v", err)
	}
	if calls := f.agent.documentCalls(); len(calls) != 1 || !samePath(calls[0], f.dir) {
		t.Fatalf("documentation pass ran in %q, want once, in the run's worktree", calls)
	}
}

// A Test fix round moves the head, so a documentation commit made on the old
// one is thrown away and the pass runs on the new head.
func TestSideBySide_DocumentationWorkIsDiscardedWhenTheHeadMoved(t *testing.T) {
	f := newSideBySideFixture(t, config.Commands{Lint: "echo lint"})
	f.agent.documentF = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "GUIDE.md"), []byte("written in "+filepath.Base(opts.CWD)+"\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document the feature"}`)}, nil
	}
	startSideLanes(f.review, f.baseSHA)
	f.lanesDone()
	newHead := f.commitInRun("fixed.go", "package fixed\n", "no-mistakes(test): fix a failing test")

	if _, err := (&DocumentStep{}).Execute(f.turn()); err != nil {
		t.Fatalf("Document Execute() error = %v", err)
	}
	calls := f.agent.documentCalls()
	if len(calls) != 2 || !samePath(calls[1], f.dir) {
		t.Fatalf("documentation pass ran in %q, want a second time, in the run's worktree", calls)
	}
	if parent := gitCmd(t, f.dir, "rev-parse", "HEAD^"); parent != newHead {
		t.Fatalf("documentation commit's parent = %s, want the new head %s", parent, newHead)
	}
	if !strings.Contains(f.logText(), "the document work started beside the review") || !strings.Contains(f.logText(), "is not used") {
		t.Fatalf("log = %q, want the early documentation work reported as not used", f.logText())
	}
}

func TestSideBySide_NothingStartsWhereItCannot(t *testing.T) {
	cases := map[string]func(*pipeline.StepContext){
		"a fix round":                    func(sctx *pipeline.StepContext) { sctx.Fixing = true },
		"an answer round":                func(sctx *pipeline.StepContext) { sctx.FinalizingAnswers = true },
		"a replayed review turn":         func(sctx *pipeline.StepContext) { sctx.SkipFixExecution = true },
		"an eval replay":                 func(sctx *pipeline.StepContext) { sctx.EvalReplay = true },
		"a side lane itself":             func(sctx *pipeline.StepContext) { sctx.SideLane = true },
		"no offer from the executor":     func(sctx *pipeline.StepContext) { sctx.SideLaneContext = nil },
		"a harness not known to be safe": func(sctx *pipeline.StepContext) { sctx.Config.Agent = types.AgentName("opencode") },
		"a worktree off the recorded head": func(sctx *pipeline.StepContext) {
			sctx.Run.HeadSHA = sctx.Run.BaseSHA
		},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSideBySideFixture(t, config.Commands{Test: "count", Lint: "echo lint"})
			configure(f.review)
			startSideLanes(f.review, f.baseSHA)
			f.lanesDone()
			if runs, evidence, document := f.baselineRuns(), f.agent.evidenceCalls(), f.agent.documentCalls(); runs != 0 || len(evidence) != 0 || len(document) != 0 {
				t.Fatalf("baseline runs = %d live checks = %q documentation passes = %q, want nothing started", runs, evidence, document)
			}
		})
	}
	// A plain clone of the worktree would lack what these repositories need.
	copyGaps := map[string][2]string{
		"a repository with submodules":    {".gitmodules", "[submodule \"lib\"]\n\tpath = lib\n\turl = ../lib.git\n"},
		"a repository with Git LFS files": {"assets/.gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n"},
	}
	for name, file := range copyGaps {
		t.Run(name, func(t *testing.T) {
			f := newSideBySideFixture(t, config.Commands{Test: "count", Lint: "echo lint"})
			f.commitInRun(file[0], file[1], "add "+file[0])
			startSideLanes(f.review, f.baseSHA)
			f.lanesDone()
			if runs, evidence, document := f.baselineRuns(), f.agent.evidenceCalls(), f.agent.documentCalls(); runs != 0 || len(evidence) != 0 || len(document) != 0 {
				t.Fatalf("baseline runs = %d live checks = %q documentation passes = %q, want nothing started", runs, evidence, document)
			}
			if !strings.Contains(f.logText(), "nothing starts beside the review") || !strings.Contains(f.logText(), "a copy of the worktree would not hold") {
				t.Fatalf("log = %q, want the reason said", f.logText())
			}
		})
	}
}

// Work that no step took is stopped and removed when the run ends.
func TestSideBySide_UnclaimedWorkIsStoppedAndRemovedWhenTheRunEnds(t *testing.T) {
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

	closed := make(chan struct{})
	go func() {
		f.review.Shared.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(sideBySideWait):
		t.Fatal("closing the run's shared state did not stop the side lane")
	}
	if _, err := os.Stat(laneDir); !os.IsNotExist(err) {
		t.Fatalf("the copy %s outlived its run (stat error %v)", laneDir, err)
	}
}

// pushGuardFixture is a run at its Push step under side_by_side: the Test
// step completed, and the baseline is recorded as passed on the head the
// review started on.
func newPushGuardFixture(t *testing.T, cmds config.Commands) *sideBySideFixture {
	t.Helper()
	f := newSideBySideFixture(t, cmds)
	steps, err := f.review.DB.GetStepsByRun(f.review.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if err := f.review.DB.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.review.Shared.RecordBaseline(f.headSHA, true)
	return f
}

func TestBaselineBeforePush_TheTestedHeadIsPushedAsIs(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "count"})
	note, restart, err := baselineBeforePush(f.turn(), f.headSHA)
	if err != nil || restart || note != "" {
		t.Fatalf("baselineBeforePush() = %q, %v, %v, want nothing to say or do", note, restart, err)
	}
	if f.baselineRuns() != 0 {
		t.Fatalf("baseline runs = %d, want 0: the head is the tested commit", f.baselineRuns())
	}
}

// Commits that touch documentation only are named and pushed without the
// baseline running again, which is what happens to them today.
func TestBaselineBeforePush_DocumentationCommitsAreNamedAndNotRetested(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "count"})
	f.commitInRun("GUIDE.md", "# Guide\n", "no-mistakes(document): document the feature")
	head := f.commitInRun("docs/usage.rst", "Usage\n", "no-mistakes(document): add usage")

	note, restart, err := baselineBeforePush(f.turn(), head)
	if err != nil || restart {
		t.Fatalf("baselineBeforePush() = %q, %v, %v", note, restart, err)
	}
	if f.baselineRuns() != 0 {
		t.Fatalf("baseline runs = %d, want 0 for documentation-only commits", f.baselineRuns())
	}
	for _, want := range []string{"2 commit(s) came after " + shortObjectID(f.headSHA), "document the feature", "add usage", "documentation only"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q, want it to contain %q", note, want)
		}
	}
}

// A commit that touches code after the tested one is not pushed untested:
// the baseline runs again on the head to push.
func TestBaselineBeforePush_CodeCommitsRunTheBaselineAgain(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "count"})
	f.commitInRun("GUIDE.md", "# Guide\n", "no-mistakes(document): document the feature")
	head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")

	note, restart, err := baselineBeforePush(f.turn(), head)
	if err != nil || restart {
		t.Fatalf("baselineBeforePush() = %q, %v, %v, want the push to go on after a passing baseline", note, restart, err)
	}
	if f.baselineRuns() != 1 {
		t.Fatalf("baseline runs = %d, want 1 on the head to push", f.baselineRuns())
	}
	for _, want := range []string{"apply formatter", "more than documentation", "lint_fixed.go", "run again on the pushed head and passed"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q, want it to contain %q", note, want)
		}
	}
	if tested, passed, _ := f.review.Shared.Baseline(); tested != head || !passed {
		t.Fatalf("recorded baseline = %q, %v, want the pushed head as passed", tested, passed)
	}
}

// A baseline that fails on the head to push stops the push. The run goes back
// to the Test step once, and a second failure fails the run.
func TestBaselineBeforePush_AFailingBaselineStopsThePush(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "exit 3"})
	head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")

	_, restart, err := baselineBeforePush(f.turn(), head)
	if err != nil || !restart {
		t.Fatalf("baselineBeforePush() restart = %v, err = %v, want the run sent back to the Test step", restart, err)
	}
	if _, passed, _ := f.review.Shared.Baseline(); passed {
		t.Fatal("a failed baseline was recorded as passed")
	}
	// Back at the push with the same failure: the Test step recorded a pass
	// on an earlier head and another code commit followed it.
	f.review.Shared.RecordBaseline(f.headSHA, true)
	_, restart, err = baselineBeforePush(f.turn(), head)
	if err == nil || restart || !strings.Contains(err.Error(), "nothing was pushed") {
		t.Fatalf("second baselineBeforePush() restart = %v, err = %v, want the run failed with nothing pushed", restart, err)
	}
}

func TestBaselineBeforePush_WhenItDoesNotRun(t *testing.T) {
	t.Run("side_by_side off", func(t *testing.T) {
		f := newPushGuardFixture(t, config.Commands{Test: "count"})
		f.review.Config.SideBySide = false
		head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
		note, restart, err := baselineBeforePush(f.turn(), head)
		if err != nil || restart || note != "" || f.baselineRuns() != 0 {
			t.Fatalf("baselineBeforePush() = %q, %v, %v after %d baseline runs, want nothing: today's behavior", note, restart, err, f.baselineRuns())
		}
	})
	t.Run("the Test step was skipped", func(t *testing.T) {
		f := newSideBySideFixture(t, config.Commands{Test: "count"})
		steps, _ := f.review.DB.GetStepsByRun(f.review.Run.ID)
		for _, step := range steps {
			if err := f.review.DB.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
		head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
		note, restart, err := baselineBeforePush(f.turn(), head)
		if err != nil || restart || note != "" || f.baselineRuns() != 0 {
			t.Fatalf("baselineBeforePush() = %q, %v, %v after %d baseline runs, want nothing for a Test step the operator skipped", note, restart, err, f.baselineRuns())
		}
	})
	t.Run("no baseline is configured", func(t *testing.T) {
		f := newPushGuardFixture(t, config.Commands{})
		head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
		note, restart, err := baselineBeforePush(f.turn(), head)
		if err != nil || restart || !strings.Contains(note, "apply formatter") || !strings.Contains(note, "No baseline test command is configured") {
			t.Fatalf("baselineBeforePush() = %q, %v, %v, want the commits named and nothing run", note, restart, err)
		}
	})
	t.Run("the Test step was completed over a failing baseline", func(t *testing.T) {
		f := newPushGuardFixture(t, config.Commands{Test: "count"})
		f.review.Shared.RecordBaseline(f.headSHA, false)
		head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
		note, restart, err := baselineBeforePush(f.turn(), head)
		if err != nil || restart || f.baselineRuns() != 0 || !strings.Contains(note, "completed over it") {
			t.Fatalf("baselineBeforePush() = %q, %v, %v after %d baseline runs, want the commits named and no second run of a baseline a person already accepted as failing", note, restart, err, f.baselineRuns())
		}
	})
}

// With no record of the tested commit, as after a daemon restart, the head is
// treated as untested and the baseline runs.
func TestBaselineBeforePush_AnUnknownTestedHeadIsUntested(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "count"})
	f.review.Shared = &pipeline.RunShared{}
	note, restart, err := baselineBeforePush(f.turn(), f.headSHA)
	if err != nil || restart {
		t.Fatalf("baselineBeforePush() = %q, %v, %v", note, restart, err)
	}
	if f.baselineRuns() != 1 || !strings.Contains(note, "is treated as untested") {
		t.Fatalf("baseline runs = %d note = %q, want the baseline run on a head nothing vouches for", f.baselineRuns(), note)
	}
}

// A repository whose tests read its documents says so, and then a commit to
// those documents runs the baseline again.
func TestBaselineBeforePush_TheRepositoryDecidesWhatIsDocumentation(t *testing.T) {
	t.Run("an empty list: nothing is documentation", func(t *testing.T) {
		f := newPushGuardFixture(t, config.Commands{Test: "count"})
		f.review.Config.Test.DocumentationPaths = []string{}
		head := f.commitInRun("GUIDE.md", "# Guide\n", "no-mistakes(document): document the feature")
		if _, restart, err := baselineBeforePush(f.turn(), head); err != nil || restart {
			t.Fatalf("baselineBeforePush() restart = %v, err = %v", restart, err)
		}
		if f.baselineRuns() != 1 {
			t.Fatalf("baseline runs = %d, want 1: this repository counts no path as documentation", f.baselineRuns())
		}
	})
	t.Run("its own list replaces the built-in one", func(t *testing.T) {
		f := newPushGuardFixture(t, config.Commands{Test: "count"})
		f.review.Config.Test.DocumentationPaths = []string{"notes/**"}
		head := f.commitInRun("notes/plan.txt", "plan\n", "no-mistakes(document): add a note")
		if _, _, err := baselineBeforePush(f.turn(), head); err != nil || f.baselineRuns() != 0 {
			t.Fatalf("err = %v baseline runs = %d, want 0 for a path the repository lists", err, f.baselineRuns())
		}
		head = f.commitInRun("GUIDE.md", "# Guide\n", "no-mistakes(document): document the feature")
		if _, _, err := baselineBeforePush(f.turn(), head); err != nil || f.baselineRuns() != 1 {
			t.Fatalf("err = %v baseline runs = %d, want 1: GUIDE.md is not in this repository's list", err, f.baselineRuns())
		}
	})
}

// pushTarget makes a bare repository the run's push target, reached through
// the worktree's origin as the Push step resolves it, with the branch already
// published at the head the review read.
func (f *sideBySideFixture) pushTarget() string {
	f.t.Helper()
	upstream := f.t.TempDir()
	gitCmd(f.t, upstream, "init", "--bare")
	gitCmd(f.t, f.dir, "remote", "set-url", "origin", upstream)
	gitCmd(f.t, f.dir, "push", "origin", "main")
	gitCmd(f.t, f.dir, "push", "origin", f.headSHA+":refs/heads/feature")
	f.review.Repo.UpstreamURL = upstream
	f.review.Run.Branch = "refs/heads/feature"
	setupGateMirror(f.t, f.review)
	return upstream
}

// At the real push boundary: a baseline that fails on the head to push leaves
// the remote untouched and sends the run back to the Test step.
func TestPushStep_SideBySideDoesNotPushAHeadTheBaselineFailsOn(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "exit 3"})
	upstream := f.pushTarget()
	head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
	recordReviewApproval(t, f.review, f.headSHA)

	outcome, err := (&PushStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.RestartFrom != types.StepTest {
		t.Fatalf("outcome = %#v, want the run sent back to the Test step", outcome)
	}
	if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != f.headSHA {
		t.Fatalf("remote branch = %s, want it left at %s: nothing pushed", remote, f.headSHA)
	}
	if f.review.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want the head the Test step will test, %s", f.review.Run.HeadSHA, head)
	}
}

// And the passing case: the head is pushed, with a note on the step saying
// which commits came after the tested one.
func TestPushStep_SideBySidePushesAfterThePassingBaselineAndSaysWhatCameAfter(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "count"})
	upstream := f.pushTarget()
	head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
	recordReviewApproval(t, f.review, f.headSHA)

	outcome, err := (&PushStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.RestartFrom != "" || outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a completed push", outcome)
	}
	if pushed := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); pushed != head {
		t.Fatalf("pushed head = %s, want %s", pushed, head)
	}
	if f.baselineRuns() != 1 {
		t.Fatalf("baseline runs = %d, want 1 on the head that was pushed", f.baselineRuns())
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Items) != 1 || findings.Items[0].Severity != types.FindingSeverityInfo || findings.Items[0].Action != types.ActionNoOp || !strings.Contains(findings.Items[0].Description, "apply formatter") {
		t.Fatalf("findings = %#v, want one informational note naming the commit after the tested one", findings.Items)
	}
}

// With the setting absent the push step says and does nothing new: the head
// is pushed even though the baseline would fail on it, as it is today.
func TestPushStep_SideBySideOffPushesAsBefore(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "exit 3"})
	f.review.Config.SideBySide = false
	upstream := f.pushTarget()
	head := f.commitInRun("lint_fixed.go", "package fixed\n", "no-mistakes(lint): apply formatter")
	recordReviewApproval(t, f.review, f.headSHA)

	outcome, err := (&PushStep{}).Execute(f.turn())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.RestartFrom != "" || outcome.Findings != "" {
		t.Fatalf("outcome = %#v, want today's empty outcome", outcome)
	}
	if pushed := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); pushed != head {
		t.Fatalf("pushed head = %s, want %s", pushed, head)
	}
}
