package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var (
	selectedClaude = agentcfg.Selection{Harness: types.AgentClaude, Model: "claude-fixture", Effort: agentcfg.EffortHigh}
	selectedCodex  = agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
)

// launchSelection is an assertion that applies its one chain to every role.
func launchSelection(head string, chain ...agentcfg.Selection) *launchassert.Expectation {
	return &launchassert.Expectation{TrustedSHA: head, Apply: true, Profiles: map[string][]agentcfg.Selection{
		"primary": chain, "reviewer": chain, "fixer": chain,
	}}
}

// selectionHarness writes a stand-in harness that records its arguments in a
// marker file and answers as the named harness does, or exits 1 when failing.
func selectionHarness(t *testing.T, harness types.AgentName, isFailing bool) (binary, marker string) {
	t.Helper()
	directory := t.TempDir()
	marker = filepath.Join(directory, string(harness)+"-entries.txt")
	binary = filepath.Join(directory, string(harness))
	replies := []string{`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`}
	if harness == types.AgentCodex {
		replies = []string{`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`}
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuoteForTest(marker) + "\n"
	if runtime.GOOS == "windows" {
		binary += ".bat"
		script = "@echo off\r\necho %*>>\"" + marker + "\"\r\n"
	}
	switch {
	case isFailing && runtime.GOOS == "windows":
		script += "exit /b 1\r\n"
	case isFailing:
		script += "exit 1\n"
	case runtime.GOOS == "windows":
		for _, reply := range replies {
			script += "echo " + reply + "\r\n"
		}
	default:
		for _, reply := range replies {
			script += "printf '%s\\n' '" + reply + "'\n"
		}
	}
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, marker
}

// selectionFixture is a daemon whose machine configuration names agent as its
// chain, pins both review roles to Codex and gives each harness the arguments
// a gate passes, with a stand-in for each harness. Its runs have the one
// fixture step, which calls an agent for three duties, or with isRecovery the
// daemon's own steps, which a recovered run is matched against.
func selectionFixture(t *testing.T, agent string, isClaudeFailing, isRecovery bool) (manager *RunManager, root *paths.Paths, database *db.DB, repository *db.Repo, head, claudeMarker, codexMarker string) {
	t.Helper()
	var factory StepFactory
	if !isRecovery {
		factory = func() []pipeline.Step { return []pipeline.Step{&launchAssertionFixtureStep{}} }
	}
	manager, root, database, repository, head, _, _ = launchAssertionFixture(t, factory)
	claude, claudeMarker := selectionHarness(t, types.AgentClaude, isClaudeFailing)
	codex, codexMarker := selectionHarness(t, types.AgentCodex, false)
	global := fmt.Sprintf(`agent: %s
agent_path_override:
  claude: %q
  codex: %q
agent_config:
  codex: {model: operator-codex, effort: low}
  claude: {model: operator-claude, effort: low}
review_agents:
  reviewer: {agent: codex}
  fixer: {agent: codex}
agent_args_override:
  codex: ['-c', 'service_tier="default"', '--ignore-user-config', '--disable', 'plugins', '--disable', 'apps', '-c', 'personality="pragmatic"', '-c', 'model_auto_compact_token_limit_scope="total"', '-c', 'features.multi_agent=true', '-c', 'project_doc_max_bytes=65536']
  claude: ['--strict-mcp-config']
`, agent, claude, codex)
	if err := os.WriteFile(root.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	return manager, root, database, repository, head, claudeMarker, codexMarker
}

func markerLines(t *testing.T, marker string) []string {
	t.Helper()
	entries, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(entries)), "\n")
}

func TestLaunchSelectionRunsItsOwnHarnessWhateverTheMachineChain(t *testing.T) {
	manager, _, database, repository, head, claudeMarker, codexMarker := selectionFixture(t, "codex", false, false)
	expected := launchSelection(head, selectedClaude)

	receipt, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
		RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "run on the caller's harness",
		LaunchNonce: "claude-task", ValidationGeneration: "generation", LaunchAssertion: expected,
	})

	if err != nil || receipt.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("selection receipt = %+v %v", receipt, err)
	}
	if run := waitForRunTerminalState(t, database, receipt.RunID); run.Status != types.RunCompleted {
		t.Fatalf("selected run = %+v", run)
	}
	lines := markerLines(t, claudeMarker)
	if len(lines) != 3 {
		t.Fatalf("Claude ran %d of the 3 duties: %v", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "--model claude-fixture") || !strings.Contains(line, "--effort high") || !strings.Contains(line, "--strict-mcp-config") {
			t.Fatalf("Claude did not get the selected model and effort with the operator's arguments: %s", line)
		}
	}
	if started := markerLines(t, codexMarker); started != nil {
		t.Fatalf("the machine chain's Codex started for a run that selected Claude: %v", started)
	}
}

