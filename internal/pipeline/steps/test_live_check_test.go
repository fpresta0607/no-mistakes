package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// liveCheckTestAgentTimeout is the Test agent's own limit in the budget tests.
// It is far above the budget they set, so a step that reaches it instead of
// the budget is told apart by the park it produces.
const liveCheckTestAgentTimeout = 3 * time.Second

const liveCheckPassingEvidence = `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"user runs the command","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`

// liveCheckLogs collects the step's discrete log lines so a test can assert
// the one line a skipped live check records.
func liveCheckLogs(sctx *pipeline.StepContext) *[]string {
	var lines []string
	sctx.Log = func(s string) { lines = append(lines, s) }
	return &lines
}

func liveCheckFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	if outcome == nil {
		t.Fatal("outcome is nil")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	return findings
}

// ledgerPathFromPrompt reads the ledger file the evidence agent was told to
// write, so the tests prove the agent is given the path and not only that the
// step knows it.
func ledgerPathFromPrompt(t *testing.T, prompt string) string {
	t.Helper()
	const marker = "- Ledger file: "
	for _, line := range strings.Split(prompt, "\n") {
		if path, ok := strings.CutPrefix(line, marker); ok {
			return strings.TrimSpace(path)
		}
	}
	t.Fatalf("the evidence prompt names no ledger file:\n%s", prompt)
	return ""
}

func appendLedger(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// The repository's own rule says the change has nothing to look at: the
// evidence agent is not called, one line says so and names the rule, and the
// baseline command still runs.
func TestTestStep_SurfaceRuleSkipsTheLiveCheckWhenTheChangeTouchesNoSurface(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "echo baseline-ran"})
	sctx.Config.Test.SurfacePaths = []string{"app/**", "*.html"}
	logs := liveCheckLogs(sctx)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("the evidence agent was called %d time(s) for a change that touches none of test.surface_paths", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the step complete: the rule is the repository's own and needs no decision", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Tested) != 1 || findings.Tested[0] != "echo baseline-ran" {
		t.Fatalf("tested = %q, want the baseline command, which still runs", findings.Tested)
	}
	if findings.TestedHeadSHA != headSHA {
		t.Fatalf("tested head = %q, want %q", findings.TestedHeadSHA, headSHA)
	}
	// No verdict: nothing was driven live, so the record must not claim one.
	if findings.Verdict != "" || len(findings.Scenarios) != 0 {
		t.Fatalf("verdict = %q scenarios = %#v, want neither for a live check that did not run", findings.Verdict, findings.Scenarios)
	}
	if !strings.Contains(findings.TestingSummary, "test.surface_paths") {
		t.Fatalf("testing summary = %q, want the rule named for the pull request", findings.TestingSummary)
	}
	var skipLines []string
	for _, line := range *logs {
		if strings.Contains(line, "test.surface_paths") {
			skipLines = append(skipLines, line)
		}
	}
	if len(skipLines) != 1 {
		t.Fatalf("log lines naming the rule = %q, want exactly one", skipLines)
	}
	for _, want := range []string{"live check skipped", "app/**", "*.html"} {
		if !strings.Contains(skipLines[0], want) {
			t.Fatalf("skip line = %q, want it to contain %q", skipLines[0], want)
		}
	}
	for _, line := range *logs {
		if strings.Contains(line, "asking agent") {
			t.Fatalf("log line %q claims an agent was asked", line)
		}
	}
}

func TestTestStep_SurfaceRuleRunsTheLiveCheckWhenTheChangeTouchesTheSurface(t *testing.T) {
	cases := map[string][]string{
		"by basename":       {"docs/**", "feature.txt"},
		"by glob":           {"*.txt"},
		"one of many rules": {"app/**", "cmd/**", "feat*.txt"},
	}
	for name, surface := range cases {
		t.Run(name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(liveCheckPassingEvidence)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Test.SurfacePaths = surface

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("evidence agent calls = %d, want 1: the change touches the surface", len(ag.calls))
			}
			if findings := liveCheckFindings(t, outcome); findings.Verdict != types.TestVerdictGo {
				t.Fatalf("verdict = %q, want the agent's own", findings.Verdict)
			}
		})
	}
}

