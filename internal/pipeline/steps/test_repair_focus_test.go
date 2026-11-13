package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	repairTurnMarker = "Fix the failing tests in this repository"
	repairResultJSON = `{"summary":"fix the test"}`
	namedFailure     = "--- FAIL: TestBroken"
	// fullCommandMustNotRun is a commands.test whose exit code gives it away.
	fullCommandMustNotRun = "exit 42"
)

// failsUntilRepaired names one failure until a repair writes fix.txt.
func failsUntilRepaired() string {
	if runtime.GOOS == "windows" {
		return "if exist fix.txt (exit /b 0) else (echo " + namedFailure + " && exit /b 3)"
	}
	return "if [ -f fix.txt ]; then exit 0; fi; echo '" + namedFailure + "'; exit 3"
}

// copiesFocusFile is a focused check that keeps what it was given.
func copiesFocusFile(target string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`copy /y %%%s%% %s >nul`, testFocusEnv, target)
	}
	return fmt.Sprintf(`cp "$%s" '%s'`, testFocusEnv, target)
}

func isRepairTurn(opts agent.RunOpts) bool {
	return strings.Contains(opts.Prompt, repairTurnMarker)
}

// repairingAgent writes fix.txt on a repair turn and answers repairJSON, and
// answers every other turn with evidenceJSON.
func repairingAgent(dir, repairJSON, evidenceJSON string) *mockAgent {
	return &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !isRepairTurn(opts) {
			return &agent.Result{Output: json.RawMessage(evidenceJSON)}, nil
		}
		if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(repairJSON)}, nil
	}}
}

func marshalFindings(t *testing.T, findings types.Findings) string {
	t.Helper()
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func failedCommandFinding(id string) types.Finding {
	return types.Finding{
		ID:          id,
		Severity:    "error",
		Action:      types.ActionAutoFix,
		Category:    types.FindingCategoryTestCommand,
		Description: "configured test command failed with exit code 3: make test",
	}
}

// answeredFailure is the gate a repair answers after a failed baseline.
func answeredFailure(t *testing.T, failure *types.TestBaselineFailure) string {
	t.Helper()
	return marshalFindings(t, types.Findings{
		Items:           []types.Finding{failedCommandFinding("test-1")},
		Summary:         namedFailure,
		BaselineFailure: failure,
	})
}

func repairContext(t *testing.T, ag *mockAgent, dir, baseSHA, headSHA string, cmds config.Commands, answered string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	sctx.Shared = &pipeline.RunShared{}
	sctx.Fixing = true
	sctx.PreviousFindings = answered
	return sctx
}

func detachedRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir, baseSHA, headSHA = setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	return dir, baseSHA, headSHA
}

