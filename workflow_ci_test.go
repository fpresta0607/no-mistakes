package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCIWorkflowRunsTestsOnAllSupportedDesktopPlatforms(t *testing.T) {
	job := ciTestJob(t)
	got := make(map[string]int)
	for _, row := range job.Strategy.Matrix.Include {
		got[row["os"]]++
	}

	want := map[string]int{
		"ubuntu-latest":  1,
		"macos-latest":   1,
		"windows-latest": 17,
	}
	if len(got) != len(want) {
		t.Fatalf("test matrix operating systems = %v, want %v", got, want)
	}
	for osName, count := range want {
		if got[osName] != count {
			t.Errorf("test matrix rows for %q = %d, want %d", osName, got[osName], count)
		}
	}
}

func unixTestStep(t *testing.T) wfStep {
	t.Helper()
	for _, step := range ciTestJob(t).Steps {
		if exactRunnerOSCondition(step.If, "!=", "Windows") && step.Env["NM_CI_UNIX_STEPS_EXCLUDE"] != "" {
			return step
		}
	}
	t.Fatal("CI workflow has no Unix test step declaring NM_CI_UNIX_STEPS_EXCLUDE")
	return wfStep{}
}

// unixStepsExcludePattern reads the exclusion the Unix step declares so a guard
// resolves the package set the step actually runs instead of trusting the word
// ./... to sit somewhere in the script.
func unixStepsExcludePattern(t *testing.T, step wfStep) *regexp.Regexp {
	t.Helper()
	pattern := step.Env["NM_CI_UNIX_STEPS_EXCLUDE"]
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("NM_CI_UNIX_STEPS_EXCLUDE %q: %v", pattern, err)
	}
	return compiled
}

// unixRemainderPackages is the package set the Unix leg's go-list remainder
// runs: every package in the module the step's declared exclusion does not
// match.
func unixRemainderPackages(t *testing.T) []string {
	t.Helper()
	return filterPackages(goListPackages(t, "./..."), unixStepsExcludePattern(t, unixTestStep(t)), false)
}

func unixGoTestCommands(t *testing.T) []workflowCommand {
	t.Helper()
	var tests []workflowCommand
	for _, command := range workflowCommandsMatching(ciTestJob(t).Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	}) {
		if command.name == "go" && len(command.args) > 0 && command.args[0] == "test" {
			tests = append(tests, command)
		}
	}
	if len(tests) == 0 {
		t.Fatal("CI workflow has no Unix go test command")
	}
	return tests
}

func TestCIWorkflowUsesRaceTestsOnUnixRunners(t *testing.T) {
	for _, command := range unixGoTestCommands(t) {
		if !command.hasArg("-race") {
			t.Errorf("Unix go test command must run with -race: %#v", command.args)
		}
	}
}

