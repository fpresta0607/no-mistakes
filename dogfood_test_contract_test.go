package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
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
// path, and keeps package tests out of the live count. The journeys and hooks
// it cites must stay real, or the runbook sends the agent nowhere.
func TestDogfoodConfig_RunbookNamesTheIsolatedLaterPhaseLivePath(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	e2eFiles, err := filepath.Glob(filepath.Join(root, "internal", "e2e", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var e2eSource strings.Builder
	for _, path := range e2eFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		e2eSource.Write(data)
	}

	cfg, err := config.LoadRepo(root)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	runbook := cfg.Test.Instructions

	for _, want := range []string{
		"bash scripts/e2e.sh -tags=e2e",
		"temporary NM_HOME",
		"local bare upstream",
		"PR body",
		"AgentInvocations",
		"FAKEAGENT_GH_LOG",
		"readGHStubInvocations",
		"is live",
		"never return them as artifacts",
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
	for _, hook := range []string{"func (h *Harness) AgentInvocations(", `"FAKEAGENT_GH_LOG"`, "func readGHStubInvocations("} {
		if !strings.Contains(e2eSource.String(), hook) {
			t.Errorf("internal/e2e no longer has %s, which the runbook names", hook)
		}
	}
	journeys := regexp.MustCompile(`\bTest[A-Z]\w*`).FindAllString(runbook, -1)
	if len(journeys) == 0 {
		t.Error("runbook cites no e2e journey to copy")
	}
	for _, journey := range journeys {
		if !strings.Contains(e2eSource.String(), "func "+journey+"(t *testing.T)") {
			t.Errorf("runbook cites %s, which internal/e2e does not define", journey)
		}
	}
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
	want := []string{"test", "-race", "-timeout=20m", "./..."}
	for _, command := range commands {
		if command.name == "go" && slices.Equal(command.args, want) {
			return
		}
	}
	t.Fatalf("CI workflow must still run go %s on its Unix legs as the broad regression owner; Unix-only commands: %#v", strings.Join(want, " "), commands)
}
