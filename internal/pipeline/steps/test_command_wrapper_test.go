package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// wrapperRanMarkerCommand leaves a file in the worktree, so a test can tell
// from the worktree whether the command ever ran. The step's log cannot say:
// it names the command before starting it.
const (
	wrapperRanMarker        = "wrapper-ran.marker"
	wrapperRanMarkerCommand = "echo ran> " + wrapperRanMarker
)

// wrapperFixture is a Test step context whose machine config puts the baseline
// test command behind a stand-in wrapper. The stand-in records its own
// arguments and then starts what follows "--", as a real wrapper does.
type wrapperFixture struct {
	sctx    *pipeline.StepContext
	agent   *mockAgent
	logFile string
	logs    *[]string
}

func newWrapperFixture(t *testing.T, cmds config.Commands, vars map[string]string) *wrapperFixture {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, cmds)
	if cmds.Prepare != "" {
		gateDir := t.TempDir()
		gitCmd(t, gateDir, "init", "--bare")
		sctx.GateDir = gateDir
		sctx.Shared = &pipeline.RunShared{}
	}

	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "turnline")
	program := filepath.Join(binDir, "turnline")
	if runtime.GOOS == "windows" {
		program += ".exe"
	}
	logFile := filepath.Join(t.TempDir(), "wrapper.ndjson")
	env := map[string]string{"FAKE_CLI_MODE": "wrapper", "FAKE_CLI_WRAPPER_LOG": logFile}
	for k, v := range vars {
		env[k] = v
	}
	sctx.Env = fakeCLIEnv(binDir, env)
	sctx.Config.TestCommandWrapper = config.CommandWrapper{
		Command:       []string{program, "--as", "{repo} {branch}, run {run_short}", "--"},
		RefusalPrefix: "turnline: ",
	}
	return &wrapperFixture{sctx: sctx, agent: ag, logFile: logFile, logs: liveCheckLogs(sctx)}
}

// calls returns the argument list of each wrapper start, in order.
func (f *wrapperFixture) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.logFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("wrapper log line %q: %v", line, err)
		}
		calls = append(calls, args)
	}
	return calls
}

// shellCall asserts that args, the words after "--", are the shell call the
// step starts with no wrapper, with the command string as ONE argument.
func assertShellCall(t *testing.T, args []string, command string) {
	t.Helper()
	if len(args) != 3 {
		t.Fatalf("words after -- = %q, want the shell, its flag, and the command as one argument", args)
	}
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd.exe", "/c"
	}
	if got := strings.ToLower(filepath.Base(args[0])); got != shell && got != strings.TrimSuffix(shell, ".exe") {
		t.Fatalf("shell = %q, want %s", args[0], shell)
	}
	if !filepath.IsAbs(args[0]) {
		t.Fatalf("shell = %q, want the resolved path the step starts today", args[0])
	}
	if args[1] != flag {
		t.Fatalf("shell flag = %q, want %q", args[1], flag)
	}
	if runtime.GOOS == "windows" {
		if args[2] != command {
			t.Fatalf("command argument = %q, want %q unchanged", args[2], command)
		}
	} else if !strings.Contains(args[2], command) {
		t.Fatalf("command argument = %q, want it to carry %q", args[2], command)
	}
}

func TestTestStep_WrapperStartsTheBaselineBehindItsWords(t *testing.T) {
	// The command joins two parts. Joined to the wrapper as a string, only the
	// first would stand behind it.
	const command = "echo first-part&& echo second-part"
	f := newWrapperFixture(t, config.Commands{Prepare: "echo prepared", Test: command}, map[string]string{
		"FAKE_CLI_WRAPPER_SAYS": "turnline: waiting for its turn (3m0s so far)",
	})

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("wrapper starts = %d (%q), want 1: the test command and nothing else, so not prepare", len(calls), calls)
	}
	args := calls[0]
	run8 := f.sctx.Run.ID
	if len(run8) > 8 {
		run8 = run8[:8]
	}
	wantName := "repo feature, run " + run8
	if len(args) < 3 || args[0] != "--as" || args[1] != wantName || args[2] != "--" {
		t.Fatalf("wrapper words = %q, want --as %q -- first, with the run's facts filled in", args, wantName)
	}
	assertShellCall(t, args[3:], command)

	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want a passing baseline read through the wrapper", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Tested) == 0 || findings.Tested[0] != command {
		t.Fatalf("tested = %q, want the repository's own command, not the wrapper", findings.Tested)
	}
	log := strings.Join(*f.logs, "\n")
	for _, want := range []string{"first-part", "second-part", "turnline: waiting for its turn (3m0s so far)"} {
		if !strings.Contains(log, want) {
			t.Fatalf("step log = %q, want the command's output and the wrapper's own line %q passed through", log, want)
		}
	}
	// A result produced behind a wrapper is never presented as a plain run.
	if !strings.Contains(log, "machine-local wrapper applied to commands.test") || !strings.Contains(log, "turnline") {
		t.Fatalf("step log = %q, want one line declaring the wrapper", log)
	}
}

