package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type launchAssertionFixtureStep struct{}

func (*launchAssertionFixtureStep) Name() types.StepName { return types.StepReview }
func (*launchAssertionFixtureStep) Execute(step *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	for _, purpose := range []string{"document", "review", "review-fix"} {
		if _, err := step.Agent.Run(step.Ctx, agent.RunOpts{Prompt: "model-free launch fixture", CWD: step.WorkDir, Purpose: purpose, Round: 1}); err != nil {
			return nil, err
		}
	}
	return &pipeline.StepOutcome{}, nil
}

func launchExpectation(head string) *launchassert.Expectation {
	profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	return &launchassert.Expectation{TrustedSHA: head, Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
}

func launchAssertionFixture(t *testing.T, factory StepFactory) (*RunManager, *paths.Paths, *db.DB, *db.Repo, string, string, string) {
	t.Helper()
	root := paths.WithRoot(t.TempDir())
	if err := root.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repository, head := setupTestGitRepo(t, root, database, "launch-assertion")
	gitCmd(t, root.RepoDir(repository.ID), "update-ref", "refs/heads/feature", head)
	directory := t.TempDir()
	marker := filepath.Join(directory, "fixture-entries.txt")
	binary := filepath.Join(directory, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuoteForTest(marker) + "\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if runtime.GOOS == "windows" {
		binary += ".bat"
		script = "@echo off\r\necho %*>>\"" + marker + "\"\r\necho {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}\r\necho {\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\r\n"
	}
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	global := fmt.Sprintf("agent: codex\nagent_path_override:\n  codex: %q\n  claude: %q\nagent_config:\n  codex: {model: fixture, effort: xhigh}\nagent_args_override:\n  codex: ['-c', 'service_tier=\"default\"']\n", binary, binary)
	if err := os.WriteFile(root.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, root, factory)
	t.Cleanup(manager.Shutdown)
	return manager, root, database, repository, head, marker, global
}

func TestLaunchAssertionRunStartRejectsEffectiveProfileMismatch(t *testing.T) {
	for _, test := range []struct {
		name        string
		replaceFrom string
		replaceWith string
		extra       string
	}{
		{name: "primary model", replaceFrom: "model: fixture", replaceWith: "model: different"},
		{name: "primary effort", replaceFrom: "effort: xhigh", replaceWith: "effort: low"},
		{name: "primary harness", replaceFrom: "agent: codex", replaceWith: "agent: claude"},
		{name: "reviewer model", extra: "review_agents:\n  reviewer: {agent: codex, model: different}\n"},
		{name: "reviewer effort", extra: "review_agents:\n  reviewer: {agent: codex, effort: low}\n"},
		{name: "reviewer harness", extra: "review_agents:\n  reviewer: {agent: claude}\n"},
		{name: "fixer model", extra: "review_agents:\n  fixer: {agent: codex, model: different}\n"},
		{name: "fixer effort", extra: "review_agents:\n  fixer: {agent: codex, effort: low}\n"},
		{name: "fixer harness", extra: "review_agents:\n  fixer: {agent: claude}\n"},
		{name: "native model", replaceFrom: "['-c',", replaceWith: "['--model', 'different', '-c',"},
		{name: "native effort", replaceFrom: "['-c',", replaceWith: "['-c', 'model_reasoning_effort=\"low\"', '-c',"},
		{name: "native tier", replaceFrom: "service_tier=\"default\"", replaceWith: "service_tier=\"priority\""},
		{name: "implicit tier", replaceFrom: "  codex: ['-c', 'service_tier=\"default\"']", replaceWith: "  codex: []"},
		{name: "opaque native profile", replaceFrom: "['-c',", replaceWith: "['--profile', 'hidden', '-c',"},
		{name: "unasserted later role", extra: "review_agents:\n  fixer_after_round: {agent: codex}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			step := &mockPassStep{name: types.StepReview}
			manager, root, database, repository, head, marker, global := launchAssertionFixture(t, func() []pipeline.Step { return []pipeline.Step{step} })
			if test.replaceFrom != "" {
				global = strings.Replace(global, test.replaceFrom, test.replaceWith, 1)
			}
			if err := os.WriteFile(root.ConfigFile(), []byte(global+test.extra), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
				RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "assert effective profile",
				LaunchNonce: "profile", ValidationGeneration: "generation", LaunchAssertion: launchExpectation(head),
			})
			if err == nil || !strings.Contains(err.Error(), "selection") && !strings.Contains(err.Error(), "profiles differ") {
				t.Fatalf("effective mismatch was not refused by the assertion: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) || step.execCnt.Load() != 0 {
				t.Fatal("mismatch entered the validation fixture")
			}
			runs, err := database.GetRunsByRepo(repository.ID)
			if err != nil || len(runs) != 0 {
				t.Fatalf("failed assertion created run or receipt custody: %+v %v", runs, err)
			}
		})
	}
}