// A file moved out of the surface is a change to the surface: the rule reads
// both sides of a rename.
func TestTestStep_SurfaceRuleCountsAFileMovedOutOfTheSurface(t *testing.T) {
	dir, _, _ := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "main")
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "page.html"), []byte("<p>a page long enough for git to call the move a rename</p>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add page")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-B", "feature")
	if err := os.MkdirAll(filepath.Join(dir, "attic"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "mv", "app/page.html", "attic/page.txt")
	gitCmd(t, dir, "commit", "-m", "retire page")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(liveCheckPassingEvidence)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Test.SurfacePaths = []string{"app/**"}

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent calls = %d, want 1: a file left app/", len(ag.calls))
	}
}

// Skipping the live check must not skip the baseline's verdict: a failing
// command parks exactly as it does with the agent.
func TestTestStep_SurfaceRuleKeepsAFailingBaselineBlocking(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 3"})
	sctx.Config.Test.SurfacePaths = []string{"app/**"}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("evidence agent calls = %d, want 0", len(ag.calls))
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 3 {
		t.Fatalf("outcome = %#v, want the failing baseline parked, fixable, with its exit code", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryTestCommand || findings.Items[0].Severity != "error" {
		t.Fatalf("findings = %#v, want the configured test command's error", findings.Items)
	}
}

// With no list, every change gets the evidence turn: today's behavior.
func TestTestStep_NoSurfaceListKeepsTheLiveCheckOnEveryChange(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(liveCheckPassingEvidence)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("evidence agent calls = %d, want 1", len(ag.calls))
	}
}

// The off-state guarantee is append-only: with every live check setting on,
// the prompt is today's prompt followed by the new sections and nothing else.
func TestTestStep_LiveCheckRulesOffIsTodaysPrompt(t *testing.T) {
	prompt := func(configure func(*pipeline.StepContext)) string {
		dir, baseSHA, headSHA := setupGitRepo(t)
		ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(liveCheckPassingEvidence)}, nil
		}}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		// Fixed locations, so two contexts render the same text.
		sctx.WorkDir = dir
		sctx.EvidenceDir = filepath.Join(t.TempDir(), "evidence")
		configure(sctx)
		if _, err := (&TestStep{}).Execute(sctx); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if len(ag.calls) != 1 {
			t.Fatalf("evidence agent calls = %d, want 1", len(ag.calls))
		}
		got := ag.calls[0].Prompt
		got = strings.ReplaceAll(got, sctx.EvidenceDir, "<evidence>")
		return strings.ReplaceAll(got, dir, "<worktree>")
	}

	off := prompt(func(*pipeline.StepContext) {})
	for _, marker := range []string{"Live check budget", "Ledger file", "Prepared environment", "Build no environment of your own"} {
		if strings.Contains(off, marker) {
			t.Fatalf("with every setting off the prompt contains %q", marker)
		}
	}
	if !strings.Contains(off, "build a disposable one yourself") {
		t.Fatal("with test.environment unset the agent must still be told to build its own disposable environment")
	}

	on := prompt(func(sctx *pipeline.StepContext) {
		sctx.Config.TestLiveCheckBudget = 15 * time.Minute
		sctx.Config.Test.Environment = "scripts/live-env.ps1 starts the app on a seeded database at http://127.0.0.1:4010"
		sctx.Config.Test.SurfacePaths = []string{"*.txt"}
	})
	if !strings.HasPrefix(on, off) {
		t.Fatal("the prompt with the live check settings on must be today's prompt plus appended sections")
	}
	added := strings.TrimPrefix(on, off)
	for _, want := range []string{
		"Live check budget",
		"15m0s",
		"- Ledger file: ",
		"Prepared environment",
		"scripts/live-env.ps1 starts the app on a seeded database at http://127.0.0.1:4010",
		"Build no environment of your own",
	} {
		if !strings.Contains(added, want) {
			t.Fatalf("appended sections = %q, want them to contain %q", added, want)
		}
	}
}