func TestTestStep_WrapperPassesTheCommandsExitCodeThrough(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "exit 3"}, nil)

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 3 {
		t.Fatalf("outcome = %#v, want the command's own failure: parked, fixable, exit code 3", outcome)
	}
	findings := liveCheckFindings(t, outcome)
	if len(findings.Items) == 0 || findings.Items[0].Category != types.FindingCategoryTestCommand || !strings.Contains(findings.Items[0].Description, "exit code 3") {
		t.Fatalf("findings = %#v, want the configured test command's failure with its exit code", findings.Items)
	}
	if len(f.agent.calls) != 1 {
		t.Fatalf("agent calls = %d, want the live check after a failing baseline, as with no wrapper", len(f.agent.calls))
	}
}

func TestTestStep_WrapperAlsoStartsMachineLocalTestChecks(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "echo team-tests"}, nil)
	f.sctx.Config.CommandOverrides = map[string]config.CommandOverride{"test": {Additional: []string{"echo local-check"}}}

	if _, err := (&TestStep{}).Execute(f.sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("wrapper starts = %d, want 2: every baseline test check stands behind it", len(calls))
	}
	assertShellCall(t, calls[0][3:], "echo team-tests")
	assertShellCall(t, calls[1][3:], "echo local-check")
}

// With the setting absent nothing starts a wrapper: today's behavior.
func TestTestStep_NoWrapperStartsTheBaselineAsBefore(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "echo baseline-ran"}, nil)
	f.sctx.Config.TestCommandWrapper = config.CommandWrapper{}

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("wrapper starts = %q, want none with no wrapper configured", calls)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a passing baseline", outcome)
	}
	if log := strings.Join(*f.logs, "\n"); !strings.Contains(log, "baseline-ran") || strings.Contains(log, "wrapper") {
		t.Fatalf("step log = %q, want the command's output and no word of a wrapper", log)
	}
}

func TestLintStep_WrapperLeavesLintAlone(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Lint: "echo lint-ran"}, nil)

	if _, err := (&LintStep{}).Execute(f.sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("wrapper starts = %q, want none: the wrapper is for the baseline test command", calls)
	}
}

// The wrapper says it never ran the command. That is no test result, so no
// fix agent is asked to repair tests that never ran and no live check runs on
// a machine that had no room for the tests.
func TestTestStep_WrapperRefusalParksWithNoAgent(t *testing.T) {
	cases := map[string]struct {
		code string
		line string
	}{
		"no turn":       {"125", "turnline: this run takes no turn: memory stayed under the floor for an hour"},
		"did not start": {"126", "turnline: the command did not start: file not found"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWrapperFixture(t, config.Commands{Test: wrapperRanMarkerCommand}, map[string]string{
				"FAKE_CLI_WRAPPER_REFUSAL":      tc.line,
				"FAKE_CLI_WRAPPER_REFUSAL_CODE": tc.code,
			})

			outcome, err := (&TestStep{}).Execute(f.sctx)
			if err != nil {
				t.Fatalf("Execute() error = %v, want a park and not a failed run", err)
			}
			if !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %#v, want a park that no fix agent is sent to", outcome)
			}
			if got := outcome.ExitCode; got == 0 {
				t.Fatalf("exit code = %d, want the refusal's own, so approving is recorded as an override", got)
			}
			if len(f.agent.calls) != 0 {
				t.Fatalf("agent calls = %d, want none", len(f.agent.calls))
			}
			findings := liveCheckFindings(t, outcome)
			if len(findings.Items) != 1 {
				t.Fatalf("findings = %#v, want exactly the no-turn finding", findings.Items)
			}
			item := findings.Items[0]
			if item.ID != types.FindingIDTestCommandNoTurn || item.Action != types.ActionAskUser || item.Severity != types.FindingSeverityWarning || item.Category != types.FindingCategoryTestCommand {
				t.Fatalf("finding = %#v, want the ask-user no-turn warning of the test command", item)
			}
			if !strings.Contains(item.Description, tc.line) {
				t.Fatalf("finding = %q, want the wrapper's own last line %q", item.Description, tc.line)
			}
			if _, err := os.Stat(filepath.Join(f.sctx.WorkDir, wrapperRanMarker)); !os.IsNotExist(err) {
				t.Fatalf("the command ran although the wrapper refused it (stat error %v)", err)
			}
		})
	}
}

// Without the wrapper's own last line, 125 is the command's exit code like
// any other, so a test command that exits 125 is still a failing test command.
func TestTestStep_ExitCode125WithoutTheWrappersLineIsTheCommandsOwn(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "exit 125"}, nil)

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 125 {
		t.Fatalf("outcome = %#v, want a failing test command", outcome)
	}
	for _, item := range liveCheckFindings(t, outcome).Items {
		if item.ID == types.FindingIDTestCommandNoTurn {
			t.Fatalf("finding %#v reads the command's own exit code as a refusal", item)
		}
	}
}