func TestLaunchAssertionRunStartFetchesBeforeCheckingTrustedSource(t *testing.T) {
	for _, isUnreadableConfig := range []bool{false, true} {
		t.Run(fmt.Sprint(isUnreadableConfig), func(t *testing.T) {
			manager, root, _, repository, head, marker, _ := launchAssertionFixture(t, func() []pipeline.Step { return []pipeline.Step{&launchAssertionFixtureStep{}} })
			expected := launchExpectation(head)
			gitCmd(t, root.RepoDir(repository.ID), "update-ref", "refs/remotes/origin/main", head)
			source := "agent: codex\n"
			if isUnreadableConfig {
				source = "agent: [\n"
			}
			updated := commitDefaultBranchConfig(t, repository.WorkingPath, source)
			if isUnreadableConfig {
				expected.TrustedSHA = updated
			}
			_, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
				RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "assert fetched source",
				LaunchNonce: "source", ValidationGeneration: "generation", LaunchAssertion: expected,
			})
			if err == nil || !strings.Contains(err.Error(), "trusted") {
				t.Fatalf("trusted source failure was not closed: %v", err)
			}
			if tracked := gitOutput(t, root.RepoDir(repository.ID), "rev-parse", "refs/remotes/origin/main"); tracked != head {
				t.Fatalf("rejected assertion moved existing tracking custody from %s to %s", head, tracked)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("trusted source failure entered the fixture")
			}
		})
	}
}

func TestLaunchAssertionRunStartMatchingFixtureAndImmutableReplay(t *testing.T) {
	manager, root, database, repository, head, marker, global := launchAssertionFixture(t, func() []pipeline.Step { return []pipeline.Step{&launchAssertionFixtureStep{}} })
	expected := launchExpectation(head)
	expected.Profiles["reviewer"][0].Model = "reviewer-fixture"
	expected.Profiles["fixer"][0].Model = "fixer-fixture"
	global += "review_agents:\n  reviewer: {agent: codex, model: reviewer-fixture}\n  fixer: {agent: codex, model: fixer-fixture}\n"
	if err := os.WriteFile(root.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	params := &ipc.StartFreshRunParams{RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "matching source proof", LaunchNonce: "match", ValidationGeneration: "generation", LaunchAssertion: expected}
	receipt, err := manager.HandleStartFreshRun(context.Background(), params)
	if err != nil || receipt.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("matching launch receipt = %+v %v", receipt, err)
	}
	run := waitForRunTerminalState(t, database, receipt.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("matching fixture run = %+v", run)
	}
	entries, err := os.ReadFile(marker)
	if err != nil || len(strings.Split(strings.TrimSpace(string(entries)), "\n")) != 3 {
		t.Fatalf("fixture entries = %s, error=%v", entries, err)
	}
	for _, model := range []string{"fixture", "reviewer-fixture", "fixer-fixture"} {
		if !strings.Contains(string(entries), "-m "+model) {
			t.Fatalf("fixture did not receive effective model %s: %s", model, entries)
		}
	}
	if err := os.WriteFile(root.ConfigFile(), []byte(strings.ReplaceAll(global, "xhigh", "low")), 0o600); err != nil {
		t.Fatal(err)
	}
	replayed, err := manager.HandleStartFreshRun(context.Background(), params)
	if err != nil || replayed.RunID != receipt.RunID || replayed.Disposition != "reused" || replayed.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("immutable replay = %+v %v", replayed, err)
	}
	conflict := launchExpectation(head)
	params.LaunchAssertion = conflict
	if _, err := manager.HandleStartFreshRun(context.Background(), params); err == nil {
		t.Fatal("conflicting nonce expectation was accepted")
	}
	params.LaunchAssertion = nil
	if _, err := manager.HandleStartFreshRun(context.Background(), params); err == nil {
		t.Fatal("omitted expectation weakened an asserted nonce")
	}
	after, err := os.ReadFile(marker)
	if err != nil || string(after) != string(entries) {
		t.Fatal("nonce replay re-entered the agent fixture")
	}
}

