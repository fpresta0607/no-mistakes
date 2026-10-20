package config

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestApplyLaunchSelectionReplacesTheChainForEveryRole(t *testing.T) {
	shared := map[string]agentcfg.Profile{
		"codex":  {Model: "operator-codex", Effort: agentcfg.EffortXHigh},
		"claude": {Model: "operator-claude", Effort: agentcfg.EffortLow},
	}
	args := map[string][]string{"claude": {"--strict-mcp-config"}}
	cfg := &Config{
		Agent:             types.AgentCodex,
		Agents:            []types.AgentName{types.AgentCodex, types.AgentClaude},
		ReviewAgents:      map[string]ReviewAgent{RoleReviewer: {Agent: types.AgentCodex}},
		AgentConfig:       shared,
		AgentArgsOverride: args,
	}
	chain := []agentcfg.Selection{{Harness: types.AgentClaude, Model: "task-model", Effort: agentcfg.EffortHigh}}

	if err := cfg.ApplyLaunchSelection(chain); err != nil {
		t.Fatal(err)
	}

	if cfg.Agent != types.AgentClaude || !reflect.DeepEqual(cfg.Agents, []types.AgentName{types.AgentClaude}) {
		t.Fatalf("chain = %s %v, want Claude alone", cfg.Agent, cfg.Agents)
	}
	if cfg.ReviewAgents != nil {
		t.Fatalf("a review role kept a profile of its own: %+v", cfg.ReviewAgents)
	}
	if want := (agentcfg.Profile{Model: "task-model", Effort: agentcfg.EffortHigh}); cfg.AgentProfileFor(types.AgentClaude) != want {
		t.Fatalf("Claude profile = %+v, want %+v", cfg.AgentProfileFor(types.AgentClaude), want)
	}
	if shared["claude"].Model != "operator-claude" || len(shared) != 2 {
		t.Fatalf("the selection wrote into the configuration other runs share: %+v", shared)
	}
	if !reflect.DeepEqual(cfg.AgentArgsFor(types.AgentClaude), []string{"--strict-mcp-config"}) {
		t.Fatalf("the operator's arguments changed: %v", cfg.AgentArgsFor(types.AgentClaude))
	}
}

func TestApplyLaunchSelectionKeepsANamedFallbackInOrder(t *testing.T) {
	cfg := &Config{Agent: types.AgentCodex}
	chain := []agentcfg.Selection{
		{Harness: types.AgentClaude, Model: "task-model", Effort: agentcfg.EffortHigh},
		{Harness: types.AgentCodex, Model: "fallback-model", Effort: agentcfg.EffortXHigh, ServiceTier: "default"},
	}

	if err := cfg.ApplyLaunchSelection(chain); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(cfg.Agents, []types.AgentName{types.AgentClaude, types.AgentCodex}) {
		t.Fatalf("chain = %v, want Claude then Codex", cfg.Agents)
	}
	if want := (agentcfg.Profile{Model: "fallback-model", Effort: agentcfg.EffortXHigh}); cfg.AgentProfileFor(types.AgentCodex) != want {
		t.Fatalf("fallback profile = %+v, want %+v", cfg.AgentProfileFor(types.AgentCodex), want)
	}
}

func TestApplyLaunchSelectionRefusesAnEmptyOrRepeatedChain(t *testing.T) {
	claude := agentcfg.Selection{Harness: types.AgentClaude, Model: "task-model", Effort: agentcfg.EffortHigh}
	for name, chain := range map[string][]agentcfg.Selection{"empty": nil, "repeated": {claude, claude}} {
		cfg := &Config{Agent: types.AgentCodex}
		if err := cfg.ApplyLaunchSelection(chain); err == nil {
			t.Fatalf("%s chain was applied", name)
		}
		if cfg.Agent != types.AgentCodex || cfg.Agents != nil {
			t.Fatalf("a refused %s chain changed the configuration: %s %v", name, cfg.Agent, cfg.Agents)
		}
	}
}