// A live check that reaches the budget completes with what it wrote down:
// the pass counts, and what it did not reach is untested with the reason.
func TestTestStep_LiveCheckBudgetCutCompletesWithWhatItProved(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		ledger := ledgerPathFromPrompt(t, opts.Prompt)
		appendLedger(t, ledger,
			`{"name":"user signs in","result":"planned"}`,
			`{"name":"user exports a report","result":"planned"}`,
			`{"name":"user deletes an account","result":"planned"}`,
			`{"name":"user signs in","result":"pass","live":true,"evidence":"POST /session answered 200 with a cookie","reason":""}`,
			`this line is not JSON and must be skipped`,
		)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "echo baseline-ran"})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want the step complete with what the ledger holds", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want no park: a call that reaches the budget does not park the run", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Scenarios) != 3 {
		t.Fatalf("scenarios = %#v, want the three the agent planned", findings.Scenarios)
	}
	byName := map[string]types.TestScenario{}
	for _, scenario := range findings.Scenarios {
		byName[scenario.Name] = scenario
	}
	if got := byName["user signs in"]; got.Result != types.ScenarioResultPass || !got.Live || got.Evidence != "POST /session answered 200 with a cookie" {
		t.Fatalf("recorded pass = %#v, want it to count as recorded", got)
	}
	for _, name := range []string{"user exports a report", "user deletes an account"} {
		got := byName[name]
		if got.Result != types.ScenarioResultUntested || got.Live || !strings.Contains(got.Reason, "budget") {
			t.Fatalf("unreached scenario %q = %#v, want untested with the budget as the reason", name, got)
		}
	}
	for _, item := range findings.Items {
		if item.ID == types.FindingIDTestAgentTimeout || item.ID == types.FindingIDTestAgentUnvalidatedWork {
			t.Fatalf("finding %q is the parking budget cut, which the live check budget replaces", item.ID)
		}
		if item.Severity != "info" {
			t.Fatalf("finding %#v would block, want only an informational note", item)
		}
	}
	var note *types.Finding
	for i := range findings.Items {
		if findings.Items[i].ID == types.FindingIDTestLiveCheckBudget {
			note = &findings.Items[i]
		}
	}
	if note == nil || note.Action != types.ActionNoOp || !strings.Contains(note.Description, "test_live_check_budget") {
		t.Fatalf("findings = %#v, want one informational note naming test_live_check_budget", findings.Items)
	}
	// The record claims no verdict it did not earn, and names the head.
	if findings.Verdict != "" {
		t.Fatalf("verdict = %q, want none: two scenarios were never driven", findings.Verdict)
	}
	if findings.TestedHeadSHA != headSHA {
		t.Fatalf("tested head = %q, want %q", findings.TestedHeadSHA, headSHA)
	}
	if len(findings.Tested) == 0 || findings.Tested[0] != "echo baseline-ran" {
		t.Fatalf("tested = %q, want the baseline that ran before the live check", findings.Tested)
	}
}

// A failure the agent wrote down before the budget is a finding exactly as a
// finished live check's failure is: a no-go that parks and can be fixed.
func TestTestStep_LiveCheckBudgetCutKeepsARecordedFailure(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		appendLedger(t, ledgerPathFromPrompt(t, opts.Prompt),
			`{"name":"user signs in","result":"planned"}`,
			`{"name":"user exports a report","result":"planned"}`,
			`{"name":"user signs in","result":"fail","live":true,"evidence":"POST /session answered 500","reason":""}`,
		)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want a recorded failure parked and fixable as today", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if findings.Verdict != types.TestVerdictNoGo {
		t.Fatalf("verdict = %q, want no-go for a recorded failure", findings.Verdict)
	}
	var blocking []types.Finding
	for _, item := range findings.Items {
		if item.Severity == types.FindingSeverityError {
			blocking = append(blocking, item)
		}
	}
	if len(blocking) != 1 || blocking[0].Action != types.ActionAutoFix || !strings.Contains(blocking[0].Description, "user signs in") {
		t.Fatalf("blocking findings = %#v, want the no-go naming the failed scenario", blocking)
	}
}

