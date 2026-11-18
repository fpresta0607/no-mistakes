//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

var agentEnvNames = []string{"PIP_UPLOADED_PRIOR_TO", "UV_EXCLUDE_NEWER", "npm_config_before"}

// unsetAgentEnvForTest removes the per-run agent environment names from the
// test process so the harness, the daemon and everything they spawn start
// without them. t.Setenv(name, "") would leave them present and empty.
func unsetAgentEnvForTest(t *testing.T) {
	t.Helper()
	for _, name := range agentEnvNames {
		previous, wasSet := os.LookupEnv(name)
		if !wasSet {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() { os.Setenv(name, previous) })
	}
}

// envProbeInvocations splits the recorded invocations after offset into real
// fake-agent turns and nm-env-probe runs (the configured step commands).
func envProbeInvocations(all []Invocation, offset int) (agents, probes []Invocation) {
	for _, inv := range all[offset:] {
		if inv.Agent == "env-probe" {
			probes = append(probes, inv)
		} else {
			agents = append(agents, inv)
		}
	}
	return agents, probes
}

func assertInvocationEnv(t *testing.T, label string, invocations []Invocation, want map[string]string) {
	t.Helper()
	for i, inv := range invocations {
		if !reflect.DeepEqual(inv.Env, want) {
			t.Errorf("%s invocation %d (%s %v) env = %#v, want %#v", label, i, inv.Agent, inv.Args, inv.Env, want)
		}
	}
}

func probeRanStep(probes []Invocation, step string) bool {
	for _, probe := range probes {
		if len(probe.Args) > 0 && probe.Args[0] == step {
			return true
		}
	}
	return false
}

// TestAgentEnvReachesEveryAgentAndStepCommandOfItsRunOnly drives the real
// `axi run --agent-env` flag and proves the pairs reach every agent
// subprocess and configured step command of that run, are stored on the run,
// and never leak into a later run on the same daemon.
func TestAgentEnvReachesEveryAgentAndStepCommandOfItsRunOnly(t *testing.T) {
	unsetAgentEnvForTest(t)
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if err := os.Link(h.FakeAgent, filepath.Join(h.BinDir, executableName("nm-env-probe"))); err != nil {
		t.Fatalf("link env probe: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	wantEnv := map[string]string{
		"PIP_UPLOADED_PRIOR_TO": "P7D",
		"UV_EXCLUDE_NEWER":      "7 days",
		"npm_config_before":     "2026-11-11T00:00:00Z",
	}
	const probeCommands = "commands:\n  test: nm-env-probe test\n  lint: nm-env-probe lint\n"

	t.Run("pairs_reach_their_run", func(t *testing.T) {
		const branch = "feature/agent-env"
		h.CommitChange(branch, ".no-mistakes.yaml", probeCommands, "configure probe commands")
		h.CommitChange(branch, "feature.txt", "hello\n", "add feature")
		worktree := h.AddWorktree(branch)
		out, err := h.RunInDir(worktree, "axi", "run", "--intent", "Validate the agent environment journey",
			"--agent-env", "PIP_UPLOADED_PRIOR_TO=P7D",
			"--agent-env", "UV_EXCLUDE_NEWER=7 days",
			"--agent-env", "npm_config_before=2026-11-11T00:00:00Z")
		if err != nil {
			t.Fatalf("axi run --agent-env: %v\n%s", err, out)
		}
		run := h.WaitForRun(branch, 3*time.Minute)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
		}

		agents, probes := envProbeInvocations(h.AgentInvocations(), 0)
		if len(agents) == 0 {
			t.Fatal("run never invoked the fake agent")
		}
		assertInvocationEnv(t, "agent", agents, wantEnv)
		for _, step := range []string{"test", "lint"} {
			if !probeRanStep(probes, step) {
				t.Errorf("no env-probe invocation for the %s step; probes: %+v", step, probes)
			}
		}
		assertInvocationEnv(t, "step command", probes, wantEnv)

		if got := h.RunInfo(run.ID).AgentEnv; !reflect.DeepEqual(got, wantEnv) {
			t.Errorf("RunInfo.AgentEnv = %#v, want %#v", got, wantEnv)
		}
	})

	t.Run("pairs_do_not_leak_into_the_next_run", func(t *testing.T) {
		offset := len(h.AgentInvocations())
		const branch = "feature/agent-env-none"
		h.CommitChange(branch, ".no-mistakes.yaml", probeCommands, "configure probe commands")
		h.CommitChange(branch, "feature.txt", "hello again\n", "add feature")
		worktree := h.AddWorktree(branch)
		if out, err := h.RunInDir(worktree, "axi", "run", "--intent", "Validate a run without agent environment"); err != nil {
			t.Fatalf("axi run: %v\n%s", err, out)
		}
		run := h.WaitForRun(branch, 3*time.Minute)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
		}

		agents, probes := envProbeInvocations(h.AgentInvocations(), offset)
		if len(agents) == 0 {
			t.Fatal("second run never invoked the fake agent")
		}
		if !probeRanStep(probes, "test") || !probeRanStep(probes, "lint") {
			t.Errorf("second run's step commands did not both run; probes: %+v", probes)
		}
		assertInvocationEnv(t, "agent", agents, nil)
		assertInvocationEnv(t, "step command", probes, nil)
		if got := h.RunInfo(run.ID).AgentEnv; len(got) != 0 {
			t.Errorf("RunInfo.AgentEnv = %#v, want empty", got)
		}
	})

	t.Run("unknown_name_is_refused_before_any_run", func(t *testing.T) {
		const branch = "feature/agent-env-refused"
		h.CommitChange(branch, "feature.txt", "refused\n", "add feature")
		worktree := h.AddWorktree(branch)
		runsBefore := len(h.Runs())
		invocationsBefore := len(h.AgentInvocations())

		out, err := h.RunInDir(worktree, "axi", "run", "--intent", "Refuse an unknown agent env name", "--agent-env", "FOO=1")
		if err == nil {
			t.Fatalf("axi run --agent-env FOO=1 succeeded, want a refusal\n%s", out)
		}
		if !strings.Contains(out, "FOO") {
			t.Errorf("refusal does not name FOO:\n%s", out)
		}
		if runsAfter := len(h.Runs()); runsAfter != runsBefore {
			t.Errorf("refused invocation started a run: %d runs before, %d after", runsBefore, runsAfter)
		}
		if invocationsAfter := len(h.AgentInvocations()); invocationsAfter != invocationsBefore {
			t.Errorf("refused invocation reached an agent: %d invocations before, %d after", invocationsBefore, invocationsAfter)
		}
	})
}
