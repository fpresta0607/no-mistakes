package main

import (
	"slices"
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
		"ubuntu-latest": 1,
		"macos-latest":  1,
		// core, two git-heavy parts, and three pipeline/steps parts.
		"windows-latest": 6,
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
		if command.name == "go" && slices.Equal(command.args, []string{"test", "-race", "-timeout=20m", "./..."}) {
			raceTests = append(raceTests, command)
		}
	}
	if len(raceTests) != 1 {
		t.Fatalf("Unix-only go test -race -timeout=20m ./... commands = %d, want 1; normalized commands: %#v", len(raceTests), commands)
	}
}

// The Unix legs ran on go test's implicit 10m per-package budget until
// internal/pipeline/steps, internal/daemon and internal/cli outran it on a slow
// macos-latest runner with no test hung (issue #73). The budget has to stay
// above that default and still fire before the job cap, so a real hang lands as
// a goroutine dump rather than a cancelled job.
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
	if jobCap := time.Duration(job.TimeoutMinutes) * time.Minute; budgets[0] >= jobCap {
		t.Errorf("Unix go test -timeout is %s and the job cap is %s; the Go timeout must fire first so a hang produces a goroutine dump", budgets[0], jobCap)
	}
}