// The ledger is the agent's own account, so it is held to the contract a
// finished live check is held to: a pass or fail that is not live with
// evidence does not count.
func TestTestStep_LiveCheckBudgetCutDowngradesUnsupportedClaims(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		appendLedger(t, ledgerPathFromPrompt(t, opts.Prompt),
			`{"name":"pass with no evidence","result":"pass","live":true,"evidence":"  ","reason":""}`,
			`{"name":"pass that was not live","result":"pass","live":false,"evidence":"unit test passed","reason":""}`,
			`{"name":"fail that was not live","result":"fail","live":false,"evidence":"read the code","reason":""}`,
			`{"name":"honestly untested","result":"untested","live":false,"evidence":"","reason":"needs a paid sandbox account"}`,
			`{"name":"","result":"pass","live":true,"evidence":"nameless","reason":""}`,
			`{"name":"unknown word","result":"maybe","live":true,"evidence":"x","reason":""}`,
		)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want no park: nothing live failed", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Scenarios) != 5 {
		t.Fatalf("scenarios = %#v, want the five named ones", findings.Scenarios)
	}
	for _, scenario := range findings.Scenarios {
		if scenario.Result != types.ScenarioResultUntested || scenario.Live || scenario.Evidence != "" || strings.TrimSpace(scenario.Reason) == "" {
			t.Fatalf("scenario %#v, want untested, not live, no evidence, with a reason", scenario)
		}
	}
	if got := findings.Scenarios[3]; got.Name != "honestly untested" || got.Reason != "needs a paid sandbox account" {
		t.Fatalf("scenario = %#v, want the agent's own reason kept", got)
	}
	if findings.Verdict != "" {
		t.Fatalf("verdict = %q, want none", findings.Verdict)
	}
}

func TestTestStep_LiveCheckBudgetCutWithNothingRecordedSaysSo(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want no park", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Scenarios) != 1 || findings.Scenarios[0].Result != types.ScenarioResultUntested || findings.Scenarios[0].Live {
		t.Fatalf("scenarios = %#v, want one untested entry so the pull request shows that nothing was proved", findings.Scenarios)
	}
	if !strings.Contains(findings.Scenarios[0].Reason, "recorded no scenario") {
		t.Fatalf("reason = %q, want it to say nothing was recorded", findings.Scenarios[0].Reason)
	}
	if !strings.Contains(findings.TestingSummary, "budget") {
		t.Fatalf("testing summary = %q, want the budget named", findings.TestingSummary)
	}
}

// What a stopped live check left in the worktree is neither pushed nor
// destroyed: it is moved beside the evidence and the worktree is put back.
func TestTestStep_LiveCheckBudgetCutSetsLeftoverWorkAside(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		appendLedger(t, ledgerPathFromPrompt(t, opts.Prompt), `{"name":"user signs in","result":"planned"}`)
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("half an edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "scratch-env", "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scratch-env", "data", "seed.sql"), []byte("insert into users values (1);\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "half_written_test.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want no park once the leftover work is set aside", outcome)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree status = %q, want it clean so nothing unvalidated can be committed", status)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	content, err := os.ReadFile(filepath.Join(dir, "feature.txt"))
	if err != nil || string(content) != "feature code\n" {
		t.Fatalf("feature.txt = %q, %v, want the committed content back", content, err)
	}
	setAside := filepath.Join(sctx.EvidenceDir, liveCheckDirName, liveCheckSetAsideName)
	for _, rel := range []string{filepath.Join("files", "scratch-env", "data", "seed.sql"), filepath.Join("files", "half_written_test.go")} {
		if _, err := os.Stat(filepath.Join(setAside, rel)); err != nil {
			t.Fatalf("set-aside copy %s missing: %v", rel, err)
		}
	}
	patch, err := os.ReadFile(filepath.Join(setAside, "tracked-changes.patch"))
	if err != nil || !strings.Contains(string(patch), "half an edit") {
		t.Fatalf("tracked-changes.patch = %q, %v, want the edit to the tracked file kept", patch, err)
	}
	findings := liveCheckFindings(t, outcome)
	var note string
	for _, item := range findings.Items {
		if item.ID == types.FindingIDTestLiveCheckBudget {
			note = item.Description
		}
		if strings.Contains(item.Description, "new test file written by agent") {
			t.Fatalf("finding %q reports a test file that was set aside, not kept", item.Description)
		}
	}
	if !strings.Contains(note, "set aside") || !strings.Contains(note, setAside) {
		t.Fatalf("note = %q, want it to say where the leftover work was set aside", note)
	}
}

