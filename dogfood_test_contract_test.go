package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// Local Test is targeted validation of the requested intent. This repository
// must not dogfood a complete-suite commands.test override (historically
// go test -race ./...), which burned multi-hour local walks while remote CI
// already owns the race-enabled full suite.
func TestDogfoodConfig_NoBroadLocalTestCommand(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRepo(root)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if got := strings.TrimSpace(cfg.Commands.Test); got != "" {
		t.Fatalf("dogfood commands.test = %q, want empty so local Test stays agent-targeted; put broad regression in remote CI", got)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatalf("read .no-mistakes.yaml: %v", err)
	}
	content := string(raw)
	for _, forbid := range []string{
		`test: "go test -race ./..."`,
		`test: 'go test -race ./...'`,
		`test: go test -race ./...`,
	} {
		if strings.Contains(content, forbid) {
			t.Fatalf(".no-mistakes.yaml still configures broad local Test %q", forbid)
		}
	}
}

// PR drafting and PR body rendering run only in the PR phase, the built CLI
// has no standalone preview of them, and the gate-step boundary keeps the Test
// turn out of the outer run's PR phase, so without a named path every such
// scenario ended untested (issue #55). The trusted runbook names the e2e
// harness, which builds this worktree and runs every phase in a disposable
// home against a local upstream and stub forge CLIs, as the supported live
// path, and keeps package tests out of the live count.
//
// test.instructions is an owned text contract: the Test prompt carries it
// verbatim, so the rules issue #55 decided are checked as that text, whitespace
// aside, which says nothing about how an agent follows them. What the runbook
// cites is executed instead: the Go test runner has to list every journey, and
// TestRunbookHooksKeepTheirCitedNames has to pass against this runbook.
func TestDogfoodConfig_RunbookNamesTheIsolatedLaterPhaseLivePath(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRepo(root)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	runbook := strings.Join(strings.Fields(cfg.Test.Instructions), " ")

	for _, want := range []string{
		"TMPDIR=/tmp bash scripts/e2e.sh -tags=e2e",
		"-overlay=",
		"temporary NM_HOME",
		"local bare upstream",
		"PR title and body rendering",
		"is live",
		"Test scratch directory",
		"Never return them as artifacts",
		"never the outer run",
		"not live",
	} {
		if !strings.Contains(runbook, want) {
			t.Errorf("trusted test.instructions is missing %q:\n%s", want, runbook)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "scripts", "e2e.sh")); err != nil {
		t.Errorf("runbook names a missing runner: %v", err)
	}

	// The harness never removes the binaries it builds into the temp directory,
	// and TestMain reaps the daemon inventory it finds there, so this run gets
	// its own.
	temp := t.TempDir()
	goTestE2E := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", slices.Concat([]string{"test", "-tags=e2e"}, args, []string{"./internal/e2e/"})...)
		winproc.Harden(cmd)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "TMPDIR="+temp, "TMP="+temp, "TEMP="+temp)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test -tags=e2e %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	const hooksTest = "TestRunbookHooksKeepTheirCitedNames"
	journeys := regexp.MustCompile(`\bTest[A-Z]\w*`).FindAllString(runbook, -1)
	if len(journeys) == 0 {
		t.Error("runbook cites no e2e journey to copy")
	}
	required := append(journeys, hooksTest)
	listed := strings.Fields(goTestE2E("-list", "^("+strings.Join(required, "|")+")$"))
	for _, name := range required {
		if !slices.Contains(listed, name) {
			t.Errorf("go test -tags=e2e -list does not find %s", name)
		}
	}
	goTestE2E("-count=1", "-run", "^"+hooksTest+"$")
}

// commands.test contract ownership lives in repo-config.md: targeted local
// validation, not CI-parity complete-suite configuration, and no brittle
// shell-heuristics claiming to detect "broad" commands.
func TestRepoConfigDocs_CommandsTestIsTargetedContract(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("docs", "src", "content", "docs", "reference", "repo-config.md"))
	if err != nil {
		t.Fatalf("read repo-config.md: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"### commands.test",
		"targeted validation",
		"not a CI-parity repository-wide regression command",
		"Broad regression belongs in remote CI",
		"does not guess whether an arbitrary shell string is \"too broad\"",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("repo-config.md commands.test contract missing %q", want)
		}
	}
}

// Remote CI must keep the complete race-enabled Go suite as the broad
// regression owner after local Test stops duplicating it.
func TestCIWorkflow_RetainsFullRaceSuiteAsBroadRegressionOwner(t *testing.T) {
	commands := workflowCommandsMatching(ciTestJob(t).Steps, func(step wfStep) bool {
		return exactRunnerOSCondition(step.If, "!=", "Windows")
	})
	want := []string{"test", "-race", "-timeout=20m", "-p=1", "./..."}
	for _, command := range commands {
		if command.name == "go" && slices.Equal(command.args, want) {
			return
		}
	}
	t.Fatalf("CI workflow must still run go %s on its Unix legs as the broad regression owner; Unix-only commands: %#v", strings.Join(want, " "), commands)
}