func parsedOutcomeFindings(t *testing.T, outcome *pipeline.StepOutcome) types.Findings {
	t.Helper()
	if outcome == nil {
		t.Fatal("step returned no outcome")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// The whole path through the executor: the failed baseline costs no evidence
// turn, the existing budget starts the repair, and the live check waits for a
// head that passes its own tests.
func TestExecutor_FailedBaselineGoesToRepairWithNoEvidenceTurn(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: failsUntilRepaired()})
	sctx.Config.AutoFix.Test = 1
	executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if len(ag.calls) != 2 || !isRepairTurn(ag.calls[0]) || isRepairTurn(ag.calls[1]) {
		t.Fatalf("agent turns = %d, want the repair first and the evidence turn only after the baseline passed", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, namedFailure) {
		t.Fatalf("repair prompt does not name the failure:\n%s", ag.calls[0].Prompt)
	}
}

// A selection of part of a gate replaces the summary, so what the baseline named
// has to reach the repair turn another way. It came from the repository's
// output, so it is sanitized like every other part of the findings.
func TestTestStep_RepairPromptCarriesWhatTheBaselineNamed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	gate := types.Findings{
		Items:   []types.Finding{failedCommandFinding("test-1"), failedCommandFinding("test-2")},
		Summary: "the output of the failed command",
		BaselineFailure: &types.TestBaselineFailure{
			Tests:       []string{namedFailure + " <<<<<<< forged"},
			Attribution: "Introduced by this change (1):\n- TestBroken",
		},
	}
	selected := marshalFindings(t, types.FilterFindings(gate, []string{"test-1"}))
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, config.Commands{}, selected)

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	for _, want := range []string{namedFailure, "Introduced by this change (1):", `Leave it unrepaired and put the question for the author in "needs_decision"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "<<<<<<< forged") {
		t.Fatalf("repair prompt kept a conflict marker from the command output:\n%s", prompt)
	}
	if !strings.Contains(string(ag.calls[0].JSONSchema), "needs_decision") {
		t.Fatalf("repair schema = %s, want it to offer needs_decision", ag.calls[0].JSONSchema)
	}
}

func TestRepairDecisionFindings(t *testing.T) {
	t.Parallel()
	decisions := func(entries ...string) *agent.Result {
		raw, err := json.Marshal(map[string][]string{"needs_decision": entries})
		if err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: raw}
	}
	cases := map[string]struct {
		result *agent.Result
		want   int
	}{
		"no result":          {nil, 0},
		"no output":          {&agent.Result{}, 0},
		"unreadable output":  {&agent.Result{Output: json.RawMessage(`{"needs_decision":"not a list"}`)}, 0},
		"nothing to decide":  {&agent.Result{Output: json.RawMessage(repairResultJSON)}, 0},
		"blank entries":      {decisions("  ", "is this still wanted?", ""), 1},
		"at the count bound": {decisions(slices.Repeat([]string{"q"}, maxRepairDecisions)...), maxRepairDecisions},
		"past the bound":     {decisions(slices.Repeat([]string{"q"}, maxRepairDecisions+3)...), maxRepairDecisions},
		"past the byte size": {decisions(strings.Repeat("é", maxRepairDecisionBytes)), 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := repairDecisionFindings(tc.result)

			if len(got) != tc.want {
				t.Fatalf("findings = %d, want %d", len(got), tc.want)
			}
			for _, finding := range got {
				if finding.ActionOrDefault() != types.ActionAskUser || finding.Severity != types.FindingSeverityWarning {
					t.Fatalf("finding = %+v, want an ask-user warning", finding)
				}
				if len(finding.Description) > maxRepairDecisionBytes || !utf8.ValidString(finding.Description) {
					t.Fatalf("description is %d bytes, valid UTF-8 %v", len(finding.Description), utf8.ValidString(finding.Description))
				}
			}
		})
	}
}

func TestRepairCommandsRun(t *testing.T) {
	t.Parallel()
	commandsRun := func(entries ...map[string]any) *agent.Result {
		raw, err := json.Marshal(map[string]any{"commands_run": entries})
		if err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: raw}
	}
	ran := func(command string) map[string]any { return map[string]any{"command": command, "exit_code": 1} }
	cases := map[string]struct {
		result *agent.Result
		want   int
	}{
		"no result":            {nil, 0},
		"no output":            {&agent.Result{}, 0},
		"unreadable output":    {&agent.Result{Output: json.RawMessage(`{"commands_run":"not a list"}`)}, 0},
		"nothing run":          {&agent.Result{Output: json.RawMessage(repairResultJSON)}, 0},
		"blank commands":       {commandsRun(ran("  "), ran("./guard.sh"), ran("")), 1},
		"no exit status":       {commandsRun(map[string]any{"command": "./guard.sh"}), 1},
		"at the count bound":   {commandsRun(slices.Repeat([]map[string]any{ran("go test ./...")}, maxRepairCommands)...), maxRepairCommands},
		"past the count bound": {commandsRun(slices.Repeat([]map[string]any{ran("go test ./...")}, maxRepairCommands+3)...), maxRepairCommands},
		"past the byte size":   {commandsRun(ran(strings.Repeat("é", maxRepairCommandBytes))), 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := repairCommandsRun(tc.result)

			if len(got) != tc.want {
				t.Fatalf("commands = %d, want %d", len(got), tc.want)
			}
			for _, command := range got {
				if command.Command == "" || len(command.Command) > maxRepairCommandBytes || !utf8.ValidString(command.Command) {
					t.Fatalf("command %q is %d bytes, valid UTF-8 %v", command.Command, len(command.Command), utf8.ValidString(command.Command))
				}
			}
		})
	}
}

// Whether the rest of the repair worked or not, a question for a human stops
// the automatic loop: another round would meet it with nobody having answered.
func TestTestStep_RepairQuestionStopsTheAutomaticFixLoop(t *testing.T) {
	t.Parallel()
	const question = "TestBroken pins the old copy, is it still wanted?"
	cases := map[string]struct {
		command        string
		wantAgentCalls int
		wantExitCode   int
	}{
		"the baseline still fails": {"exit 3", 1, 3},
		"the baseline now passes":  {"exit 0", 2, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := detachedRepo(t)
			repairJSON := `{"summary":"fix the test","needs_decision":["` + question + `"]}`
			ag := repairingAgent(dir, repairJSON, passingScenarioFindingsJSON)
			sctx := repairContext(t, ag, dir, baseSHA, headSHA, config.Commands{Test: tc.command}, answeredFailure(t, nil))

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}

			if !outcome.NeedsApproval || outcome.AutoFixable || outcome.ExitCode != tc.wantExitCode {
				t.Fatalf("outcome = %+v, want a park no automatic round can answer", outcome)
			}
			if len(ag.calls) != tc.wantAgentCalls {
				t.Fatalf("agent calls = %d, want %d", len(ag.calls), tc.wantAgentCalls)
			}
			findings := parsedOutcomeFindings(t, outcome)
			if !slices.ContainsFunc(findings.Items, func(item types.Finding) bool {
				return item.Description == question && item.ActionOrDefault() == types.ActionAskUser
			}) {
				t.Fatalf("gate lacks the repair's question: %+v", findings.Items)
			}
		})
	}
}

func TestTestStep_FocusedCheckStandsInForTheFullCommandAfterARepair(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	seen := filepath.Join(t.TempDir(), "seen-focus.txt")
	focused := copiesFocusFile(seen)
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	cmds := config.Commands{Test: fullCommandMustNotRun, TestFocused: focused}
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, cmds, answeredFailure(t, &types.TestBaselineFailure{Tests: []string{namedFailure}}))
	logs := liveCheckLogs(sctx)
	var focusFilesAtAgent []string
	answer := ag.runFn
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !isRepairTurn(opts) {
			scratch, _ := envValue(opts.Env, "TMPDIR")
			if scratch == "" {
				t.Fatal("evidence agent received no scratch directory")
			}
			focusFilesAtAgent, _ = filepath.Glob(filepath.Join(scratch, testFocusFileName))
		}
		return answer(ctx, opts)
	}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %+v, want the step complete on a passing focused check", outcome)
	}
	findings := parsedOutcomeFindings(t, outcome)
	want := types.TestFocusedCheck{Command: focused, HeadSHA: gitCmd(t, dir, "rev-parse", "HEAD"), Tests: 1, Paths: 1}
	if findings.FocusedCheck == nil || *findings.FocusedCheck != want {
		t.Fatalf("focused check record = %+v, want %+v", findings.FocusedCheck, want)
	}
	if len(findings.Tested) == 0 || findings.Tested[0] != focused || slices.Contains(findings.Tested, fullCommandMustNotRun) {
		t.Fatalf("tested = %+v, want the focused check and no claim of the full command", findings.Tested)
	}
	if !strings.Contains(findings.TestingSummary, "the Test step did not run the configured test command on this head") {
		t.Fatalf("testing summary = %q, want it to say the full command did not run", findings.TestingSummary)
	}
	if containsLog(*logs, "baseline tests passed") || !containsLog(*logs, "focused check passed") {
		t.Fatalf("step log calls a focused pass a baseline pass: %q", *logs)
	}
	if len(focusFilesAtAgent) != 0 {
		t.Fatalf("the focus file was still in the agent's scratch directory: %v", focusFilesAtAgent)
	}
	given, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if want := "test " + namedFailure + "\npath fix.txt\n"; string(given) != want {
		t.Fatalf("focus file = %q, want %q", given, want)
	}
	if _, _, _, recorded := sctx.Shared.Baseline(); recorded {
		t.Fatal("a focused pass was recorded as a full baseline")
	}
	for _, call := range ag.calls {
		if slices.ContainsFunc(call.Env, func(entry string) bool { return strings.HasPrefix(entry, testFocusEnv+"=") }) {
			t.Fatal("the focus reached an agent turn: only the repository's own commands read it")
		}
	}
	if strings.Contains(lastPrompt(ag), "Focused re-check after a repair") {
		t.Fatal("the first live check of a change was narrowed, with no earlier one to narrow from")
	}
}

func TestTestStep_FailedFocusedCheckParksAndIsRecorded(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	cmds := config.Commands{Test: fullCommandMustNotRun, TestFocused: "exit 5"}
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, cmds, answeredFailure(t, &types.TestBaselineFailure{Tests: []string{namedFailure}}))
	logs := liveCheckLogs(sctx)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if containsLog(*logs, "configured test command failed") || !containsLog(*logs, "focused check failed") {
		t.Fatalf("step log = %q, want the failure named as the focused check's", *logs)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 5 {
		t.Fatalf("outcome = %+v, want an auto-fixable park on the focused check's exit code", outcome)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want only the repair", len(ag.calls))
	}
	findings := parsedOutcomeFindings(t, outcome)
	if findings.FocusedCheck == nil || findings.FocusedCheck.ExitCode != 5 {
		t.Fatalf("focused check record = %+v, want exit code 5", findings.FocusedCheck)
	}
	if !strings.Contains(findings.TestingSummary, "No live check ran on this head") {
		t.Fatalf("testing summary = %q", findings.TestingSummary)
	}
	if len(findings.Items) == 0 || !strings.HasPrefix(findings.Items[0].Description, "focused test check failed with exit code 5: exit 5") {
		t.Fatalf("findings = %+v, want the failure named as the focused check's", findings.Items)
	}
	if _, _, _, recorded := sctx.Shared.Baseline(); recorded {
		t.Fatal("a focused check was recorded as a full baseline")
	}
}

// commands.test never ran, so nothing may claim a result for it: base
// attribution compares that command and has nothing to say about this one.
func TestTestStep_FailedFocusedCheckGetsNoBaseAttribution(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	cmds := config.Commands{Test: fullCommandMustNotRun, TestFocused: "exit 5"}
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, cmds, answeredFailure(t, &types.TestBaselineFailure{Tests: []string{namedFailure}}))
	sctx.Config.Test.BaseAttribution = true

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(outcome.Findings, "commands.test") || strings.Contains(outcome.Findings, "Base attribution") {
		t.Fatalf("findings claim a commands.test result: %s", outcome.Findings)
	}
}

// The focused check passed but a machine-local check beside it failed, and an
// approval could carry this head to Push. commands.test still never ran on it.
func TestTestStep_FocusedCheckBesideAFailedLocalCheckStaysUntested(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	cmds := config.Commands{Test: fullCommandMustNotRun, TestFocused: "exit 0"}
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, cmds, answeredFailure(t, &types.TestBaselineFailure{Tests: []string{namedFailure}}))
	sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: []string{"exit 7"}}}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if !outcome.NeedsApproval || outcome.ExitCode != 7 {
		t.Fatalf("outcome = %+v, want the local failure to park", outcome)
	}
	if _, _, _, recorded := sctx.Shared.Baseline(); recorded {
		t.Fatal("the head reads as having a full baseline")
	}
}

// With nothing to stand in, or nothing complete to give it, the behavior is
// what it was: the whole of commands.test runs and no focused check is claimed.
func TestTestStep_FullCommandRunsWhenAFocusedCheckCannotStandIn(t *testing.T) {
	t.Parallel()
	named := &types.TestBaselineFailure{Tests: []string{namedFailure}}
	noTurnGate := types.Findings{Items: []types.Finding{{
		ID:          types.FindingIDTestCommandNoTurn,
		Severity:    types.FindingSeverityWarning,
		Category:    types.FindingCategoryTestCommand,
		Description: "the test command was given no turn",
	}}}
	noTurnBesideWork := noTurnGate
	noTurnBesideWork.Items = append(slices.Clone(noTurnGate.Items), types.Finding{ID: "test-2", Severity: "error", Action: types.ActionAutoFix, Description: "rename the fixture"})
	cases := map[string]struct {
		commands    config.Commands
		isFirstPass bool
		gate        types.Findings
		deferred    string
		failure     *types.TestBaselineFailure
		noChange    bool
		wantTested  string
	}{
		"the repository provides no focused command": {
			commands:   config.Commands{Test: "exit 0"},
			failure:    named,
			wantTested: "exit 0",
		},
		"the failure was not fully named": {
			commands:   config.Commands{Test: "exit 0", TestFocused: fullCommandMustNotRun},
			failure:    &types.TestBaselineFailure{Tests: []string{namedFailure}, Incomplete: true},
			wantTested: "exit 0",
		},
		"no repair ran on the first pass": {
			commands:    config.Commands{Test: "exit 0", TestFocused: fullCommandMustNotRun},
			isFirstPass: true,
			wantTested:  "exit 0",
		},
		"the answer was to a command given no turn": {
			commands:   config.Commands{Test: "exit 0", TestFocused: fullCommandMustNotRun},
			gate:       noTurnGate,
			wantTested: "exit 0",
		},
		"a command given no turn sits beside the selected finding": {
			commands:   config.Commands{Test: "exit 0", TestFocused: fullCommandMustNotRun},
			gate:       noTurnBesideWork,
			wantTested: "exit 0",
		},
		"a command given no turn was left out of the selection": {
			commands:   config.Commands{Test: "exit 0", TestFocused: fullCommandMustNotRun},
			deferred:   marshalFindings(t, noTurnGate),
			wantTested: "exit 0",
		},
		"the repair changed nothing and no failure was named": {
			commands:   config.Commands{Test: "exit 0", TestFocused: fullCommandMustNotRun},
			noChange:   true,
			wantTested: "exit 0",
		},
		"there is no full command to stand in for": {
			commands: config.Commands{TestFocused: fullCommandMustNotRun},
			failure:  named,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := detachedRepo(t)
			ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
			if tc.noChange {
				ag = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
				}}
			}
			answered := answeredFailure(t, tc.failure)
			if len(tc.gate.Items) > 0 {
				answered = marshalFindings(t, tc.gate)
			}
			sctx := repairContext(t, ag, dir, baseSHA, headSHA, tc.commands, answered)
			sctx.Fixing = !tc.isFirstPass
			sctx.DeferredFindings = tc.deferred

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}

			if outcome.ExitCode != 0 {
				t.Fatalf("exit code = %d, want the focused check left unrun", outcome.ExitCode)
			}
			findings := parsedOutcomeFindings(t, outcome)
			if findings.FocusedCheck != nil {
				t.Fatalf("focused check record = %+v, want none", findings.FocusedCheck)
			}
			if tc.wantTested != "" && (len(findings.Tested) == 0 || findings.Tested[0] != tc.wantTested) {
				t.Fatalf("tested = %+v, want %q first", findings.Tested, tc.wantTested)
			}
			if slices.Contains(findings.Tested, fullCommandMustNotRun) {
				t.Fatalf("tested = %+v, want no focused command", findings.Tested)
			}
		})
	}
}

// test_command_wrapper stands in front of every baseline test check, and a
// focused check is one.
func TestTestStep_WrapperStartsTheFocusedCheck(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: fullCommandMustNotRun, TestFocused: "echo focused"}, nil)
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = answeredFailure(t, &types.TestBaselineFailure{Tests: []string{namedFailure}})

	if _, err := (&TestStep{}).Execute(f.sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("wrapper starts = %d, want only the focused check", len(calls))
	}
	assertShellCall(t, calls[0][3:], "echo focused")
}

// A refused focused check ran nothing, so the record claims nothing.
func TestTestStep_WrapperRefusalLeavesNoFocusedCheckRecord(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: fullCommandMustNotRun, TestFocused: "echo focused"}, map[string]string{
		"FAKE_CLI_WRAPPER_REFUSAL":      "turnline: this run takes no turn",
		"FAKE_CLI_WRAPPER_REFUSAL_CODE": "125",
	})
	f.sctx.Shared = &pipeline.RunShared{}
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = answeredFailure(t, &types.TestBaselineFailure{Tests: []string{namedFailure}})

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want the no-turn park", outcome)
	}
	findings := parsedOutcomeFindings(t, outcome)
	if findings.FocusedCheck != nil {
		t.Fatalf("focused check record = %+v, want none", findings.FocusedCheck)
	}
	if len(findings.Items) == 0 || findings.Items[0].ID != types.FindingIDTestCommandNoTurn {
		t.Fatalf("findings = %+v, want the no-turn finding", findings.Items)
	}
	if _, _, _, recorded := f.sctx.Shared.Baseline(); recorded {
		t.Fatal("a refused focused check was recorded as a baseline, so Push would skip commands.test")
	}
}

// Under side_by_side the Push step decides from the recorded baseline, and a
// focused pass must leave that decision at "untested".
func TestBaselineBeforePush_AFocusedPassStillRunsTheBaseline(t *testing.T) {
	f := newPushGuardFixture(t, config.Commands{Test: "count"})
	f.review.Shared.RecordFocusedCheck(f.baseSHA)

	note, restart, err := baselineBeforePush(f.turn(), f.headSHA)

	if err != nil || restart {
		t.Fatalf("baselineBeforePush() = %q, %v, %v", note, restart, err)
	}
	if f.baselineRuns() != 1 {
		t.Fatalf("baseline runs = %d, want the full baseline on the head to push", f.baselineRuns())
	}
	if !strings.Contains(note, "treated as untested") {
		t.Fatalf("note = %q, want it to say the head counted as untested", note)
	}
	if tested, _, passed, _ := f.review.Shared.Baseline(); tested != f.headSHA || !passed {
		t.Fatalf("recorded baseline = %q, %v, want the pushed head as passed", tested, passed)
	}
}

func TestNewPipelineAttestation_FocusedCheckBelongsToItsOwnHead(t *testing.T) {
	t.Parallel()
	raw := marshalFindings(t, types.Findings{
		Tested:         []string{"make focused"},
		TestingSummary: "the focused check passed",
		FocusedCheck: &types.TestFocusedCheck{
			Command:  "make focused",
			ExitCode: 0,
			HeadSHA:  testPipelineHeadSHA,
			Tests:    2,
			Paths:    1,
		},
	})
	steps, rounds := testStepWithFindings(t, raw)

	onHead := newPipelineAttestation(steps, rounds, testPipelineHeadSHA, pipelineAttestationPolicy{})
	moved := newPipelineAttestation(steps, rounds, strings.Repeat("ab", 20), pipelineAttestationPolicy{})

	want := pipelineAttestationFocusedCheck{ExitCode: 0, Tests: 2, Paths: 1}
	if onHead.FocusedCheck == nil || *onHead.FocusedCheck != want {
		t.Fatalf("attested focused check = %+v, want %+v", onHead.FocusedCheck, want)
	}
	if moved.FocusedCheck != nil {
		t.Fatalf("a later head carried the focused check: %+v", moved.FocusedCheck)
	}
}

// A repair whose baseline still fails drove nothing, so the last live check
// stays on the head it ran on and the attestation claims none for this one.
func TestTestStep_FailedRepairLeavesTheLastLiveCheckOnItsOwnHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	liveHead := strings.Repeat("c", 40)
	scenario := types.TestScenario{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true}
	ag := repairingAgent(dir, repairResultJSON, passingScenarioFindingsJSON)
	answered := marshalFindings(t, types.Findings{
		Items:         []types.Finding{failedCommandFinding("test-1")},
		Scenarios:     []types.TestScenario{scenario},
		Verdict:       types.TestVerdictGo,
		TestedHeadSHA: liveHead,
	})
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 3"}, answered)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want only the repair", len(ag.calls))
	}
	findings := parsedOutcomeFindings(t, outcome)
	if findings.TestedHeadSHA != liveHead || len(findings.Scenarios) != 1 {
		t.Fatalf("record = %+v, want the last live check kept on its own head", findings)
	}
	if findings.Verdict != "" || findings.Scenarios[0].Result != types.ScenarioResultUntested || findings.Scenarios[0].Live {
		t.Fatalf("record = %+v, want no verdict and the scenario not driven on this head", findings)
	}
	steps, rounds := testStepWithFindings(t, outcome.Findings)
	if rendered := BuildTestingSummary(steps, rounds); strings.Contains(rendered, "go - ") || strings.Contains(rendered, "✅ pass") {
		t.Fatalf("the pull request shows a live result on a head that failed its tests:\n%s", rendered)
	}
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if attestation := newPipelineAttestation(steps, rounds, head, pipelineAttestationPolicy{}); attestation.LiveValidation != nil {
		t.Fatalf("attestation claimed a live check on the repaired head: %+v", attestation.LiveValidation)
	}
}

func TestTestStep_RepairNarrowsTheLiveCheckToWhatItTouched(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := detachedRepo(t)
	const repairedScenario, undrivenScenario = "a declined card can retry", "user reaches the success screen"
	evidence := `{"findings":[],"summary":"","tested":["drove the retry"],"testing_summary":"drove the retry","artifacts":[],` +
		`"scenarios":[{"name":"` + repairedScenario + `","result":"pass","live":true,"evidence":"retry.png","reason":""}],"verdict":"go"}`
	ag := repairingAgent(dir, repairResultJSON, evidence)
	answered := marshalFindings(t, types.Findings{
		Items: []types.Finding{{ID: "test-1", Severity: "error", Action: types.ActionAutoFix, Description: "the retry button is gone"}},
		Scenarios: []types.TestScenario{
			{Name: repairedScenario, Result: types.ScenarioResultFail, Live: true},
			{Name: undrivenScenario, Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
		},
		Verdict:       types.TestVerdictNoGo,
		TestedHeadSHA: headSHA,
	})
	sctx := repairContext(t, ag, dir, baseSHA, headSHA, config.Commands{}, answered)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	prompt := lastPrompt(ag)
	for _, want := range []string{"Focused re-check after a repair", repairedScenario, undrivenScenario, "The repair changed: fix.txt"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("evidence prompt missing %q:\n%s", want, prompt)
		}
	}
	findings := parsedOutcomeFindings(t, outcome)
	index := slices.IndexFunc(findings.Scenarios, func(scenario types.TestScenario) bool { return scenario.Name == undrivenScenario })
	if index < 0 {
		t.Fatalf("scenarios = %+v, want the undriven one kept on the record", findings.Scenarios)
	}
	kept := findings.Scenarios[index]
	if kept.Result != types.ScenarioResultUntested || kept.Live || kept.Evidence != "" {
		t.Fatalf("undriven scenario = %+v, want it untested with no live claim", kept)
	}
	if live, total := types.LiveScenarioCounts(findings.Scenarios); live != 1 || total != 2 {
		t.Fatalf("live counts = %d of %d, want 1 of 2", live, total)
	}
}

// Issue #116: a repair that changed nothing leaves the head the last live check
// ran on. What passed there in this executor lifetime stands, so the re-check
// drives only what failed and the step keeps each pass as it was recorded,
// together with the tested entries and artifacts that back it (decision 1021628).
func TestTestStep_RepairThatChangedNothingKeepsThePassesOfTheSameHead(t *testing.T) {
	t.Parallel()
	const failedScenario, passedScenario = "a declined card can retry", "user reaches the success screen"
	const sameHeadRule, movedHeadRule = "run none of them again", "Drive at least one scenario: when nothing above applies, drive the recorded scenario closest to the repair's change."
	const keptTested = "go vet ./... passed"
	const repairReport = "no change; the guard failure needs a scope decision"
	const repairCommandsJSON = `[{"command":"./guard.sh --pinned","exit_code":1}]`
	const reportedCommand = "- ./guard.sh --pinned (exit 1)"
	const operatorInstruction = "Run the pinned guard.sh exactly once and never retry it."
	const receiptHeader = "Execution receipt from the repair turn"
	keptArtifact := types.TestArtifact{Kind: "image", Label: "checkout screen", Path: "checkout.png"}
	evidence := `{"findings":[{"severity":"error","action":"auto-fix","description":"the retry button is gone"}],"summary":"","tested":["drove the retry"],"testing_summary":"drove the retry","artifacts":[],` +
		`"scenarios":[{"name":"` + failedScenario + `","result":"fail","live":true,"evidence":"retry.png","reason":"the button is still gone"}],"verdict":"no-go"}`
	cases := map[string]struct {
		liveCheckHead func(headSHA string) string
		testedHead    func(headSHA string) string
		repairEdits   bool
		wantKept      bool
	}{
		"the same head in the same executor lifetime": {wantKept: true},
		"no live check recorded in this executor lifetime": {
			liveCheckHead: func(string) string { return "" },
		},
		"the last live check ran on another head": {
			liveCheckHead: func(string) string { return strings.Repeat("c", 40) },
			testedHead:    func(string) string { return strings.Repeat("c", 40) },
		},
		"the repair changed a file": {repairEdits: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := detachedRepo(t)
			liveCheckHead, testedHead := headSHA, headSHA
			if tc.liveCheckHead != nil {
				liveCheckHead = tc.liveCheckHead(headSHA)
			}
			if tc.testedHead != nil {
				testedHead = tc.testedHead(headSHA)
			}
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if !isRepairTurn(opts) {
					return &agent.Result{Output: json.RawMessage(evidence)}, nil
				}
				if tc.repairEdits {
					if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
						return nil, err
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"` + repairReport + `","commands_run":` + repairCommandsJSON + `}`)}, nil
			}}
			answered := marshalFindings(t, types.Findings{
				Items: []types.Finding{{ID: "test-1", Severity: "error", Action: types.ActionAutoFix, Description: "the retry button is gone", UserInstructions: operatorInstruction}},
				Scenarios: []types.TestScenario{
					{Name: failedScenario, Result: types.ScenarioResultFail, Live: true},
					{Name: passedScenario, Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png", Reason: "go vet and the checkout tests passed"},
				},
				Tested:        []string{keptTested},
				Artifacts:     []types.TestArtifact{keptArtifact},
				Verdict:       types.TestVerdictNoGo,
				TestedHeadSHA: testedHead,
			})
			sctx := repairContext(t, ag, dir, baseSHA, headSHA, config.Commands{}, answered)
			sctx.Shared.RecordLiveCheck(liveCheckHead)

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}

			prompt := lastPrompt(ag)
			wantRule, neverRule := movedHeadRule, sameHeadRule
			if tc.wantKept {
				wantRule, neverRule = sameHeadRule, movedHeadRule
			}
			if !strings.Contains(prompt, wantRule) || strings.Contains(prompt, neverRule) {
				t.Fatalf("evidence prompt must hold %q and not %q:\n%s", wantRule, neverRule, prompt)
			}
			for _, part := range []string{receiptHeader, repairReport, reportedCommand, operatorInstruction} {
				if strings.Contains(prompt, part) != tc.wantKept {
					t.Fatalf("evidence prompt holds %q = %v, want %v (the repair's receipt rides only on the head it ran on):\n%s", part, !tc.wantKept, tc.wantKept, prompt)
				}
			}
			findings := parsedOutcomeFindings(t, outcome)
			index := slices.IndexFunc(findings.Scenarios, func(scenario types.TestScenario) bool { return scenario.Name == passedScenario })
			if index < 0 {
				t.Fatalf("scenarios = %+v, want the undriven pass kept on the record", findings.Scenarios)
			}
			kept := findings.Scenarios[index]
			isKeptAsPassed := kept.Result == types.ScenarioResultPass && kept.Live && kept.Evidence == "checkout.png"
			isUntested := kept.Result == types.ScenarioResultUntested && !kept.Live && kept.Evidence == ""
			if tc.wantKept && !isKeptAsPassed {
				t.Fatalf("undriven pass = %+v, want it kept as the live pass this head recorded", kept)
			}
			if !tc.wantKept && !isUntested {
				t.Fatalf("undriven pass = %+v, want it untested with no live claim", kept)
			}
			wantCarried := 0
			if tc.wantKept {
				wantCarried = 1
			}
			if got := countOf(findings.Tested, keptTested); got != wantCarried || !slices.Contains(findings.Tested, "drove the retry") {
				t.Fatalf("tested = %q, want %q %d time(s) beside the re-check's own entry", findings.Tested, keptTested, wantCarried)
			}
			if got := countOf(findings.Artifacts, keptArtifact); got != wantCarried {
				t.Fatalf("artifacts = %+v, want %+v %d time(s)", findings.Artifacts, keptArtifact, wantCarried)
			}
			if findings.Verdict != types.TestVerdictNoGo || !outcome.NeedsApproval {
				t.Fatalf("verdict = %q, needs approval = %v, want the failure still parking", findings.Verdict, outcome.NeedsApproval)
			}
			if outcome.AutoFixable == tc.wantKept {
				t.Fatalf("auto-fixable = %v, want %v: a no-go right after a repair that changed nothing on this head parks for the operator", outcome.AutoFixable, !tc.wantKept)
			}
			if got := sctx.Shared.LiveCheckHead(); got != sctx.Run.HeadSHA {
				t.Fatalf("live check head = %q, want the head this evidence turn completed on, %q", got, sctx.Run.HeadSHA)
			}
		})
	}
}

