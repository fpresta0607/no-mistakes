package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
)

// testFixSchema is the Test repair turn's result: commitSummarySchema plus the
// failures it left for a human. A failed baseline has no evidence turn after
// its repair, so this is where a test whose failure may be wanted is asked about.
var testFixSchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d},
		"needs_decision": {
			"type": "array",
			"maxItems": %d,
			"items": {"type": "string", "maxLength": %d}
		}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes, maxRepairDecisions, maxRepairDecisionBytes))

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
	lines, _, unattributed := testFailureLines(failed[index].Output)
	failure := &types.TestBaselineFailure{
		Attribution: attribution,
		Incomplete:  len(lines) == 0 || len(unattributed) > 0 || len(lines) > baseAttributionMaxListed,
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

// repairFocus returns the focus for the re-run after a repair, written to the
// step's scratch directory, or nil when commands.test must run whole: no repair
// ran, the repository provides no focused command, or the failure it answers was
// not fully named, so narrowing could pass without re-running what failed.
func repairFocus(sctx *pipeline.StepContext, scratchDir string, repaired bool, paths []string) (*testRepairFocus, error) {
	command := strings.TrimSpace(sctx.Config.Commands.TestFocused)
	if !repaired || command == "" || strings.TrimSpace(sctx.Config.Commands.Test) == "" {
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
	return fmt.Sprintf("After the repair, the focused check `%s` ran in place of the configured test command and exited %d on %s; the configured test command did not run on this head.", check.Command, check.ExitCode, shortObjectID(check.HeadSHA))
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
// so the first live check of a change is always a whole one.
func focusedRecheckSection(last Findings, repairedPaths []string) string {
	if len(last.Scenarios) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nFocused re-check after a repair (this replaces deriving a new scenario list above):\nThe last live check ran at %s and recorded these scenarios:\n", shortObjectID(last.TestedHeadSHA))
	for _, scenario := range last.Scenarios {
		line := fmt.Sprintf("- %s: %s", sanitizePromptText(scenario.Result), sanitizePromptText(scenario.Name))
		if reason := sanitizePromptText(scenario.Reason); reason != "" {
			line += " (" + reason + ")"
		}
		b.WriteString(line + "\n")
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
- Drive nothing else and do not widen the list. Leave every other scenario out of your result: the step keeps each on the record as not driven on this head.
- Report only the scenarios you drove or tried to drive in this turn, and base the verdict on those.`)
	return b.String()
}

// keepUndrivenScenarios completes a focused re-check's result with the scenarios
// of the last live check it left out. None of them was driven on this head, so
// each is recorded as untested with where it last ran, and live_validation never
// counts it. A failure left out was never shown gone, so a "go" cannot stand.
func keepUndrivenScenarios(focused, last Findings) Findings {
	reported := make(map[string]bool, len(focused.Scenarios))
	for _, scenario := range focused.Scenarios {
		reported[strings.TrimSpace(scenario.Name)] = true
	}
	for _, scenario := range last.Scenarios {
		if reported[strings.TrimSpace(scenario.Name)] {
			continue
		}
		switch scenario.Result {
		case types.ScenarioResultPass:
			scenario.Reason = fmt.Sprintf("passed live at %s before the repair and was not driven again on this head", shortObjectID(last.TestedHeadSHA))
		case types.ScenarioResultFail:
			scenario.Reason = fmt.Sprintf("failed live at %s and was not driven again after the repair", shortObjectID(last.TestedHeadSHA))
			if focused.Verdict == types.TestVerdictGo {
				focused.Verdict = types.TestVerdictInconclusive
			}
		}
		scenario.Result, scenario.Live, scenario.Evidence = types.ScenarioResultUntested, false, ""
		focused.Scenarios = append(focused.Scenarios, scenario)
	}
	return focused
}