// internal/pipeline/steps outgrew its per-package budget on macos-latest: it
// timed out at 1201s in the merge train while internal/daemon passed the same
// run at 1136s, every parked goroutine in the dump was a t.Parallel waiter and
// the three running tests were a second old or less. That is exhausted
// capacity, not a hang - the signature the testing-conventions skill records -
// so the suite runs its own tests in two complementary -run/-skip halves, the
// same shard split the Windows rows use, with the rest of the packages running
// beside them. Each half then has its own budget while no test, timeout or
// coverage is given up. The remainder must exclude the steps package or the
// halves would run it a third time.
func TestCIWorkflow_UnixShardsTheStepsPackageIntoTwoHalves(t *testing.T) {
	commands := unixGoTestCommands(t)
	var halves []namedTestShard
	var remainder *workflowCommand
	for index, command := range commands {
		if !slices.Equal(goTestPackagePatterns(command), []string{"./internal/pipeline/steps"}) {
			if remainder != nil {
				t.Fatalf("Unix go test runs a second non-steps package set: %#v and %#v", remainder.args, command.args)
			}
			remainder = &commands[index]
			continue
		}
		halves = append(halves, namedTestShard{
			name: strings.Join(command.args, " "),
			run:  namedTestPattern(t, command, "-run"),
			skip: namedTestPattern(t, command, "-skip"),
		})
	}
	if len(halves) != 2 {
		t.Fatalf("Unix go test runs internal/pipeline/steps %d times, want 2 complementary halves so each stays inside -timeout: %#v", len(halves), commands)
	}
	if remainder == nil {
		t.Fatalf("Unix go test must still run the rest of the suite beside the steps halves: %#v", commands)
	}
	if patterns := goTestPackagePatterns(*remainder); len(patterns) != 0 {
		t.Fatalf("the Unix command that runs the rest of the suite must run the computed go list package set, not the literal patterns %v", patterns)
	}
	covered := append(unixRemainderPackages(t), goListPackages(t, "./internal/pipeline/steps")...)
	slices.Sort(covered)
	if all := goListPackages(t, "./..."); !slices.Equal(covered, all) {
		t.Fatalf("the Unix remainder plus the two steps halves must cover go list ./... exactly once: covered %v, module %v", covered, all)
	}

	names := append(declaredTestNames(t, filepath.Join("internal", "pipeline", "steps")), "Test", "Test_Probe")
	for c := 'A'; c <= 'Z'; c++ {
		names = append(names, "Test"+string(c)+"Probe")
	}
	for c := '0'; c <= '9'; c++ {
		names = append(names, "Test"+string(c)+"Probe")
	}
	for _, name := range names {
		var selectedBy []string
		for _, half := range halves {
			if half.selects(name) {
				selectedBy = append(selectedBy, half.name)
			}
		}
		if len(selectedBy) != 1 {
			t.Errorf("%s is selected by %d Unix steps halves (%v), want exactly 1", name, len(selectedBy), selectedBy)
		}
	}
}

func namedTestPattern(t *testing.T, command workflowCommand, flag string) *regexp.Regexp {
	t.Helper()
	pattern, ok := commandFlagValue(command, flag)
	if !ok || pattern == "" {
		return nil
	}
	if strings.Contains(pattern, "/") {
		t.Fatalf("Unix steps shard %s %q contains /, which go test splits into subtest levels", flag, pattern)
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("Unix steps shard %s %q: %v", flag, pattern, err)
	}
	return compiled
}

func commandFlagValue(command workflowCommand, flag string) (string, bool) {
	for index, arg := range command.args {
		if arg == flag && index+1 < len(command.args) {
			return unquoteWorkflowArg(command.args[index+1]), true
		}
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			return unquoteWorkflowArg(value), true
		}
	}
	return "", false
}

// The Unix legs ran on go test's implicit 10m per-package budget until
// internal/pipeline/steps, internal/daemon and internal/cli outran it on a slow
// macos-latest runner with no test hung (issue #73). The budget has to stay
// above that default and still fire before the job cap for the package that
// starts last, so a real hang lands as a goroutine dump rather than a cancelled
// job.
func TestCIWorkflow_UnixPackageBudgetExceedsGoDefaultAndFiresBeforeJobCap(t *testing.T) {
	job := ciTestJob(t)
	commands := unixGoTestCommands(t)

	const goDefaultPackageBudget = 10 * time.Minute
	var budgets []time.Duration
	var halved []time.Duration
	for _, command := range commands {
		budget := goTestTimeout(t, command)
		budgets = append(budgets, budget)
		if budget <= goDefaultPackageBudget {
			t.Errorf("Unix go test -timeout is %s; it must exceed go test's %s default that the suite outgrew on macOS", budget, goDefaultPackageBudget)
		}
		if slices.Equal(goTestPackagePatterns(command), []string{"./internal/pipeline/steps"}) {
			halved = append(halved, budget)
		}
	}
	if len(halved) != 2 {
		t.Fatalf("Unix steps halves with a per-binary budget = %d, want 2; commands: %#v", len(halved), commands)
	}
	// The steps halves are the last commands to start: the A-Q half is 61% of
	// the package's macos-latest wall measured when it timed out at 1201s (the
	// same share the Windows rows show at 25m of 41m), so the second half starts
	// about 14 minutes into this step including checkout and setup, and its own
	// budget still fires inside the job cap.
	const latestMeasuredHalfStart = 14 * time.Minute
	secondHalfBudget := halved[len(halved)-1]
	if jobCap := time.Duration(job.TimeoutMinutes) * time.Minute; secondHalfBudget+latestMeasuredHalfStart >= jobCap {
		t.Errorf("Unix go test -timeout %s plus the %s latest measured steps-half start reaches the %s job cap; the Go timeout must fire first so a hang in the last half to start produces a goroutine dump", secondHalfBudget, latestMeasuredHalfStart, jobCap)
	}
}

