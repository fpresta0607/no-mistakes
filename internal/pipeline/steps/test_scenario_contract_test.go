package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTestStep_PromptDerivesScenariosAndMarksLive pins the live-validation
// prompt contract: the step asks for named scenarios driven against the real
// product, agent-owned workarounds (disposable, isolated setups it builds
// itself) to get each scenario live, an explicit live marking that a unit test
// cannot claim, untested reserved for scenarios that are truly impossible to
// drive live with a reason saying what was tried, and a verdict. The
// pre-contract framing that let a green unit-test run stand in for driving the
// product must be gone.
func TestTestStep_PromptDerivesScenariosAndMarksLive(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		// Scenario derivation, not test selection.
		"Derive the scenarios this change must satisfy, then run each one against the real running product",
		"Turn that intent into a short list of named scenarios",
		"one concrete thing an end user does and one observable result that proves it",
		"add an adversarial scenario that actively tries to break it",
		// Live is a claim about what actually ran.
		"drive each scenario end-to-end against that running product",
		"give the pty a non-zero window size (TIOCSWINSZ)",
		"drain the master",
		"terminal reported a zero-sized grid",
		"a live UI check silently becomes a fake",
		`Mark a scenario "live": true ONLY when you drove it against the real product in this run`,
		"A unit test, a stub, a mock, a recorded fixture, or reading the code is NOT live",
		// The agent owns workarounds to get each scenario live, and whatever
		// it builds for that stays disposable and isolated.
		"Getting every scenario live is your responsibility",
		"build a disposable one yourself",
		"point the real product at it through whatever isolation the product supports",
		"A missing environment is a problem to solve, not a reason to skip the scenario",
		"a fixture that stands in for the product itself is not",
		"Everything you build must stay disposable and isolated",
		"never read or write the operator's real data, real configuration, or shared services",
		// Untested is reserved for the truly impossible and says what was tried.
		`Return a scenario with result "untested" only when live validation is truly impossible here`,
		"state what you tried in order to drive it live and why each attempt cannot work",
		"naming the specific tool, credential, permission, or authority that is out of reach and how to provide it",
		`"No environment was provided" is not such a reason while you could have built a disposable one`,
		"Never guess a pass",
		"an honest \"untested\" after exhausting your workarounds costs nothing and a guessed \"pass\" costs everything",
		"reported as an untested scenario with its reason, NOT as a finding",
		// The verdict and what it does.
		`Return a "verdict"`,
		`A "no-go" verdict parks this step for a decision`,
		`A "no-surface" verdict parks for a human to decide whether to proceed without live validation`,
		"no runtime product surface no-mistakes can drive live",
		"never mark those as pass",
		"never use no-surface to skip live validation of a change that does have a product surface",
		"Untested scenarios are listed on the pull request and do not park by themselves",
		// The targeted-validation boundary survives the rewrite.
		"Do NOT run the complete repository test suite",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected evidence prompt to contain %q\nprompt:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		"run the smallest relevant tests yourself",
		"Look for existing tests that would generate sufficient evidence",
		// The old contract let any missing capability justify untested.
		"When a scenario cannot be driven live here",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("evidence prompt still carries pre-contract framing %q", forbidden)
		}
	}

	var schema struct {
		Properties struct {
			Scenarios struct {
				Items struct {
					Properties struct {
						Reason struct {
							Description string `json:"description"`
						} `json:"reason"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"scenarios"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ag.calls[0].JSONSchema, &schema); err != nil {
		t.Fatalf("decode delivered evidence schema: %v", err)
	}
	reason := schema.Properties.Scenarios.Items.Properties.Reason.Description
	for _, want := range []string{
		"what was tried to drive this scenario live and why live validation is impossible",
		"why there is no live-validatable surface",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("delivered schema's untested reason description = %q, want it to contain %q", reason, want)
		}
	}
}

func TestTestStep_PromptIncludesOnlyConfiguredTrustedRunbook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		instructions string
		wantRunbook  bool
	}{
		{name: "none configured"},
		{name: "configured", instructions: "Start the app with `make dev` and drive checkout.", wantRunbook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Test.Instructions = tc.instructions

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			prompt := ag.calls[0].Prompt
			if got := strings.Contains(prompt, "Repository live-validation runbook (trusted, from the default branch):"); got != tc.wantRunbook {
				t.Fatalf("runbook section present = %v, want %v\nprompt:\n%s", got, tc.wantRunbook, prompt)
			}
			if tc.wantRunbook && !strings.Contains(prompt, tc.instructions) {
				t.Fatalf("prompt omitted configured runbook:\n%s", prompt)
			}
		})
	}
}

func TestTestStep_FailingBaselineStillRunsEvidenceTurn(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	testCmd := "printf 'baseline broke'; exit 7"
	if runtime.GOOS == "windows" {
		testCmd = "echo baseline broke && exit /b 7"
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("evidence agent calls = %d, want 1", calls)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 7 {
		t.Fatalf("outcome = %+v, want blocking auto-fixable baseline failure", outcome)
	}
	if findings.Verdict != types.TestVerdictGo || len(findings.Scenarios) != 1 {
		t.Fatalf("evidence contract was not retained: %+v", findings)
	}
	if len(findings.Tested) < 2 || findings.Tested[0] != testCmd {
		t.Fatalf("tested = %+v, want baseline followed by evidence checks", findings.Tested)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "configured test command failed with exit code 7") {
		t.Fatalf("baseline finding missing from %+v", findings.Items)
	}
	if findings.Items[0].Category != types.FindingCategoryTestCommand {
		t.Fatalf("finding category = %q, want %s", findings.Items[0].Category, types.FindingCategoryTestCommand)
	}
}

const passingScenarioFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["npm run e2e -- checkout"],
  "testing_summary": "drove checkout end to end",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""}],
  "verdict": "go"
}`

// TestTestStep_VerdictPolicy proves captain's call C2 = a end to end: a no-go
// verdict parks the step with a blocking finding, an untested scenario passes
// through without parking, a go verdict adds nothing, and a no-surface
// verdict parks as ask-user rather than hard-failing. All four keep the
// scenario record on the step so the PR can render it.
func TestTestStep_VerdictPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		output            string
		wantApproval      bool
		wantDescription   string
		wantAction        string
		wantScenarioCount int
	}{
		{
			name:              "go passes through",
			output:            passingScenarioFindingsJSON,
			wantApproval:      false,
			wantScenarioCount: 1,
		},
		{
			name: "untested is listed without parking",
			output: `{"findings":[],"summary":"","tested":["manual check"],"testing_summary":"partly driven","artifacts":[],
				"scenarios":[
					{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""},
					{"name":"payment declines are shown","result":"untested","live":false,"evidence":"","reason":"no card sandbox credential on this machine"}
				],"verdict":"go"}`,
			wantApproval:      false,
			wantScenarioCount: 2,
		},
		{
			name: "no-go parks",
			output: `{"findings":[],"summary":"","tested":["npm run e2e -- checkout"],"testing_summary":"checkout broke","artifacts":[],
				"scenarios":[{"name":"user reaches the success screen","result":"fail","live":true,"evidence":"checkout.png","reason":""}],
				"verdict":"no-go"}`,
			wantApproval:      true,
			wantDescription:   "live validation verdict: no-go",
			wantAction:        types.ActionAutoFix,
			wantScenarioCount: 1,
		},
		{
			name: "inconclusive parks for a human",
			output: `{"findings":[],"summary":"","tested":["read the diff"],"testing_summary":"nothing could be driven","artifacts":[],
				"scenarios":[{"name":"user reaches the success screen","result":"untested","live":false,"evidence":"","reason":"no browser on this machine"}],
				"verdict":"inconclusive"}`,
			wantApproval:      true,
			wantDescription:   "live validation verdict: inconclusive",
			wantAction:        types.ActionAskUser,
			wantScenarioCount: 1,
		},
		{
			name: "no-surface parks as ask-user",
			output: `{"findings":[],"summary":"","tested":["inspected .github/workflows/ci.yml"],"testing_summary":"CI workflow has no running product to drive","artifacts":[],
				"scenarios":[{"name":"Windows git-heavy shard runs the git-backed packages","result":"untested","live":false,"evidence":"","reason":"CI workflow YAML has no running product no-mistakes can drive"}],
				"verdict":"no-surface"}`,
			wantApproval:      true,
			wantDescription:   "this change has no live-validatable surface; proceed without live validation?",
			wantAction:        types.ActionAskUser,
			wantScenarioCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			output := tc.output
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(output)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.UserIntent = "Show users a success screen after checkout"

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval != tc.wantApproval {
				t.Fatalf("NeedsApproval = %v, want %v (findings: %s)", outcome.NeedsApproval, tc.wantApproval, outcome.Findings)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Scenarios) != tc.wantScenarioCount {
				t.Fatalf("recorded %d scenarios, want %d", len(findings.Scenarios), tc.wantScenarioCount)
			}
			if tc.wantDescription == "" {
				for _, item := range findings.Items {
					if strings.Contains(item.Description, "live validation verdict") || strings.Contains(item.Description, "no live-validatable surface") {
						t.Fatalf("unexpected verdict finding on a passing run: %q", item.Description)
					}
				}
				return
			}
			var matched *types.Finding
			for i, item := range findings.Items {
				if strings.Contains(item.Description, tc.wantDescription) {
					matched = &findings.Items[i]
				}
			}
			if matched == nil {
				t.Fatalf("no finding carrying %q in %s", tc.wantDescription, outcome.Findings)
			}
			if matched.Severity == types.FindingSeverityInfo {
				t.Fatalf("verdict finding must block, got severity %q", matched.Severity)
			}
			if tc.wantAction != "" && matched.Action != tc.wantAction {
				t.Fatalf("verdict finding action = %q, want %q", matched.Action, tc.wantAction)
			}
		})
	}
}

// TestTestStep_MissingScenarioContractFails proves the contract is required
// rather than advisory: an evidence turn that answers without scenarios or
// without a verdict has not answered, and a bad verdict is not silently
// accepted as "unknown".
func TestTestStep_MissingScenarioContractFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		output  string
		wantErr string
	}{
		{
			name:    "no scenarios array",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"verdict":"go"}`,
			wantErr: "missing scenarios array",
		},
		{
			name:    "empty scenarios array",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[],"verdict":"inconclusive"}`,
			wantErr: "empty scenarios array",
		},
		{
			name:    "no verdict",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"ok","reason":""}]}`,
			wantErr: "missing verdict",
		},
		{
			name:    "verdict outside the vocabulary",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"probably fine"}`,
			wantErr: "is not one of",
		},
		{
			name:    "scenario result outside the vocabulary",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"maybe","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "is not one of",
		},
		{
			name:    "unnamed scenario",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"  ","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "missing name",
		},
		{
			name:    "missing declared scenario fields",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass"}],"verdict":"go"}`,
			wantErr: "missing live",
		},
		{
			name:    "pass must be live",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":false,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: `result "pass" but live=false`,
		},
		{
			name:    "pass requires evidence",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"  ","reason":""}],"verdict":"go"}`,
			wantErr: "missing evidence",
		},
		{
			name:    "fail requires evidence",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"fail","live":true,"evidence":"","reason":""}],"verdict":"no-go"}`,
			wantErr: "missing evidence",
		},
		{
			name:    "untested cannot be live",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"untested","live":true,"evidence":"","reason":"no browser"}],"verdict":"inconclusive"}`,
			wantErr: `result "untested" but live=true`,
		},
		{
			name:    "untested without a reason",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"untested","live":false,"evidence":"","reason":""}],"verdict":"inconclusive"}`,
			wantErr: `result "untested" without a reason`,
		},
		{
			name:    "go cannot override failure",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"fail","live":true,"evidence":"failure","reason":""}],"verdict":"go"}`,
			wantErr: `verdict "go" contradicts failed scenario`,
		},
		{
			name:    "inconclusive cannot override failure",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"fail","live":true,"evidence":"failure","reason":""}],"verdict":"inconclusive"}`,
			wantErr: `verdict "inconclusive" contradicts failed scenario`,
		},
		{
			name:    "no-surface cannot cover a live pass",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"no-surface"}`,
			wantErr: `verdict "no-surface" contradicts live-exercisable scenario`,
		},
		{
			name:    "no-surface cannot cover a claimed pass without live",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":false,"evidence":"ok","reason":""}],"verdict":"no-surface"}`,
			wantErr: `result "pass" but live=false`,
		},
		{
			name:    "protocol vocabulary is exact",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"Pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "is not one of",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			output := tc.output
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(output)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			_, err := (&TestStep{}).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute() error = %v, want one naming %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("after %d attempts", testAnalyzerMaxAttempts)) {
				t.Fatalf("Execute() error = %v, want the exhausted correction bound named", err)
			}
			if len(ag.calls) != testAnalyzerMaxAttempts {
				t.Fatalf("agent calls = %d, want %d bounded correction attempts before failing", len(ag.calls), testAnalyzerMaxAttempts)
			}
		})
	}
}

const noSurfaceCIWorkflowFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["inspected .github/workflows/ci.yml"],
  "testing_summary": "CI workflow split has no running product to drive",
  "artifacts": [],
  "scenarios": [{"name":"Windows git-heavy shard runs the git-backed packages","result":"untested","live":false,"evidence":"","reason":"CI workflow YAML has no running product no-mistakes can drive"}],
  "verdict": "no-surface"
}`

// TestTestStep_NoLiveSurfaceCIWorkflowAsksUser models the captain's
// windows-shard failure: a CI-workflow-only change has nothing no-mistakes
// can drive live. The evidence turn must park as ask-user with the reason,
// not hard-fail the step the way pass+live:false used to.
func TestTestStep_NoLiveSurfaceCIWorkflowAsksUser(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	headSHA := commitCIWorkflowOnlyChange(t, dir, baseSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(noSurfaceCIWorkflowFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Split the Windows CI job into a git-heavy shard and a core remainder"

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("no-surface must park, not hard-fail: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = false, want ask-user park (findings: %s)", outcome.Findings)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if findings.Verdict != types.TestVerdictNoSurface {
		t.Fatalf("verdict = %q, want %q", findings.Verdict, types.TestVerdictNoSurface)
	}
	if !types.NoLiveExercisableScenarios(findings.Scenarios) {
		t.Fatalf("scenarios = %+v, want all untested and not live", findings.Scenarios)
	}
	var matched *types.Finding
	for i, item := range findings.Items {
		if strings.Contains(item.Description, "this change has no live-validatable surface; proceed without live validation?") {
			matched = &findings.Items[i]
			break
		}
	}
	if matched == nil {
		t.Fatalf("missing no-surface ask-user finding in %s", outcome.Findings)
	}
	if matched.Action != types.ActionAskUser {
		t.Fatalf("action = %q, want %q", matched.Action, types.ActionAskUser)
	}
	if matched.Severity != types.FindingSeverityWarning {
		t.Fatalf("severity = %q, want warning so the step parks without treating this as a defect", matched.Severity)
	}
	if !strings.Contains(matched.Description, "CI workflow YAML has no running product no-mistakes can drive") {
		t.Fatalf("finding omitted the scenario reason: %q", matched.Description)
	}
	if len(types.AutoFixableFindings(findings).Items) != 0 {
		t.Fatalf("no-surface must not be auto-fixable, got %s", outcome.Findings)
	}
}

const mixedLivePassAndUntestedFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["manual check"],
  "testing_summary": "partly driven",
  "artifacts": [],
  "scenarios": [
    {"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""},
    {"name":"payment declines are shown","result":"untested","live":false,"evidence":"","reason":"no card sandbox credential on this machine"}
  ],
  "verdict": "go"
}`

const passNotLiveFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["adapter stub"],
  "testing_summary": "adapter stubbed the scenario",
  "artifacts": [],
  "scenarios": [{"name":"adapter handles the request","result":"pass","live":false,"evidence":"stub","reason":""}],
  "verdict": "go"
}`

const untestedWithoutReasonFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["read the diff"],
  "testing_summary": "could not drive live",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"untested","live":false,"evidence":"","reason":""}],
  "verdict": "inconclusive"
}`

type rejectedStructuredOutputError struct{ message string }

func (e rejectedStructuredOutputError) Error() string                { return e.message }
func (rejectedStructuredOutputError) StructuredOutputRejected() bool { return true }

// TestTestStep_InvalidAnalyzerPayloadTriggersCorrectionRound is the
// recoverability contract: a pass that was not live-validated, or an
// untested scenario missing a reason, is returned to the analyzer with an
// actionable message so it can resubmit. The step must not fail the run on
// that first invalid payload.
func TestTestStep_InvalidAnalyzerPayloadTriggersCorrectionRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		invalid    string
		wantPrompt string
	}{
		{
			name:       "pass with live false",
			invalid:    passNotLiveFindingsJSON,
			wantPrompt: `scenario 1: result "pass" but live=false - if you did not drive this against the live product, mark it result "untested" with a reason instead of "pass"`,
		},
		{
			name:       "untested without a reason",
			invalid:    untestedWithoutReasonFindingsJSON,
			wantPrompt: `scenario 1: result "untested" without a reason - state what was tried to drive it live and why live validation is impossible, naming the specific tool, credential, permission, or authority out of reach and how to provide it`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			invalid := tc.invalid
			calls := 0
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					calls++
					if calls == 1 {
						return &agent.Result{Output: json.RawMessage(invalid)}, nil
					}
					return &agent.Result{Output: json.RawMessage(mixedLivePassAndUntestedFindingsJSON)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.UserIntent = "Show users a success screen after checkout"

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("invalid payload must be returned to the analyzer, not fail the step: %v", err)
			}
			if len(ag.calls) != 2 {
				t.Fatalf("agent calls = %d, want 1 rejected payload plus 1 correction", len(ag.calls))
			}
			first := ag.calls[0].Prompt
			if strings.Contains(first, "were REJECTED") {
				t.Fatalf("first evidence prompt must not be a correction round:\n%s", first)
			}
			correction := ag.calls[1].Prompt
			for _, want := range []string{
				"were REJECTED",
				"This is a correction-only turn",
				"Do not use tools, execute commands, start or modify the product, rerun scenarios, or perform any external operation",
				"Preserve its supported observations and findings without inventing new evidence",
				"Downgrade every unsupported pass or fail",
				"<rejected-json>",
				tc.wantPrompt,
			} {
				if !strings.Contains(correction, want) {
					t.Fatalf("correction prompt missing %q:\n%s", want, correction)
				}
			}
			for _, replayed := range []string{
				"You are validating a code change by driving the product itself",
				"Show users a success screen after checkout",
				"drive each scenario end-to-end against that running product",
			} {
				if strings.Contains(correction, replayed) {
					t.Fatalf("correction prompt replayed evidence-task instruction %q:\n%s", replayed, correction)
				}
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if findings.Verdict != types.TestVerdictGo || len(findings.Scenarios) != 2 {
				t.Fatalf("corrected payload was not accepted: %+v", findings)
			}
			if outcome.NeedsApproval {
				t.Fatalf("mixed live-pass + untested-with-reason must not park, findings: %s", outcome.Findings)
			}
		})
	}
}

