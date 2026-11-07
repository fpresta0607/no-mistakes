package main

import (
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

func TestCIWorkflowUsesRaceTestsOnUnixRunners(t *testing.T) {
	job := ciTestJob(t)
	commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	})

	var raceTests []workflowCommand
	for _, command := range commands {
		if command.name == "go" && slices.Equal(command.args, []string{"test", "-race", "-timeout=20m", "-p=1", "./..."}) {
			raceTests = append(raceTests, command)
		}
	}
	if len(raceTests) != 1 {
		t.Fatalf("Unix-only go test -race -timeout=20m -p=1 ./... commands = %d, want 1; normalized commands: %#v", len(raceTests), commands)
	}
}

// internal/pipeline/steps alone needs most of the Unix package budget: it
// passed at 1112s on macos-latest and timed out at 1201s once the other heavy
// packages ran beside it, with no test hung. Every parked goroutine in that
// dump was a t.Parallel waiter and only three tests were running, a second or
// less old - the exhausted-capacity signature the testing-conventions skill
// records, not a hang. Letting the packages take the runner one at a time is
// the only Unix-side lever that neither raises the budget, skips a test nor
// widens a timeout, so keep -p 1 pinned here: without it pipeline/steps goes
// back to competing for three cores with internal/pipeline, internal/gate and
// the rest of the suite.
func TestCIWorkflow_UnixRunsOnePackageAtATime(t *testing.T) {
	job := ciTestJob(t)
	commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	})

	var tests []workflowCommand
	for _, command := range commands {
		if command.name == "go" && len(command.args) > 0 && command.args[0] == "test" {
			tests = append(tests, command)
		}
	}
	if len(tests) != 1 {
		t.Fatalf("Unix-only go test commands = %d, want 1; normalized commands: %#v", len(tests), commands)
	}
	packageParallelism, ok := commandFlagValue(tests[0], "-p")
	if !ok || packageParallelism != "1" {
		t.Fatalf("Unix go test package parallelism = %q (found %t), want 1 so the heavy package gets the runner instead of a share of it; args: %#v", packageParallelism, ok, tests[0].args)
	}
}

func commandFlagValue(command workflowCommand, flag string) (string, bool) {
	for index, arg := range command.args {
		if arg == flag && index+1 < len(command.args) {
			return command.args[index+1], true
		}
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			return value, true
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
	commands := workflowCommandsMatching(job.Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	})

	var budgets []time.Duration
	for _, command := range commands {
		if command.name == "go" && len(command.args) > 0 && command.args[0] == "test" {
			budgets = append(budgets, goTestTimeout(t, command))
		}
	}
	if len(budgets) != 1 {
		t.Fatalf("Unix-only go test commands = %d, want 1; normalized commands: %#v", len(budgets), commands)
	}
	const goDefaultPackageBudget = 10 * time.Minute
	if budgets[0] <= goDefaultPackageBudget {
		t.Errorf("Unix go test -timeout is %s; it must exceed go test's %s default that the suite outgrew on macOS", budgets[0], goDefaultPackageBudget)
	}
	// internal/pipeline/steps started this far into the Unix test step on the
	// slow PR #44 macos-latest run 41793873984.
	const latestMeasuredPackageStart = 12 * time.Minute
	if jobCap := time.Duration(job.TimeoutMinutes) * time.Minute; budgets[0]+latestMeasuredPackageStart >= jobCap {
		t.Errorf("Unix go test -timeout %s plus the %s latest measured package start reaches the %s job cap; the Go timeout must fire first so a hang in the last package to start produces a goroutine dump", budgets[0], latestMeasuredPackageStart, jobCap)
	}
}