func TestLaunchAssertionRunStartUsesResolvedRoleDefaults(t *testing.T) {
	manager, _, database, repository, head, marker, _ := launchAssertionFixture(t, func() []pipeline.Step { return []pipeline.Step{&launchAssertionFixtureStep{}} })
	expected := launchExpectation(head)
	receipt, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
		RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "assert resolved defaults",
		LaunchNonce: "defaults", ValidationGeneration: "generation", LaunchAssertion: expected,
	})
	if err != nil || receipt.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("default-role receipt = %+v %v", receipt, err)
	}
	if run := waitForRunTerminalState(t, database, receipt.RunID); run.Status != types.RunCompleted {
		t.Fatalf("default-role fixture did not complete: %+v", run)
	}
	entries, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(entries)), "\n")
	if len(lines) != 3 {
		t.Fatalf("default roles entered the fixture %d times, want 3", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, "-m fixture") || !strings.Contains(line, "model_reasoning_effort") || !strings.Contains(line, "service_tier") {
			t.Fatalf("default role lost its native selection: %s", line)
		}
	}
}

func TestLaunchAssertionRecoveryRejectsSourceOrProfileDrift(t *testing.T) {
	for _, change := range []string{"matching", "source", "primary", "reviewer", "fixer", "native", "missing proof"} {
		t.Run(change, func(t *testing.T) {
			manager, root, database, repository, head, marker, global := launchAssertionFixture(t, nil)
			expected := launchExpectation(head)
			proof, err := expected.Verify(head, expected.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", head, head, nil, "recovery", "generation", "digest", "", false, nil, expected, proof)
			if err != nil {
				t.Fatal(err)
			}
			if change == "missing proof" {
				// Only a tampered database can hold an asserted row without its proof.
				raw, err := sql.Open("sqlite", root.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(`DROP TRIGGER runs_launch_assertion_proof_immutable`); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`UPDATE runs SET launch_assertion_proof = NULL WHERE id = ?`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, root.RepoDir(repository.ID), "worktree", "add", "--detach", root.WorktreeDir(repository.ID, run.ID), head)
			parkRunAtReviewGate(t, database, run.ID, steps.AllSteps())
			switch change {
			case "source":
				commitDefaultBranchConfig(t, repository.WorkingPath, "agent: codex\n")
			case "primary":
				global = strings.Replace(global, "model: fixture", "model: different", 1)
			case "reviewer", "fixer":
				global += "review_agents:\n  " + change + ": {agent: codex, effort: low}\n"
			case "native":
				global = strings.Replace(global, "service_tier=\"default\"", "service_tier=\"priority\"", 1)
			}
			if err := os.WriteFile(root.ConfigFile(), []byte(global), 0o600); err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := manager.prepareRecoveredRun(context.Background(), stored)
			if change == "matching" {
				if err != nil {
					t.Fatalf("matching recovery refused: %v", err)
				}
				if err := plan.agent.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "launch") {
				t.Fatalf("recovery drift not rejected by launch proof: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("recovery validation entered the fixture")
			}
		})
	}
}

func TestLaunchAssertionRunStartProvesClaudeSelections(t *testing.T) {
	for _, test := range []struct {
		name        string
		agentConfig string
		agentArgs   string
		isMatching  bool
	}{
		{name: "matching", agentConfig: "{model: claude-fixture, effort: high}", agentArgs: "[]", isMatching: true},
		{name: "effort mismatch", agentConfig: "{model: claude-fixture, effort: low}", agentArgs: "[]"},
		{name: "native model mismatch", agentConfig: "{model: claude-fixture, effort: high}", agentArgs: "['--model', 'other']"},
		{name: "implicit effort", agentConfig: "{model: claude-fixture}", agentArgs: "[]"},
		{name: "unprovable native argument", agentConfig: "{model: claude-fixture, effort: high}", agentArgs: "['--fallback-model', 'other']"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, root, database, repository, head, _, _ := launchAssertionFixture(t, func() []pipeline.Step { return []pipeline.Step{&launchAssertionFixtureStep{}} })
			directory := t.TempDir()
			marker := filepath.Join(directory, "claude-entries.txt")
			binary := filepath.Join(directory, "claude")
			result := `{"type":"result","subtype":"success","is_error":false,"result":"ok"}`
			script := "#!/bin/sh\nprintf '%s\n' \"$*\" >> " + shellQuoteForTest(marker) + "\nprintf '%s\n' '" + result + "'\n"
			if runtime.GOOS == "windows" {
				binary += ".bat"
				script = "@echo off\r\necho %*>>\"" + marker + "\"\r\necho " + result + "\r\n"
			}
			if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			global := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %q\nagent_config:\n  claude: %s\nagent_args_override:\n  claude: %s\n", binary, test.agentConfig, test.agentArgs)
			if err := os.WriteFile(root.ConfigFile(), []byte(global), 0o600); err != nil {
				t.Fatal(err)
			}
			profile := agentcfg.Selection{Harness: types.AgentClaude, Model: "claude-fixture", Effort: agentcfg.EffortHigh}
			expected := &launchassert.Expectation{TrustedSHA: head, Profiles: map[string][]agentcfg.Selection{
				"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
			}}

			receipt, err := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
				RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "assert Claude selection",
				LaunchNonce: "claude", ValidationGeneration: "generation", LaunchAssertion: expected,
			})

			if !test.isMatching {
				if err == nil || !strings.Contains(err.Error(), "selection") && !strings.Contains(err.Error(), "profiles differ") {
					t.Fatalf("Claude mismatch was not refused by the assertion: %v", err)
				}
				if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
					t.Fatal("Claude mismatch entered the agent fixture")
				}
				if runs, err := database.GetRunsByRepo(repository.ID); err != nil || len(runs) != 0 {
					t.Fatalf("refused Claude assertion created a run: %+v %v", runs, err)
				}
				return
			}
			if err != nil || receipt.LaunchAssertionProof.Check(expected) != nil {
				t.Fatalf("matching Claude receipt = %+v %v", receipt, err)
			}
			if run := waitForRunTerminalState(t, database, receipt.RunID); run.Status != types.RunCompleted {
				t.Fatalf("matching Claude fixture run = %+v", run)
			}
			entries, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(entries)), "\n")
			if len(lines) != 3 {
				t.Fatalf("Claude fixture entered %d times, want 3: %s", len(lines), entries)
			}
			for _, line := range lines {
				if !strings.Contains(line, "--model claude-fixture") || !strings.Contains(line, "--effort high") {
					t.Fatalf("Claude fixture did not receive the proved selection: %s", line)
				}
			}
		})
	}
}
