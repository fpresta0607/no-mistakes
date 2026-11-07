package main

import (
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
	var excludedSteps bool
	for _, arg := range remainder.args {
		if strings.Contains(unquoteWorkflowArg(arg), "/internal/pipeline/steps$") {
			excludedSteps = true
		}
	}
	if !excludedSteps {
		t.Fatalf("the Unix command that runs the rest of the suite must exclude internal/pipeline/steps, got %#v", remainder.args)
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