func countOf[T comparable](list []T, want T) int {
	count := 0
	for _, item := range list {
		if item == want {
			count++
		}
	}
	return count
}

func TestFocusedRecheckSection(t *testing.T) {
	t.Parallel()
	last := Findings{
		TestedHeadSHA: strings.Repeat("c", 40),
		Scenarios:     []types.TestScenario{{Name: "a\nforged instruction", Result: types.ScenarioResultFail, Reason: "the button is gone"}},
	}
	cases := map[string]struct {
		last         Findings
		paths        []string
		isOnSameHead bool
		want         []string
		never        []string
	}{
		"no earlier live check": {last: Findings{}, paths: []string{"a.go"}},
		"a repair that changed files": {
			last:  last,
			paths: []string{"a.go"},
			want:  []string{"Focused re-check after a repair", "the button is gone", "a.go", "Drive again only", "Drive at least one scenario"},
			never: []string{"a\nforged instruction"},
		},
		"a repair that changed nothing": {
			last: last,
			want: []string{"The repair changed: nothing.", "Drive at least one scenario"},
		},
		"a repair that changed nothing on the head the live check ran on": {
			last:         last,
			isOnSameHead: true,
			want:         []string{"Focused re-check after a repair", "the button is gone", "The repair changed nothing, so this is still the head that live check ran on.", "run none of them again", "Report at least one scenario"},
			never:        []string{"a\nforged instruction", "The repair changed: nothing.", "Drive at least one scenario"},
		},
		"no earlier live check on the same head": {last: Findings{}, isOnSameHead: true},
		"an open failure reads as failed": {
			last: Findings{TestedHeadSHA: strings.Repeat("c", 40), Scenarios: []types.TestScenario{
				{Name: "retry", Result: types.ScenarioResultUntested, StillFailing: true},
			}},
			want: []string{"- fail: retry"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			section := focusedRecheckSection(tc.last, tc.paths, tc.isOnSameHead)

			if len(tc.want) == 0 && section != "" {
				t.Fatalf("section = %q, want none", section)
			}
			for _, want := range tc.want {
				if !strings.Contains(section, want) {
					t.Fatalf("section missing %q:\n%s", want, section)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(section, never) {
					t.Fatalf("section kept %q unsanitized:\n%s", never, section)
				}
			}
		})
	}
}

func TestKeepUndrivenScenarios(t *testing.T) {
	t.Parallel()
	const liveHead = "cccccccccccccccccccccccccccccccccccccccc"
	driven := types.TestScenario{Name: "driven", Result: types.ScenarioResultPass, Live: true, Evidence: "a.png"}
	cases := map[string]struct {
		verdict      string
		last         []types.TestScenario
		isOnSameHead bool
		wantVerdict  string
		wantReason   string
		wantKept     *types.TestScenario
	}{
		"nothing to keep": {verdict: types.TestVerdictGo, wantVerdict: types.TestVerdictGo},
		"a scenario driven again is not duplicated": {
			verdict:     types.TestVerdictGo,
			last:        []types.TestScenario{{Name: " driven ", Result: types.ScenarioResultFail, Live: true}},
			wantVerdict: types.TestVerdictGo,
		},
		"an undriven pass keeps where it last passed": {
			verdict:     types.TestVerdictGo,
			last:        []types.TestScenario{{Name: "other", Result: types.ScenarioResultPass, Live: true, Evidence: "b.png"}},
			wantVerdict: types.TestVerdictGo,
			wantReason:  "passed live at cccccccc",
		},
		"an undriven failure cannot leave a go standing": {
			verdict:     types.TestVerdictGo,
			last:        []types.TestScenario{{Name: "other", Result: types.ScenarioResultFail, Live: true}},
			wantVerdict: types.TestVerdictInconclusive,
			wantReason:  "failed live at cccccccc",
		},
		"an undriven failure leaves a no-go as it is": {
			verdict:     types.TestVerdictNoGo,
			last:        []types.TestScenario{{Name: "other", Result: types.ScenarioResultFail, Live: true}},
			wantVerdict: types.TestVerdictNoGo,
			wantReason:  "failed live at cccccccc",
		},
		"an untested scenario keeps its own reason": {
			verdict:     types.TestVerdictGo,
			last:        []types.TestScenario{{Name: "other", Result: types.ScenarioResultUntested, Reason: "no card sandbox"}},
			wantVerdict: types.TestVerdictGo,
			wantReason:  "no card sandbox",
		},
		"an undriven pass on the same head stays the live pass it was": {
			verdict:      types.TestVerdictGo,
			last:         []types.TestScenario{{Name: "other", Result: types.ScenarioResultPass, Live: true, Evidence: "b.png", Reason: "go vet passed"}},
			isOnSameHead: true,
			wantVerdict:  types.TestVerdictGo,
			wantKept:     &types.TestScenario{Name: "other", Result: types.ScenarioResultPass, Live: true, Evidence: "b.png", Reason: "go vet passed"},
		},
		"an undriven failure on the same head still cannot leave a go standing": {
			verdict:      types.TestVerdictGo,
			last:         []types.TestScenario{{Name: "other", Result: types.ScenarioResultFail, Live: true}},
			isOnSameHead: true,
			wantVerdict:  types.TestVerdictInconclusive,
			wantReason:   "failed live at cccccccc",
		},
		"an untested scenario on the same head keeps its own reason": {
			verdict:      types.TestVerdictGo,
			last:         []types.TestScenario{{Name: "other", Result: types.ScenarioResultUntested, Reason: "no card sandbox"}},
			isOnSameHead: true,
			wantVerdict:  types.TestVerdictGo,
			wantReason:   "no card sandbox",
		},
		"a failure left undriven for a second round still blocks a go": {
			verdict:     types.TestVerdictGo,
			last:        []types.TestScenario{{Name: "other", Result: types.ScenarioResultUntested, Reason: "failed live at cccccccc", StillFailing: true}},
			wantVerdict: types.TestVerdictInconclusive,
			wantReason:  "failed live at cccccccc",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			focused := Findings{Verdict: tc.verdict, Scenarios: []types.TestScenario{driven}}
			last := Findings{TestedHeadSHA: liveHead, Scenarios: tc.last}

			got := keepUndrivenScenarios(focused, last, tc.isOnSameHead, nil)

			if got.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", got.Verdict, tc.wantVerdict)
			}
			if got.Scenarios[0] != driven {
				t.Fatalf("driven scenario changed: %+v", got.Scenarios[0])
			}
			if tc.wantKept != nil {
				if len(got.Scenarios) != 2 || got.Scenarios[1] != *tc.wantKept {
					t.Fatalf("scenarios = %+v, want the recorded pass kept as %+v", got.Scenarios, *tc.wantKept)
				}
				return
			}
			if tc.wantReason == "" {
				if len(got.Scenarios) != 1 {
					t.Fatalf("scenarios = %+v, want only the driven one", got.Scenarios)
				}
				return
			}
			kept := got.Scenarios[1]
			if kept.Result != types.ScenarioResultUntested || kept.Live || kept.Evidence != "" || !strings.Contains(kept.Reason, tc.wantReason) {
				t.Fatalf("kept scenario = %+v, want untested with a reason holding %q", kept, tc.wantReason)
			}
		})
	}
}

