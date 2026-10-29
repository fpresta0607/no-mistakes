package main

import (
	"strings"
	"testing"
)

// pipeline/steps is one package whose ~1000 top-level tests cost about
// 1600-1850s of test time on a 4-core Windows runner, more than one 15m go test
// budget. Its shard runs as three parts of the package's top-level tests, each
// computed by tools/testshard from the real `go test -list '^Test'` output.
// These tests pin that split so a later edit cannot quietly collapse it back into
// one job, skip tests, shorten the run or lower parallelism.
func TestCIWorkflow_WindowsStepsShardRunsThreePartsOfTheTestList(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	parts := map[string]struct{}{}
	rows := 0
	for _, row := range job.Strategy.Matrix.Include {
		if row["os"] != "windows-latest" || row["shard"] != "steps" {
			continue
		}
		rows++
		part := row["steps_part"]
		if part == "" {
			t.Fatalf("windows steps row %v is missing steps_part", row)
		}
		if _, done := parts[part]; done {
			t.Fatalf("windows steps row %v repeats steps_part %q", row, part)
		}
		parts[part] = struct{}{}
		if want := "test (windows-steps " + part + "/3)"; row["name"] != want {
			t.Errorf("windows steps row name = %q, want %q", row["name"], want)
		}
	}
	if rows != 3 {
		t.Fatalf("windows steps shard rows = %d, want 3 parts so no job runs the whole package", rows)
	}
	for _, want := range []string{"1", "2", "3"} {
		if _, ok := parts[want]; !ok {
			t.Errorf("windows steps shard has no part %q", want)
		}
	}

	steps := 0
	for _, step := range job.Steps {
		if matrixShardCondition(step.If) != "steps" {
			continue
		}
		steps++
		if step.Shell != "pwsh" {
			t.Errorf("pipeline/steps part step %q must run with pwsh to capture the tool's pattern, got shell %q", step.Name, step.Shell)
		}
		if !strings.Contains(step.Run, "go run ./tools/testshard -parts 3 -part ${{ matrix.steps_part }}") {
			t.Errorf("pipeline/steps part step %q must compute its pattern with tools/testshard and the matrix part, got:\n%s", step.Name, step.Run)
		}
		if !strings.Contains(step.Run, "go test -v -timeout=15m -run $run ./internal/pipeline/steps/...") {
			t.Errorf("pipeline/steps part step %q must keep -timeout=15m and run the package with the computed -run pattern, got:\n%s", step.Name, step.Run)
		}
	}
	// One step, parameterized by the matrix part, so the three jobs cannot drift
	// into three hand-edited command lines.
	if steps != 1 {
		t.Fatalf("pipeline/steps part steps = %d, want one step driving every matrix part", steps)
	}

	// The split must not buy its wall time by dropping work.
	for _, command := range windowsGoTestCommands(t) {
		rendered := strings.Join(command.args, " ")
		for _, forbidden := range []string{"-skip", "-short", "-parallel", "-p="} {
			for _, arg := range command.args {
				if arg == forbidden || strings.HasPrefix(arg, forbidden+"=") {
					t.Errorf("Windows test command %q uses %s; the split keeps every test and today's parallelism", rendered, arg)
				}
			}
		}
	}

	// The partition is proven on every run by the cheap Linux check job, before
	// any Windows job spends a slot.
	found := false
	for _, step := range loadCIWorkflowDoc(t).Jobs["check"].Steps {
		if strings.Contains(step.Run, "go run ./tools/testshard -verify") {
			found = true
		}
	}
	if !found {
		t.Error("the check job must run `go run ./tools/testshard -verify` so the parts are proven to cover every listed test")
	}
}

// TestCIWorkflow_StepsPartsKeepTheJobAndGoTimeouts guards the two budgets the
// split must not touch: the Go timeout stays 15m so a real hang dumps goroutines,
// and the job cap stays 40 minutes so a wedged runner cannot burn the day.
func TestCIWorkflow_StepsPartsKeepTheJobAndGoTimeouts(t *testing.T) {
	t.Parallel()

	job := ciTestJob(t)
	if job.TimeoutMinutes != 40 {
		t.Fatalf("test job timeout-minutes = %d, want 40", job.TimeoutMinutes)
	}
	for _, step := range job.Steps {
		if matrixShardCondition(step.If) != "steps" {
			continue
		}
		if strings.Contains(step.Run, "-timeout=15m") {
			continue
		}
		t.Errorf("pipeline/steps part step %q must keep go test -timeout=15m, got:\n%s", step.Name, step.Run)
	}
}
