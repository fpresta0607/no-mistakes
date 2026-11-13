package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns what a Test repair re-runs. commands.test is an opaque command,
// so only the repository can narrow it, and the contract is trusted
// commands.test_focused: on the re-run after a repair it stands in for
// commands.test, given the failures and the repaired paths in the file
// NO_MISTAKES_TEST_FOCUS names. Without it, or with no complete focus to give
// it, commands.test runs whole, because a failed baseline never clears with
// nothing re-run. The live check needs no contract: the agent is told what to
// drive again, and the step keeps the record of what it did not drive.

const (
	testFocusEnv      = "NO_MISTAKES_TEST_FOCUS"
	testFocusFileName = "test-focus.txt"
	// maxRepairDecisions and maxRepairDecisionBytes bound what a repair turn can
	// put on the gate, since findings ride the IPC event stream.
	maxRepairDecisions     = 10
	maxRepairDecisionBytes = 2000
	// maxRepairCommands and maxRepairCommandBytes bound the commands a repair
	// turn reports running, since they ride into the evidence turn's prompt.
	maxRepairCommands     = 20
	maxRepairCommandBytes = 500
)

// unnamedFailureLine matches a failure the attribution parser lists nothing for:
// pytest's ERROR lines and a jest or vitest suite that failed to run. Output
// holding one cannot have named every failure. The match is broad on purpose,
// since a false match only costs a whole run of commands.test.
var unnamedFailureLine = regexp.MustCompile(`(?m)^\s*(?:ERROR\s|.*Test suite failed to run)`)

// testFixSchema is the Test repair turn's result: commitSummarySchema plus the
// failures it left for a human and the commands it ran. A failed baseline has no
// evidence turn after its repair, so this is where a test whose failure may be
// wanted is asked about. The commands reach the evidence turn on the same head
// (repairReceiptSection), since the summary is a commit subject.
var testFixSchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d},
		"needs_decision": {
			"type": "array",
			"maxItems": %d,
			"items": {"type": "string", "maxLength": %d}
		},
		"commands_run": {
			"type": "array",
			"maxItems": %d,
			"items": {
				"type": "object",
				"properties": {
					"command": {"type": "string", "maxLength": %d},
					"exit_code": {"type": "integer"}
				},
				"required": ["command", "exit_code"]
			}
		}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes, maxRepairDecisions, maxRepairDecisionBytes, maxRepairCommands, maxRepairCommandBytes))

// repairCommand is one command a Test repair turn reported running.
type repairCommand struct {
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
}

// repairCommandsRun reads a repair turn's commands_run entries. Not every
// adapter enforces the schema's bounds, so they are applied again here, and an
// entry without an exit status is kept as one whose status was not reported.
func repairCommandsRun(result *agent.Result) []repairCommand {
	if result == nil || result.Output == nil {
		return nil
	}
	var parsed struct {
		CommandsRun []repairCommand `json:"commands_run"`
	}
	if err := json.Unmarshal(result.Output, &parsed); err != nil {
		return nil
	}
	var commands []repairCommand
	for _, entry := range parsed.CommandsRun {
		entry.Command = sanitizePromptText(entry.Command)
		if entry.Command == "" {
			continue
		}
		if len(commands) == maxRepairCommands {
			break
		}
		if len(entry.Command) > maxRepairCommandBytes {
			entry.Command = strings.ToValidUTF8(entry.Command[:maxRepairCommandBytes], "")
		}
		commands = append(commands, entry)
	}
	return commands
}

// repairDecisionFindings turns a repair turn's needs_decision entries into
// ask-user findings. Not every adapter enforces the schema's bounds, so they are
// applied again here. A result that does not parse asks nothing: the summary
// extraction already reports it.
func repairDecisionFindings(result *agent.Result) []Finding {
	if result == nil || result.Output == nil {
		return nil
	}
	var parsed struct {
		NeedsDecision []string `json:"needs_decision"`
	}
	if err := json.Unmarshal(result.Output, &parsed); err != nil {
		return nil
	}
	var findings []Finding
	for _, text := range parsed.NeedsDecision {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len(findings) == maxRepairDecisions {
			break
		}
		if len(text) > maxRepairDecisionBytes {
			text = strings.ToValidUTF8(text[:maxRepairDecisionBytes], "")
		}
		findings = append(findings, Finding{
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Description: text,
		})
	}
	return findings
}