// Decision 1021628: a pass kept on the same head brings the tested entries and
// artifacts of its live check, each once, and nothing else carries them.
func TestKeepUndrivenScenarios_AKeptPassCarriesItsRecord(t *testing.T) {
	t.Parallel()
	vet := types.TestArtifact{Kind: "log", Label: "go vet", Path: "vet.txt"}
	shot := types.TestArtifact{Kind: "image", Label: "checkout", Path: "checkout.png"}
	last := Findings{
		TestedHeadSHA: strings.Repeat("c", 40),
		Tested:        []string{"make test passed", "go vet ./... passed", "drove the retry"},
		Artifacts:     []types.TestArtifact{vet, shot},
	}
	pass := types.TestScenario{Name: "checkout", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"}
	fail := types.TestScenario{Name: "retry", Result: types.ScenarioResultFail, Live: true, Evidence: "retry.png"}
	cases := map[string]struct {
		lastScenarios []types.TestScenario
		isOnSameHead  bool
		wantTested    []string
		wantArtifacts []types.TestArtifact
	}{
		"a kept pass brings each entry once": {
			lastScenarios: []types.TestScenario{pass, fail},
			isOnSameHead:  true,
			wantTested:    []string{"drove the retry", "go vet ./... passed"},
			wantArtifacts: []types.TestArtifact{vet, shot},
		},
		"no pass to keep on the same head carries nothing": {
			lastScenarios: []types.TestScenario{fail},
			isOnSameHead:  true,
			wantTested:    []string{"drove the retry"},
			wantArtifacts: []types.TestArtifact{vet},
		},
		"a moved head carries nothing": {
			lastScenarios: []types.TestScenario{pass, fail},
			wantTested:    []string{"drove the retry"},
			wantArtifacts: []types.TestArtifact{vet},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			focused := Findings{
				Verdict:   types.TestVerdictNoGo,
				Tested:    []string{"drove the retry"},
				Artifacts: []types.TestArtifact{vet},
				Scenarios: []types.TestScenario{{Name: "retry", Result: types.ScenarioResultFail, Live: true, Evidence: "retry-2.png"}},
			}
			last := last
			last.Scenarios = tc.lastScenarios

			got := keepUndrivenScenarios(focused, last, tc.isOnSameHead, []string{"make test passed"})

			if !slices.Equal(got.Tested, tc.wantTested) {
				t.Fatalf("tested = %q, want %q", got.Tested, tc.wantTested)
			}
			if !slices.Equal(got.Artifacts, tc.wantArtifacts) {
				t.Fatalf("artifacts = %+v, want %+v", got.Artifacts, tc.wantArtifacts)
			}
		})
	}
}