func TestTestStep_FinalizerRejectionTriggersCorrectionRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return nil, rejectedStructuredOutputError{message: "structured output did not match schema: missing scenarios"}
			}
			return &agent.Result{Output: json.RawMessage(mixedLivePassAndUntestedFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("finalizer rejection must enter the bounded correction round: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want one rejected invocation plus one correction", len(ag.calls))
	}
	if !strings.Contains(ag.calls[1].Prompt, "structured output did not match schema: missing scenarios") {
		t.Fatalf("correction prompt omitted the finalizer error:\n%s", ag.calls[1].Prompt)
	}
	if outcome.NeedsApproval {
		t.Fatalf("valid corrected payload must not park, findings: %s", outcome.Findings)
	}
}

// TestTestStep_FinalizerRejectionCorrectsTheWholePayload replays
// fpresta0607/code-goblins#1561. Pi refused an evidence payload whose only
// defect was a scenario without a reason, and its error quotes just the first
// 200 characters. The correction turn was handed that snippet alone, rebuilt
// a payload around the one finding the snippet began, and parked the run on
// an ask-user finding about a file the change never touched. It must be
// handed the whole payload Pi refused, findings past the snippet included.
func TestTestStep_FinalizerRejectionCorrectsTheWholePayload(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	refused, err := json.Marshal(map[string]any{
		"findings": []map[string]any{
			{"id": "hidden-desktop-window-enumeration", "severity": "warning", "file": "internal/proc/window_windows.go", "line": 58, "description": "The configured test command failed only on a window enumeration test this change does not touch, a host capability of the hidden desktop.", "action": "no-op"},
			{"id": "custody-test-timing-race", "severity": "info", "file": "internal/supervisor/custody_test.go", "line": 93, "description": "The new custody test read the job before its child started; it now waits for the child.", "action": "no-op"},
		},
		"summary":         "Drove the engine selector end to end.",
		"tested":          []string{"`go test ./internal/supervisor`"},
		"testing_summary": "Drove the board API in a disposable home.",
		"artifacts":       []any{},
		"scenarios": []map[string]any{
			{"name": "a saved engine survives a restart", "result": "pass", "live": true, "evidence": "snapshot.json", "reason": ""},
			{"name": "an unknown model is refused", "result": "untested", "live": false, "evidence": ""},
		},
		"verdict": "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(refused[200:]), "custody-test-timing-race") {
		t.Fatal("the second finding must start past the 200-character snippet Pi's error quotes")
	}
	var lines []byte
	for _, event := range []any{
		map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": string(refused)}}}},
		map[string]any{"type": "agent_end", "messages": []any{}},
	} {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, line...), '\n')
	}
	piDir := t.TempDir()
	events := filepath.Join(piDir, "events.jsonl")
	if err := os.WriteFile(events, lines, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(piDir, "pi")
	script := "#!/bin/sh\ncat > /dev/null\ncat '" + events + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\nmore > nul\r\ntype \"" + events + "\"\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	pi, err := agent.New(types.AgentPi, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "were REJECTED") {
				return &agent.Result{Output: json.RawMessage(mixedLivePassAndUntestedFindingsJSON)}, nil
			}
			// A placed agent starts through the desktop helper this test
			// binary does not serve, and the desktop plays no part here.
			opts.Desktop = ""
			return pi.Run(ctx, opts)
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("Pi's refusal must enter the bounded correction round: %v", err)
	}

	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want the refused evidence turn plus one correction", len(ag.calls))
	}
	correction := ag.calls[1].Prompt
	for _, want := range []string{
		`missing required field "reason"`,
		"<rejected-json>",
		"custody-test-timing-race",
		"an unknown model is refused",
	} {
		if !strings.Contains(correction, want) {
			t.Fatalf("correction prompt missing %q, so the turn cannot correct the payload Pi refused:\n%s", want, correction)
		}
	}
}