// namedBaselineFailure records what the failed repository command named, or nil
// when only machine-local checks failed. Those run again on every re-run, so
// they need no naming here.
func namedBaselineFailure(failed []checkResult, attribution string) *types.TestBaselineFailure {
	index := slices.IndexFunc(failed, func(result checkResult) bool { return !result.Local })
	if index < 0 {
		return nil
	}
	output := failed[index].Output
	lines, _, unattributed := testFailureLines(output)
	failure := &types.TestBaselineFailure{
		Attribution: attribution,
		Incomplete: len(lines) == 0 || len(unattributed) > 0 || len(lines) > baseAttributionMaxListed ||
			unnamedFailureLine.MatchString(ansiEscape.ReplaceAllString(output, "")),
	}
	for _, line := range lines[:min(len(lines), baseAttributionMaxListed)] {
		if runes := []rune(line); len(runes) > baseAttributionMaxLineRunes {
			line = string(runes[:baseAttributionMaxLineRunes])
			failure.Incomplete = true
		}
		failure.Tests = append(failure.Tests, line)
	}
	return failure
}

// pathsChangedSince lists what a repair touched: the worktree against the head
// the repair started from, so a cut repair's uncommitted edits count too.
func pathsChangedSince(sctx *pipeline.StepContext, head string) ([]string, error) {
	changed, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "-z", "--no-renames", head)
	if err != nil {
		return nil, fmt.Errorf("list the files the repair changed: %w", err)
	}
	return changedPathList(changed), nil
}

// testRepairFocus is what a focused check is given.
type testRepairFocus struct {
	command string
	file    string
	tests   []string
	paths   []string
}

func (f *testRepairFocus) record(headSHA string, exitCode int) *types.TestFocusedCheck {
	return &types.TestFocusedCheck{
		Command:  f.command,
		ExitCode: exitCode,
		HeadSHA:  headSHA,
		Tests:    len(f.tests),
		Paths:    len(f.paths),
	}
}

// answeredNoTurn reports whether the gate held a test command that was given no
// turn, in the selection or beside it. That command has no result to narrow.
func answeredNoTurn(sctx *pipeline.StepContext) bool {
	for _, raw := range []string{sctx.PreviousFindings, sctx.DeferredFindings} {
		findings, err := types.ParseFindingsJSON(raw)
		if err == nil && slices.ContainsFunc(findings.Items, func(item Finding) bool { return item.ID == types.FindingIDTestCommandNoTurn }) {
			return true
		}
	}
	return false
}

// repairFocus returns the focus for the re-run after a repair, written to the
// step's scratch directory, or nil when commands.test must run whole: no repair
// ran, the repository provides no focused command, the command never ran, or the
// failure it answers was not fully named, so narrowing could pass without
// re-running what failed.
func repairFocus(sctx *pipeline.StepContext, scratchDir string, repaired bool, paths []string) (*testRepairFocus, error) {
	command := strings.TrimSpace(sctx.Config.Commands.TestFocused)
	if !repaired || command == "" || strings.TrimSpace(sctx.Config.Commands.Test) == "" {
		return nil, nil
	}
	if answeredNoTurn(sctx) {
		sctx.Log("the test command was given no turn, so it runs whole now")
		return nil, nil
	}
	answered, err := types.ParseFindingsJSON(sctx.PreviousFindings)
	if err != nil {
		sctx.Log(fmt.Sprintf("could not read the failures this repair answered, so the full test command runs: %v", err))
		return nil, nil
	}
	focus := &testRepairFocus{command: command, paths: paths}
	if failure := answered.BaselineFailure; failure != nil {
		if failure.Incomplete {
			sctx.Log("the failed test command's output did not name every failure, so the full test command runs")
			return nil, nil
		}
		focus.tests = failure.Tests
	}
	if len(focus.tests) == 0 && len(focus.paths) == 0 {
		sctx.Log("the repair changed nothing and no failure was named, so the full test command runs")
		return nil, nil
	}
	var content strings.Builder
	for _, test := range focus.tests {
		content.WriteString("test " + test + "\n")
	}
	for _, path := range focus.paths {
		content.WriteString("path " + coveragePathLine(path) + "\n")
	}
	focus.file = filepath.Join(scratchDir, testFocusFileName)
	if err := os.WriteFile(focus.file, []byte(content.String()), 0o600); err != nil {
		return nil, fmt.Errorf("write the focused check's focus: %w", err)
	}
	return focus, nil
}