// A scenario that failed can only clear on a live pass. Reporting it untested
// this round must not read as resolving it, and passing it must.
func TestKeepUndrivenScenarios_AReportedFailureClearsOnlyOnAPass(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		result      string
		wasOpen     bool
		wantVerdict string
		wantOpen    bool
	}{
		"reported untested after failing":     {types.ScenarioResultUntested, false, types.TestVerdictInconclusive, true},
		"reported untested while still open":  {types.ScenarioResultUntested, true, types.TestVerdictInconclusive, true},
		"driven to a live pass after failing": {types.ScenarioResultPass, false, types.TestVerdictGo, false},
		"driven to a live pass while open":    {types.ScenarioResultPass, true, types.TestVerdictGo, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			earlier := types.TestScenario{Name: "retry", Result: types.ScenarioResultFail, Live: true}
			if tc.wasOpen {
				earlier = types.TestScenario{Name: "retry", Result: types.ScenarioResultUntested, StillFailing: true}
			}
			focused := Findings{Verdict: types.TestVerdictGo, Scenarios: []types.TestScenario{{Name: "retry", Result: tc.result}}}

			got := keepUndrivenScenarios(focused, Findings{TestedHeadSHA: strings.Repeat("c", 40), Scenarios: []types.TestScenario{earlier}}, false, nil)

			if got.Verdict != tc.wantVerdict || got.Scenarios[0].StillFailing != tc.wantOpen {
				t.Fatalf("verdict = %q, still failing = %v, want %q, %v", got.Verdict, got.Scenarios[0].StillFailing, tc.wantVerdict, tc.wantOpen)
			}
		})
	}
}