func TestTestStep_MalformedJSONTriggersCorrectionRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(`{"findings": [}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(mixedLivePassAndUntestedFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("malformed JSON must enter the bounded correction round: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want one malformed payload plus one correction", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatalf("valid corrected payload must not park, findings: %s", outcome.Findings)
	}
}

// TestTestStep_InvalidAnalyzerPayloadExhaustsCorrectionBound proves the
// loop is bounded: a payload that stays invalid is a genuine blocking
// failure after testAnalyzerMaxAttempts, never an infinite resubmit.
func TestTestStep_InvalidAnalyzerPayloadExhaustsCorrectionBound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passNotLiveFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("a payload that stays invalid must fail after the correction bound")
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome after the bound is exhausted", outcome)
	}
	if len(ag.calls) != testAnalyzerMaxAttempts {
		t.Fatalf("agent calls = %d, want %d", len(ag.calls), testAnalyzerMaxAttempts)
	}
	got := err.Error()
	if !strings.Contains(got, fmt.Sprintf("after %d attempts", testAnalyzerMaxAttempts)) {
		t.Fatalf("error = %q, want the exhausted bound named", got)
	}
	if !strings.Contains(got, `result "pass" but live=false`) {
		t.Fatalf("error = %q, want the actionable validation reason", got)
	}
	if !strings.Contains(ag.calls[1].Prompt, `scenario 1: result "pass" but live=false`) {
		t.Fatalf("retry prompt never told the analyzer how to correct:\n%s", ag.calls[1].Prompt)
	}
}

func TestTestStep_ValidMixedPayloadDoesNotRetry(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(mixedLivePassAndUntestedFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1: a valid mixed payload must not enter a correction round", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatalf("mixed live-pass + untested-with-reason must not park, findings: %s", outcome.Findings)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if findings.Verdict != types.TestVerdictGo || len(findings.Scenarios) != 2 {
		t.Fatalf("valid mixed payload was not retained: %+v", findings)
	}
}

func commitCIWorkflowOnlyChange(t *testing.T, dir, baseSHA string) string {
	t.Helper()
	gitCmd(t, dir, "checkout", "-B", "feature", baseSHA)
	workflowDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workflowDir, "ci.yml")
	body := "name: CI\non: push\njobs:\n  test:\n    runs-on: windows-latest\n    steps:\n      - run: go test ./internal/git/...\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "split windows git shard")
	return gitCmd(t, dir, "rev-parse", "HEAD")
}
