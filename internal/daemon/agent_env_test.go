package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// agentEnvCutoff has no digit before the redirection of the Windows capture
// script, where a trailing digit would read as a file handle.
const agentEnvCutoff = "2026-11-01T12:30:00Z"

// writeEnvCapturingPiAgent writes a fake pi binary that records whether
// UV_EXCLUDE_NEWER is set in its environment, and to what, into capturePath.
func writeEnvCapturingPiAgent(t *testing.T, dir, capturePath string) string {
	t.Helper()
	const response = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`
	bin := filepath.Join(dir, "pi")
	script := "#!/bin/sh\ncat >/dev/null\nif [ -n \"${UV_EXCLUDE_NEWER+x}\" ]; then printf 'set:%s' \"$UV_EXCLUDE_NEWER\" >" + shellQuoteForTest(capturePath) + "; else printf unset >" + shellQuoteForTest(capturePath) + "; fi\nprintf '%s\\n' '" + response + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\nmore >nul\r\nif defined UV_EXCLUDE_NEWER (echo set:%UV_EXCLUDE_NEWER% >\"" + capturePath + "\") else (echo unset >\"" + capturePath + "\")\r\necho " + response + "\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readCapturedAgentEnv(t *testing.T, capturePath string) string {
	t.Helper()
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("the agent never ran: %v", err)
	}
	return strings.TrimSpace(string(captured))
}

// useEnvCapturingPiAgent points the daemon root's agent at the fake pi and
// clears any ambient UV_EXCLUDE_NEWER so the capture reflects the run alone.
func useEnvCapturingPiAgent(t *testing.T, p *paths.Paths) string {
	t.Helper()
	t.Setenv("UV_EXCLUDE_NEWER", "ambient")
	if err := os.Unsetenv("UV_EXCLUDE_NEWER"); err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(t.TempDir(), "agent-env.txt")
	piBin := writeEnvCapturingPiAgent(t, t.TempDir(), capturePath)
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\nagent_path_override:\n  pi: "+piBin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return capturePath
}

func TestRunEnvironmentAddsTheRunsPairsToTheForgeOverlay(t *testing.T) {
	forge := &forgecontext.Context{Environment: runenv.Overlay{
		Set:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
		Unset: []string{"GH_TOKEN"},
	}}
	pairs := map[string]string{"UV_EXCLUDE_NEWER": "7 days", "npm_config_before": agentEnvCutoff}
	tests := []struct {
		name      string
		forge     *forgecontext.Context
		run       *db.Run
		wantSet   map[string]string
		wantUnset []string
	}{
		{
			name:      "forge overlay and pairs",
			forge:     forge,
			run:       &db.Run{AgentEnv: pairs},
			wantSet:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal", "UV_EXCLUDE_NEWER": "7 days", "npm_config_before": agentEnvCutoff},
			wantUnset: []string{"GH_TOKEN"},
		},
		{
			name:    "nil forge context and pairs",
			forge:   nil,
			run:     &db.Run{AgentEnv: pairs},
			wantSet: pairs,
		},
		{
			name:      "no pairs is the forge overlay",
			forge:     forge,
			run:       &db.Run{},
			wantSet:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
			wantUnset: []string{"GH_TOKEN"},
		},
		{
			name:  "no pairs and no forge changes nothing",
			forge: nil,
			run:   &db.Run{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runEnvironment(tc.forge, tc.run)

			if len(got.Set) != len(tc.wantSet) || (len(tc.wantSet) > 0 && !reflect.DeepEqual(got.Set, tc.wantSet)) {
				t.Errorf("Set = %v, want %v", got.Set, tc.wantSet)
			}
			if len(got.Unset) != len(tc.wantUnset) || (len(tc.wantUnset) > 0 && !reflect.DeepEqual(got.Unset, tc.wantUnset)) {
				t.Errorf("Unset = %v, want %v", got.Unset, tc.wantUnset)
			}
		})
	}
	if _, leaked := forge.Environment.Set["UV_EXCLUDE_NEWER"]; leaked || len(forge.Environment.Set) != 1 {
		t.Fatalf("the shared forge context was modified: %v", forge.Environment.Set)
	}
}

func TestPrepareRecoveredRunBuildsTheAgentEnvironmentFromTheStoredPairs(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"with_pairs", map[string]string{"UV_EXCLUDE_NEWER": agentEnvCutoff}, "set:" + agentEnvCutoff},
		{"without_pairs", nil, "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, database := newRefreshRunFixture(t)
			capturePath := useEnvCapturingPiAgent(t, p)
			repo, head := setupTestGitRepo(t, p, database, "agent-env-recovery")
			run, err := database.InsertLaunchedRun(repo.ID, "feature", head, head, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, tc.env, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", p.WorktreeDir(repo.ID, run.ID), head)
			parkRunAtReviewGate(t, database, run.ID, steps.AllSteps())
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manager := NewRunManager(database, p, nil)

			plan, err := manager.prepareRecoveredRun(context.Background(), stored)
			if err != nil {
				t.Fatalf("recover the parked run: %v", err)
			}
			defer plan.agent.Close()
			if _, err := plan.agent.Run(context.Background(), agent.RunOpts{Prompt: "capture", CWD: plan.workDir, Purpose: "test-evidence"}); err != nil {
				t.Fatalf("run the recovered agent: %v", err)
			}

			if got := readCapturedAgentEnv(t, capturePath); got != tc.want {
				t.Fatalf("recovered agent saw UV_EXCLUDE_NEWER as %q, want %q", got, tc.want)
			}
		})
	}
}

type runAgentStep struct{}

func (runAgentStep) Name() types.StepName { return types.StepTest }

func (runAgentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "capture", CWD: sctx.WorkDir, Purpose: "test-evidence"})
	return &pipeline.StepOutcome{}, err
}

func TestStartRunGivesTheAgentEnvToTheRunStartedWithItAndToNoOtherRun(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	capturePath := useEnvCapturingPiAgent(t, p)
	withPairs, withPairsHead := setupTestGitRepo(t, p, database, "agent-env-with-pairs")
	without, withoutHead := setupTestGitRepo(t, p, database, "agent-env-without-pairs")
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{runAgentStep{}} })
	t.Cleanup(manager.Shutdown)
	pairs := map[string]string{"UV_EXCLUDE_NEWER": agentEnvCutoff}

	for _, tc := range []struct {
		name     string
		repo     *db.Repo
		head     string
		env      map[string]string
		wantSeen string
	}{
		{"run_started_with_pairs", withPairs, withPairsHead, pairs, "set:" + agentEnvCutoff},
		{"run_of_another_repository_started_without", without, withoutHead, nil, "unset"},
	} {
		runID, err := manager.startRunWithPRText(context.Background(), tc.repo, "main", tc.head, refreshTestZeroSHA, "test", nil, "carry the cutoff", "", false, "", nil, nil, tc.env, db.RunPRText{})
		if err != nil {
			t.Fatalf("%s: start run: %v", tc.name, err)
		}
		if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
			t.Fatalf("%s: run status = %s, error = %v", tc.name, run.Status, run.Error)
		}

		if got := readCapturedAgentEnv(t, capturePath); got != tc.wantSeen {
			t.Errorf("%s: the run's agent saw UV_EXCLUDE_NEWER as %q, want %q", tc.name, got, tc.wantSeen)
		}
		stored, err := database.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored.AgentEnv, tc.env) {
			t.Errorf("%s: stored run carries %v, want %v", tc.name, stored.AgentEnv, tc.env)
		}
		if err := os.Remove(capturePath); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartRunRefusesAnAgentEnvNameOutsideTheListWithoutARunRow(t *testing.T) {
	manager, _, database, repo, head, _ := newTestOutputDirsManager(t)

	_, err := manager.startRunWithPRText(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "refuse the name", "", false, "", nil, nil, map[string]string{"PATH": "/evil"}, db.RunPRText{})

	if err == nil || !strings.Contains(err.Error(), `"PATH"`) || !strings.Contains(err.Error(), "accepted names: PIP_UPLOADED_PRIOR_TO, UV_EXCLUDE_NEWER, npm_config_before") {
		t.Fatalf("start run = %v, want a refusal naming PATH and the accepted names", err)
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("a refused launch left %d run rows", len(runs))
	}
}

func TestFreshLaunchRefusesAnAgentEnvNameOutsideTheList(t *testing.T) {
	manager, _, database, repo, head, _ := newTestOutputDirsManager(t)
	params := &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "refuse the name", LaunchNonce: "agent-env-refused", ValidationGeneration: "generation", AgentEnv: map[string]string{"FOO": "bar"}}

	_, err := manager.HandleStartFreshRun(context.Background(), params)

	if err == nil || !strings.Contains(err.Error(), `"FOO"`) {
		t.Fatalf("fresh launch = %v, want a refusal naming FOO", err)
	}
	if runs, err := database.GetRunsByRepo(repo.ID); err != nil || len(runs) != 0 {
		t.Fatalf("a refused launch left %d run rows (%v)", len(runs), err)
	}
}

func TestFreshLaunchReplayKeepsTheRecordedAgentEnv(t *testing.T) {
	manager, _, database, repo, head, _ := newTestOutputDirsManager(t)
	first := map[string]string{"UV_EXCLUDE_NEWER": "2026-11-01T00:00:00Z"}
	params := &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "bind the cutoff", LaunchNonce: "agent-env-replay", ValidationGeneration: "generation", AgentEnv: first}
	receipt, err := manager.HandleStartFreshRun(context.Background(), params)
	if err != nil {
		t.Fatalf("fresh launch: %v", err)
	}
	if run := waitForRunTerminalState(t, database, receipt.RunID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}

	params.AgentEnv = map[string]string{"UV_EXCLUDE_NEWER": "2026-11-02T00:00:00Z"}
	replayed, err := manager.HandleStartFreshRun(context.Background(), params)

	if err != nil || replayed.RunID != receipt.RunID {
		t.Fatalf("replay with a recomputed cutoff = %+v, %v; want the bound run", replayed, err)
	}
	stored, err := database.GetRun(receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.AgentEnv, first) {
		t.Fatalf("replayed run carries %v, want the pairs recorded at its start %v", stored.AgentEnv, first)
	}
}

func TestRunToInfoReportsTheRunsAgentEnv(t *testing.T) {
	pairs := map[string]string{"npm_config_before": agentEnvCutoff}
	_, database := newRefreshRunFixture(t)

	info := runToInfo(database, &db.Run{ID: "run", AgentEnv: pairs}, nil)

	if !reflect.DeepEqual(info.AgentEnv, pairs) {
		t.Fatalf("run info carries %v, want %v", info.AgentEnv, pairs)
	}
}