// With no refusal_prefix configured the step cannot tell a refusal from a
// result, so it reads every exit code as the command's: today's reading.
func TestTestStep_WrapperWithNoRefusalPrefixReadsEveryExitCodeAsTheCommands(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: wrapperRanMarkerCommand}, map[string]string{
		"FAKE_CLI_WRAPPER_REFUSAL":      "turnline: this run takes no turn: memory stayed under the floor for an hour",
		"FAKE_CLI_WRAPPER_REFUSAL_CODE": "125",
	})
	f.sctx.Config.TestCommandWrapper.RefusalPrefix = ""

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.AutoFixable || outcome.ExitCode != 125 {
		t.Fatalf("outcome = %#v, want a failing test command", outcome)
	}
}

// A wrapper that cannot be started gave the command no turn either. It parks
// the same way, so a missing or replaced wrapper never fails a whole run.
func TestTestStep_WrapperThatCannotStartParksLikeARefusal(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: wrapperRanMarkerCommand}, nil)
	f.sctx.Config.TestCommandWrapper.Command[0] = filepath.Join(t.TempDir(), "no-such-wrapper.exe")

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a park and not a failed run", err)
	}
	findings := liveCheckFindings(t, outcome)
	if !outcome.NeedsApproval || outcome.AutoFixable || len(findings.Items) != 1 || findings.Items[0].ID != types.FindingIDTestCommandNoTurn {
		t.Fatalf("outcome = %#v findings = %#v, want the no-turn park", outcome, findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "could not be started") {
		t.Fatalf("finding = %q, want it to say the wrapper could not be started", findings.Items[0].Description)
	}
	if len(f.agent.calls) != 0 {
		t.Fatalf("agent calls = %d, want none", len(f.agent.calls))
	}
}

// On Windows the wrapper is started by the cooperative-command helper, which
// reports a start it could not make as its own last line with exit code 1.
// With a wrapper configured that line is about the wrapper, never a test
// result, and it is read only then.
func TestCommandWrapperStartFailure_ReadsTheWindowsHelpersLine(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "echo x"}, nil)
	output := "an earlier line\n" + shellenv.CooperativeStartFailurePrefix + "fork/exec C:\\bin\\cfo.exe: Access is denied.\n\n"

	got := commandWrapperStartFailure(f.sctx, "test", output, 1, nil)
	if !strings.Contains(got, "could not be started") || !strings.Contains(got, "Access is denied.") {
		t.Fatalf("commandWrapperStartFailure() = %q, want the helper's reason", got)
	}
	if got := commandWrapperStartFailure(f.sctx, "test", output, 3, nil); got != "" {
		t.Fatalf("exit code 3 read as a failed start: %q", got)
	}
	if got := commandWrapperStartFailure(f.sctx, "test", "tests failed\n", 1, nil); got != "" {
		t.Fatalf("a failing test command read as a failed start: %q", got)
	}
	if got := commandWrapperStartFailure(f.sctx, "lint", output, 1, nil); got != "" {
		t.Fatalf("lint has no wrapper, yet its output read as a failed wrapper start: %q", got)
	}
	f.sctx.Config.TestCommandWrapper = config.CommandWrapper{}
	if got := commandWrapperStartFailure(f.sctx, "test", output, 1, nil); got != "" {
		t.Fatalf("with no wrapper configured the line read as a failed wrapper start: %q", got)
	}
}

// Answering the park with fix runs the baseline again. There is nothing for a
// repair agent to repair, so none is asked.
func TestTestStep_WrapperRefusalFixAnswerRunsTheBaselineAgainWithNoRepairTurn(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "echo baseline-ran"}, nil)
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = `{"findings":[{"id":"` + types.FindingIDTestCommandNoTurn + `","severity":"warning","category":"test-command","description":"no turn","action":"ask-user"}],"summary":""}`

	outcome, err := (&TestStep{}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(f.calls(t)) != 1 {
		t.Fatalf("wrapper starts = %d, want the baseline run once more", len(f.calls(t)))
	}
	if len(f.agent.calls) != 1 || !strings.Contains(f.agent.calls[0].Prompt, "You are validating a code change by driving the product itself") {
		t.Fatalf("agent calls = %d, want only the live check: no repair turn for a command that never ran", len(f.agent.calls))
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the step complete once the baseline took its turn and passed", outcome)
	}
	if !strings.Contains(strings.Join(*f.logs, "\n"), "given no turn") {
		t.Fatalf("step log = %q, want it to say why no repair turn ran", strings.Join(*f.logs, "\n"))
	}
}

// The re-run on the base commit is a second whole test run, so it stands
// behind the wrapper too.
func TestTestStep_WrapperAlsoStartsTheBaseAttributionRun(t *testing.T) {
	f := newWrapperFixture(t, config.Commands{Test: "exit 3"}, nil)
	f.sctx.Config.Test.BaseAttribution = true

	if _, err := (&TestStep{}).Execute(f.sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("wrapper starts = %d, want 2: the run on the head and the run on the base", len(calls))
	}
	assertShellCall(t, calls[1][3:], "exit 3")
}