func TestLaunchSelectionsOnTwoHarnessesShareOneDaemon(t *testing.T) {
	manager, root, database, repository, head, claudeMarker, codexMarker := selectionFixture(t, "codex", false, false)
	gitCmd(t, root.RepoDir(repository.ID), "update-ref", "refs/heads/second", head)
	launches := []struct {
		branch   string
		expected *launchassert.Expectation
	}{{"feature", launchSelection(head, selectedClaude)}, {"second", launchSelection(head, selectedCodex)}}

	var receipts []ipc.LaunchReceipt
	for _, launch := range launches {
		receipt, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
			RepoID: repository.ID, Branch: launch.branch, HeadSHA: head, Intent: "run on the caller's harness",
			LaunchNonce: launch.branch, ValidationGeneration: "generation", LaunchAssertion: launch.expected,
		})
		if err != nil || receipt.LaunchAssertionProof.Check(launch.expected) != nil {
			t.Fatalf("%s receipt = %+v %v", launch.branch, receipt, err)
		}
		receipts = append(receipts, receipt)
	}

	for _, receipt := range receipts {
		if run := waitForRunTerminalState(t, database, receipt.RunID); run.Status != types.RunCompleted {
			t.Fatalf("run on %s = %+v", receipt.Branch, run)
		}
	}
	for _, line := range markerLines(t, claudeMarker) {
		if !strings.Contains(line, "--model claude-fixture") {
			t.Fatalf("Claude ran with another run's selection: %s", line)
		}
	}
	for _, line := range markerLines(t, codexMarker) {
		if !strings.Contains(line, "-m fixture") || !strings.Contains(line, "xhigh") {
			t.Fatalf("Codex ran with another run's selection: %s", line)
		}
	}
	if claude, codex := len(markerLines(t, claudeMarker)), len(markerLines(t, codexMarker)); claude != 3 || codex != 3 {
		t.Fatalf("each run has 3 duties on its own harness, got Claude %d and Codex %d", claude, codex)
	}
}

func TestLaunchSelectionStartsNoHarnessItDidNotName(t *testing.T) {
	for _, test := range []struct {
		name       string
		chain      []agentcfg.Selection
		wantStatus types.RunStatus
		wantCodex  bool
	}{
		{"no fallback named", []agentcfg.Selection{selectedClaude}, types.RunFailed, false},
		{"fallback named", []agentcfg.Selection{selectedClaude, selectedCodex}, types.RunCompleted, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The machine chain names Codex after Claude, and Claude fails.
			manager, _, database, repository, head, claudeMarker, codexMarker := selectionFixture(t, "[claude, codex]", true, false)
			expected := launchSelection(head, test.chain...)

			receipt, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
				RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "fall back only where named",
				LaunchNonce: "fallback", ValidationGeneration: "generation", LaunchAssertion: expected,
			})

			if err != nil || receipt.LaunchAssertionProof.Check(expected) != nil {
				t.Fatalf("selection receipt = %+v %v", receipt, err)
			}
			if run := waitForRunTerminalState(t, database, receipt.RunID); run.Status != test.wantStatus {
				t.Fatalf("run = %+v, want %s", run, test.wantStatus)
			}
			if len(markerLines(t, claudeMarker)) == 0 {
				t.Fatal("the selected harness never started")
			}
			if started := markerLines(t, codexMarker); (started != nil) != test.wantCodex {
				t.Fatalf("Codex started = %v, want %v: %v", started != nil, test.wantCodex, started)
			}
		})
	}
}

func TestLaunchSelectionRecoversOnItsOwnHarness(t *testing.T) {
	manager, root, database, repository, head, claudeMarker, codexMarker := selectionFixture(t, "codex", false, true)
	expected := launchSelection(head, selectedClaude)
	proof, err := expected.Verify(head, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", head, head, nil, "recovery", "generation", "digest", "", false, nil, expected, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root.RepoDir(repository.ID), "worktree", "add", "--detach", root.WorktreeDir(repository.ID, run.ID), head)
	parkRunAtReviewGate(t, database, run.ID, steps.AllSteps())
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := manager.prepareRecoveredRun(context.Background(), stored)

	if err != nil {
		t.Fatalf("a selected run did not recover on its own harness: %v", err)
	}
	if err := plan.agent.Close(); err != nil {
		t.Fatal(err)
	}
	if markerLines(t, claudeMarker) != nil || markerLines(t, codexMarker) != nil {
		t.Fatal("recovery validation started a harness")
	}
}

func TestLaunchSelectionRefusesAPiRunProfile(t *testing.T) {
	manager, _, database, repository, head, claudeMarker, codexMarker := selectionFixture(t, "codex", false, false)

	_, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
		RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "two selections",
		LaunchNonce: "both", ValidationGeneration: "generation", LaunchAssertion: launchSelection(head, selectedClaude),
		PiProfile: &agentcfg.PiProfile{Model: "anthropic/claude-fixture", Effort: agentcfg.EffortHigh},
	})

	if err == nil || !strings.Contains(err.Error(), "launch selection") {
		t.Fatalf("a launch selection with a Pi run profile was not refused as one: %v", err)
	}
	if runs, err := database.GetRunsByRepo(repository.ID); err != nil || len(runs) != 0 {
		t.Fatalf("the refused launch created a run: %+v %v", runs, err)
	}
	if markerLines(t, claudeMarker) != nil || markerLines(t, codexMarker) != nil {
		t.Fatal("the refused launch started a harness")
	}
}