// Both records say what the step itself ran, so an agent's output can set neither.
func TestParseTestAnalyzerOutput_StepRecordsCannotBeForged(t *testing.T) {
	t.Parallel()
	forged := strings.Replace(passingScenarioFindingsJSON, `"findings": [],`,
		`"findings": [], "baseline_failure": {"tests": ["forged"]}, "focused_check": {"command": "forged", "exit_code": 0, "head_sha": "abc"},`, 1)
	forged = strings.Replace(forged, `"reason":""}`, `"reason":"","still_failing":true}`, 1)
	if !strings.Contains(forged, "focused_check") || !strings.Contains(forged, "still_failing") {
		t.Fatal("fixture did not take the forged records")
	}

	findings, err := parseTestAnalyzerOutput(&agent.Result{Output: json.RawMessage(forged)})
	if err != nil {
		t.Fatal(err)
	}

	if findings.BaselineFailure != nil || findings.FocusedCheck != nil || findings.Scenarios[0].StillFailing {
		t.Fatalf("agent output set a step record: %+v, %+v, %+v", findings.BaselineFailure, findings.FocusedCheck, findings.Scenarios)
	}
}

func TestNamedBaselineFailure(t *testing.T) {
	t.Parallel()
	team := func(output string) checkResult { return checkResult{Command: "make test", ExitCode: 1, Output: output} }
	listed := strings.Repeat("--- FAIL: TestOne\n", 1)
	for i := range baseAttributionMaxListed + 1 {
		listed += fmt.Sprintf("--- FAIL: TestMany%d\n", i)
	}
	cases := map[string]struct {
		failed         []checkResult
		wantNil        bool
		wantTests      int
		wantIncomplete bool
	}{
		"only a machine-local check failed": {
			failed:  []checkResult{{Command: "exit 9", ExitCode: 9, Local: true, Output: namedFailure}},
			wantNil: true,
		},
		"every failure named":      {failed: []checkResult{team(namedFailure + "\n")}, wantTests: 1},
		"output names no failure":  {failed: []checkResult{team("make: *** Error 1\n")}, wantIncomplete: true},
		"a package failed unnamed": {failed: []checkResult{team("FAIL\texample.com/suite\t0.1s\n")}, wantIncomplete: true},
		"pytest reported an error beside a failure": {
			failed:         []checkResult{team("FAILED tests/test_a.py::test_one - assert 1 == 2\nERROR tests/test_b.py::test_two - fixture db not found\n1 failed, 1 error in 0.12s\n")},
			wantTests:      1,
			wantIncomplete: true,
		},
		"a jest suite failed to run beside a failure": {
			failed:         []checkResult{team("  ✕ renders the header (12 ms)\n  ● Test suite failed to run\n")},
			wantTests:      1,
			wantIncomplete: true,
		},
		"more failures than listed":  {failed: []checkResult{team(listed)}, wantTests: baseAttributionMaxListed, wantIncomplete: true},
		"a named line cut to length": {failed: []checkResult{team("--- FAIL: Test" + strings.Repeat("x", baseAttributionMaxLineRunes+5) + "\n")}, wantTests: 1, wantIncomplete: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := namedBaselineFailure(tc.failed, "attribution")

			if tc.wantNil {
				if got != nil {
					t.Fatalf("failure = %+v, want none", got)
				}
				return
			}
			if got == nil || len(got.Tests) != tc.wantTests || got.Incomplete != tc.wantIncomplete || got.Attribution != "attribution" {
				t.Fatalf("failure = %+v, want %d tests, incomplete %v", got, tc.wantTests, tc.wantIncomplete)
			}
		})
	}
}

// The prompt no longer orders building, and every setting that shapes the turn
// still reaches it.
func TestTestStep_EvidencePromptOrdersNoBuildingWithEveryLiveCheckSetting(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestLiveCheckBudget = 15 * time.Minute
	sctx.Config.Test.Environment = "scripts/live-env.ps1 starts the app"
	sctx.Config.Test.SurfacePaths = []string{"*.txt"}

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want the evidence turn on a change inside test.surface_paths", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"at most 15m0s of wall-clock time",
		"Live check budget",
		"Prepared environment (trusted, from the default branch):",
		"scripts/live-env.ps1 starts the app",
		`Report that scenario "untested" at once, naming what is missing and how to provide it`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("evidence prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, duty := range []string{
		"Getting every scenario live is your responsibility",
		"build a disposable one yourself",
		"No environment was provided",
	} {
		if strings.Contains(prompt, duty) {
			t.Fatalf("evidence prompt still carries the duty %q", duty)
		}
	}
}