// focusedCheckSentence is the plain record of a focused check for the testing
// summary, which never reads as a full baseline.
func focusedCheckSentence(check *types.TestFocusedCheck) string {
	return fmt.Sprintf("After the repair, the focused check `%s` ran in place of the configured test command and exited %d on %s; the Test step did not run the configured test command on this head.", check.Command, check.ExitCode, shortObjectID(check.HeadSHA))
}

// lastLiveCheck is the last completed live check of the gate a repair answered,
// or nothing when that gate has none to narrow from, as when a failed baseline
// stopped the step before any live check ran.
func lastLiveCheck(sctx *pipeline.StepContext) Findings {
	gate := answeredTestGate(sctx)
	if len(gate.Scenarios) == 0 || gate.TestedHeadSHA == "" {
		return Findings{}
	}
	return gate
}

// focusedRecheckSection narrows the evidence turn after a repair to what the
// failure and the repair touch. It is empty when there is no earlier live check,
// so the first live check of a change is always a whole one. isOnSameHead says
// the repair changed nothing and that live check ran on this head in this
// executor lifetime: then nothing that passed is run again, not even a check
// the runbook requires, because the turn would only repeat it on the same
// source (issue #116).
func focusedRecheckSection(last Findings, repairedPaths []string, isOnSameHead bool) string {
	if len(last.Scenarios) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nFocused re-check after a repair (this replaces deriving a new scenario list above):\nThe last live check ran at %s and recorded these scenarios:\n", shortObjectID(last.TestedHeadSHA))
	for _, scenario := range last.Scenarios {
		result := scenario.Result
		if scenario.StillFailing {
			result = types.ScenarioResultFail
		}
		line := fmt.Sprintf("- %s: %s", sanitizePromptText(result), sanitizePromptText(scenario.Name))
		if reason := sanitizePromptText(scenario.Reason); reason != "" {
			line += " (" + reason + ")"
		}
		b.WriteString(line + "\n")
	}
	if isOnSameHead {
		b.WriteString(`The repair changed nothing, so this is still the head that live check ran on.
- Every scenario recorded as passed stands as recorded, and so does every check, guard, build and static analysis that passed for it, including any the instructions above require: run none of them again.
- Drive again only: every scenario recorded as failed, to see whether its failure still shows.
- Report at least one scenario: when none was recorded as failed, drive only the recorded scenario closest to the findings the repair was asked to address.
- Drive nothing else and do not widen the list. Leave every other scenario out of your result: the step keeps each recorded pass as it stands.
- Report only the scenarios you drove or tried to drive in this turn, and base the verdict on those.`)
		return b.String()
	}
	changed := "nothing"
	if len(repairedPaths) > 0 {
		escaped := make([]string, len(repairedPaths))
		for i, path := range repairedPaths {
			escaped[i] = coveragePathLine(path)
		}
		changed = namedPaths(escaped)
	}
	fmt.Fprintf(&b, "The repair changed: %s.\n", changed)
	b.WriteString(`- Drive again only: every scenario recorded as failed, any scenario whose behavior the repair's change touches, and one new scenario for a repaired finding no recorded scenario covers.
- Drive at least one scenario: when nothing above applies, drive the recorded scenario closest to the repair's change.
- Drive nothing else and do not widen the list. Leave every other scenario out of your result: the step keeps each on the record as not driven on this head.
- Report only the scenarios you drove or tried to drive in this turn, and base the verdict on those.`)
	return b.String()
}