// The Unix remainder's package set comes from go list, so a failed or empty
// listing must fail the step. An empty expansion runs go test against the
// repository root alone while both steps halves still report green, and
// internal/daemon, internal/cli, internal/git and the rest of the suite would
// never run on that leg.
func TestCIWorkflow_UnixStepFailsClosedWhenTheRemainderPackageSetIsEmpty(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable to execute the Unix test step")
	}
	step := unixTestStep(t)

	cases := []struct {
		name        string
		goList      string
		goListExit  string
		wantTests   []string
		wantFailure bool
	}{
		{
			name:        "empty listing",
			goListExit:  "0",
			wantFailure: true,
		},
		{
			name:        "failed listing",
			goListExit:  "1",
			wantFailure: true,
		},
		{
			name:   "resolved listing",
			goList: "github.com/kunchenguid/no-mistakes\ngithub.com/kunchenguid/no-mistakes/internal/daemon\ngithub.com/kunchenguid/no-mistakes/internal/pipeline/steps\n",
			wantTests: []string{
				"test -race -timeout=20m -p=2 github.com/kunchenguid/no-mistakes github.com/kunchenguid/no-mistakes/internal/daemon",
				"test -race -timeout=20m -run ^Test[A-Q] ./internal/pipeline/steps",
				"test -race -timeout=20m -skip ^Test[A-Q] ./internal/pipeline/steps",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tests, err, output := runUnixTestStep(t, bash, step, tc.goList, tc.goListExit)
			if tc.wantFailure {
				if err == nil {
					t.Fatalf("the Unix test step exited 0 after go list %q (exit %s); it must fail instead of running a narrowed suite\n%s", tc.goList, tc.goListExit, output)
				}
				if len(tests) != 0 {
					t.Fatalf("the Unix test step ran go %v after its package listing yielded nothing", tests)
				}
				return
			}
			if err != nil {
				t.Fatalf("the Unix test step failed with a resolved package listing: %v\n%s", err, output)
			}
			slices.Sort(tests)
			want := slices.Clone(tc.wantTests)
			slices.Sort(want)
			if !slices.Equal(tests, want) {
				t.Fatalf("Unix go test invocations = %v, want %v", tests, want)
			}
		})
	}
}

// runUnixTestStep executes the Unix test step's script with a fake go on PATH
// that reports the given listing for `go list` and records every other
// invocation, so the guard observes the command the step really runs.
func runUnixTestStep(t *testing.T, bash string, step wfStep, goList, goListExit string) ([]string, error, string) {
	t.Helper()
	dir := t.TempDir()
	shimDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("create fake go directory: %v", err)
	}
	logPath := filepath.Join(dir, "go.log")
	const shim = `#!/bin/sh
if [ "$1" = "list" ]; then
	printf '%s' "$FAKE_GO_LIST"
	exit "$FAKE_GO_LIST_EXIT"
fi
{
	printf '%s\n' "--"
	printf '%s\n' "$@"
} >> "$FAKE_GO_LOG"
`
	if err := os.WriteFile(filepath.Join(shimDir, "go"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	scriptPath := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(scriptPath, []byte(step.Run), 0o644); err != nil {
		t.Fatalf("write Unix test step script: %v", err)
	}

	if goListExit == "" {
		goListExit = "0"
	}
	cmd := exec.Command(bash, "-e", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GO_LIST="+goList,
		"FAKE_GO_LIST_EXIT="+goListExit,
		"FAKE_GO_LOG="+logPath,
	)
	for name, value := range step.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()

	data, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read recorded go invocations: %v", readErr)
	}
	var tests []string
	var args []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		switch {
		case line == "--":
			if len(args) > 0 {
				tests = append(tests, strings.Join(args, " "))
				args = nil
			}
		case line != "" || len(args) > 0:
			args = append(args, line)
		}
	}
	if len(args) > 0 {
		tests = append(tests, strings.Join(args, " "))
	}
	return tests, err, output.String()
}