// A commit cannot be put back without rewriting the run's branch, so a
// stopped live check that committed still parks exactly as a cut does today.
func TestTestStep_LiveCheckBudgetCutThatCommittedStillParks(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "sneaked.txt"), []byte("unvalidated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "agent commit nobody validated")
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a park: the branch holds a commit no Test turn validated", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	var ids []string
	for _, item := range findings.Items {
		ids = append(ids, item.ID)
	}
	if !strings.Contains(strings.Join(ids, ","), types.FindingIDTestAgentUnvalidatedWork) {
		t.Fatalf("finding ids = %q, want the unvalidated-work refusal", ids)
	}
}

// A fix round answers a known defect. A live check stopped before it could
// show that defect gone must not clear it, so it parks as a cut does today.
func TestTestStep_LiveCheckBudgetCutInAFixRoundKeepsTheAnsweredGate(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test"}
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"repair sign in"}`)}, nil
		}
		appendLedger(t, ledgerPathFromPrompt(t, opts.Prompt), `{"name":"user signs in","result":"planned"}`)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 60 * time.Millisecond
	sctx.Config.TestAgentTimeout = liveCheckTestAgentTimeout
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"live validation verdict: no-go (1 of 1 scenarios were driven live against the product); failed: user signs in","action":"auto-fix"}],"summary":"","verdict":"no-go","scenarios":[{"name":"user signs in","result":"fail","live":true,"evidence":"500","reason":""}],"tested_head_sha":"` + headSHA + `"}`

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a park: the failure this round answered was never shown gone", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if findings.Verdict != types.TestVerdictNoGo {
		t.Fatalf("verdict = %q, want the answered gate's no-go carried", findings.Verdict)
	}
}

func TestTestStep_LiveCheckInsideItsBudgetIsUnchanged(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Minute {
			t.Errorf("the live check's deadline is %s away, want its one minute budget", time.Until(deadline))
		}
		return &agent.Result{Output: json.RawMessage(liveCheckPassingEvidence)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = time.Minute

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a completed pass", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if findings.Verdict != types.TestVerdictGo || len(findings.Scenarios) != 1 {
		t.Fatalf("findings = %#v, want the agent's own result untouched", findings)
	}
	for _, item := range findings.Items {
		if item.ID == types.FindingIDTestLiveCheckBudget {
			t.Fatal("a live check that finished inside its budget must carry no budget note")
		}
	}
}

// Without the setting the only bound is test_agent_timeout, and reaching it
// parks: today's behavior.
func TestTestStep_NoLiveCheckBudgetKeepsTheParkingCut(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestAgentTimeout = 40 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	findings := liveCheckFindings(t, outcome)
	if !outcome.NeedsApproval || len(findings.Items) != 1 || findings.Items[0].ID != types.FindingIDTestAgentTimeout {
		t.Fatalf("outcome = %#v findings = %#v, want the test-agent-timeout park", outcome, findings.Items)
	}
}

// The ledger and the set-aside folder sit in the run's evidence directory,
// which is published when test.evidence.store_in_repo is on. Neither is
// evidence, so the publisher leaves the folder out by name.
func TestLiveCheckFolderIsNeverPublishedAsEvidence(t *testing.T) {
	if !strings.Contains(strings.Join(evidencePublishExcludedDirs(), ","), liveCheckDirName) {
		t.Fatalf("published evidence excludes %q, want %q among them", evidencePublishExcludedDirs(), liveCheckDirName)
	}
}