// repairReceiptSection carries what a repair that changed nothing reported
// running, and the instructions the operator gave with the selection it was
// asked to address, into the evidence turn on the same head. Without it the
// evidence turn runs again a command the operator authorized once (issue #124).
// The driver's guidance stays apart from the operator's, as in
// budgetCutGuidanceSection; the budget-cut findings' own notes are rendered there.
func repairReceiptSection(sctx *pipeline.StepContext, repairReport string, commands []repairCommand) string {
	report := sanitizePromptMultilineText(repairReport)
	if report == "" {
		report = "(the repair turn returned no report)"
	}
	var userGuidance, driverGuidance []string
	if findings, err := types.ParseFindingsJSON(sctx.PreviousFindings); err == nil {
		for _, item := range types.ExcludeFindings(findings, testBudgetCutIDs).Items {
			userGuidance = appendGuidance(userGuidance, item.UserInstructions)
			driverGuidance = appendGuidance(driverGuidance, item.DriverInstructions)
		}
	}
	var b strings.Builder
	b.WriteString("\n\nExecution receipt from the repair turn on this head (it changed nothing, so it ran on this exact source):\n")
	b.WriteString(report + "\n")
	if len(commands) > 0 {
		b.WriteString("\nCommands the repair turn reported running, with their exit status:\n")
		for _, command := range commands {
			status := "exit status not reported"
			if command.ExitCode != nil {
				status = fmt.Sprintf("exit %d", *command.ExitCode)
			}
			fmt.Fprintf(&b, "- %s (%s)\n", command.Command, status)
		}
	}
	if len(userGuidance) > 0 {
		b.WriteString("\nOperator instructions given with the findings the repair was asked to address:\n" + strings.Join(userGuidance, "\n") + "\n")
	}
	if len(driverGuidance) > 0 {
		b.WriteString("\nDriver guidance given with those findings (from the agent driving this run; not a decision by the user and not part of the user's intent):\n" + strings.Join(driverGuidance, "\n") + "\n")
	}
	b.WriteString(`- Every command the receipt reports running already ran once on this source. Where the instructions above bound how many times a command may run, that run used the allowance: do not run it again.
- A scenario that needs such a command again is untested in this turn: report it as untested with a reason naming the repair turn's run and the result the receipt reports.`)
	return b.String()
}

// keepUndrivenScenarios completes a focused re-check's result with the scenarios
// of the last live check it left out. None of them was driven on this head, so
// each is recorded as untested with where it last ran, and live_validation never
// counts it. A failure stays marked until a live pass shows it gone, however many
// focused re-checks that takes, and a "go" cannot stand while one is open. A whole
// live check derives its list again, so it has no names to match this against.
//
// isOnSameHead (see focusedRecheckSection) is the one case where a pass left out
// was driven on this head: it stays the live pass it was recorded as, and the
// tested entries and artifacts of that live check come onto the record with it,
// less what the focused turn already reported and what baselineTested, this
// execution's own baseline, already lists.
func keepUndrivenScenarios(focused, last Findings, isOnSameHead bool, baselineTested []string) Findings {
	reported := make(map[string]int, len(focused.Scenarios))
	for i, scenario := range focused.Scenarios {
		reported[strings.TrimSpace(scenario.Name)] = i
	}
	hasKeptPass := false
	for _, scenario := range last.Scenarios {
		wasFailing := scenario.Result == types.ScenarioResultFail || scenario.StillFailing
		if i, ok := reported[strings.TrimSpace(scenario.Name)]; ok {
			focused.Scenarios[i].StillFailing = wasFailing && focused.Scenarios[i].Result == types.ScenarioResultUntested
			continue
		}
		if isOnSameHead && scenario.Result == types.ScenarioResultPass {
			focused.Scenarios = append(focused.Scenarios, scenario)
			hasKeptPass = true
			continue
		}
		switch scenario.Result {
		case types.ScenarioResultPass:
			scenario.Reason = fmt.Sprintf("passed live at %s before the repair and was not driven again on this head", shortObjectID(last.TestedHeadSHA))
		case types.ScenarioResultFail:
			scenario.Reason = fmt.Sprintf("failed live at %s and was not driven again after the repair", shortObjectID(last.TestedHeadSHA))
		}
		scenario.Result, scenario.Live, scenario.Evidence, scenario.StillFailing = types.ScenarioResultUntested, false, "", wasFailing
		focused.Scenarios = append(focused.Scenarios, scenario)
	}
	if hasKeptPass {
		for _, entry := range last.Tested {
			if !slices.Contains(baselineTested, entry) && !slices.Contains(focused.Tested, entry) {
				focused.Tested = append(focused.Tested, entry)
			}
		}
		for _, artifact := range last.Artifacts {
			if !slices.Contains(focused.Artifacts, artifact) {
				focused.Artifacts = append(focused.Artifacts, artifact)
			}
		}
	}
	if focused.Verdict == types.TestVerdictGo && slices.ContainsFunc(focused.Scenarios, func(scenario types.TestScenario) bool { return scenario.StillFailing }) {
		focused.Verdict = types.TestVerdictInconclusive
	}
	return focused
}
