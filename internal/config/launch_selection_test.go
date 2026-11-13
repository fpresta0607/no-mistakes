package config

import (
	"reflect"
	"strings"
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

	if err := cfg.ApplyLaunchSelection(chain, chain, chain); err != nil {
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

	if err := cfg.ApplyLaunchSelection(chain, chain, chain); err != nil {
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
		if err := cfg.ApplyLaunchSelection(chain, chain, chain); err == nil {
			t.Fatalf("%s chain was applied", name)
		}
		if cfg.Agent != types.AgentCodex || cfg.Agents != nil {
			t.Fatalf("a refused %s chain changed the configuration: %s %v", name, cfg.Agent, cfg.Agents)
		}
	}
}

func TestApplyLaunchSelectionPinsTheReviewerAndFixerTheirOwnChains(t *testing.T) {
	primary := agentcfg.Selection{Harness: types.AgentClaude, Model: "task-model", Effort: agentcfg.EffortHigh}
	reviewer := agentcfg.Selection{Harness: types.AgentClaude, Model: "review-model", Effort: agentcfg.EffortXHigh}
	reviewerFallback := agentcfg.Selection{Harness: types.AgentCodex, Model: "review-fallback", Effort: agentcfg.EffortHigh, ServiceTier: "default"}
	fixer := agentcfg.Selection{Harness: types.AgentClaude, Model: "fix-model", Effort: agentcfg.EffortMedium}
	cfg := &Config{
		Agent:        types.AgentCodex,
		ReviewAgents: map[string]ReviewAgent{RoleReviewerAfterRound: {Agent: types.AgentCodex}},
		AgentConfig:  map[string]agentcfg.Profile{"claude": {Model: "operator-claude", Effort: agentcfg.EffortLow}},
	}

	if err := cfg.ApplyLaunchSelection([]agentcfg.Selection{primary}, []agentcfg.Selection{reviewer, reviewerFallback}, []agentcfg.Selection{fixer}); err != nil {
		t.Fatal(err)
	}

	want := map[string]ReviewAgent{
		RoleReviewer: {Agent: types.AgentClaude, Model: "review-model", Effort: agentcfg.EffortXHigh, Fallback: []ReviewAgent{
			{Agent: types.AgentCodex, Model: "review-fallback", Effort: agentcfg.EffortHigh},
		}},
		RoleFixer: {Agent: types.AgentClaude, Model: "fix-model", Effort: agentcfg.EffortMedium},
	}
	if !reflect.DeepEqual(cfg.ReviewAgents, want) {
		t.Fatalf("review roles = %+v, want %+v", cfg.ReviewAgents, want)
	}
	if !reflect.DeepEqual(cfg.Agents, []types.AgentName{types.AgentClaude}) {
		t.Fatalf("primary chain = %v, want Claude alone", cfg.Agents)
	}
	if want := (agentcfg.Profile{Model: "task-model", Effort: agentcfg.EffortHigh}); cfg.AgentProfileFor(types.AgentClaude) != want {
		t.Fatalf("primary Claude profile = %+v, want %+v", cfg.AgentProfileFor(types.AgentClaude), want)
	}
	if got := cfg.ForReviewAgent(cfg.ReviewAgents[RoleFixer]).AgentProfile(); got != (agentcfg.Profile{Model: "fix-model", Effort: agentcfg.EffortMedium}) {
		t.Fatalf("fixer profile = %+v", got)
	}
}

func TestApplyLaunchSelectionRefusesAFixerWithAFallback(t *testing.T) {
	primary := []agentcfg.Selection{{Harness: types.AgentClaude, Model: "task-model", Effort: agentcfg.EffortHigh}}
	fixer := []agentcfg.Selection{
		{Harness: types.AgentClaude, Model: "fix-model", Effort: agentcfg.EffortHigh},
		{Harness: types.AgentCodex, Model: "fix-fallback", Effort: agentcfg.EffortHigh, ServiceTier: "default"},
	}
	cfg := &Config{Agent: types.AgentCodex}

	err := cfg.ApplyLaunchSelection(primary, primary, fixer)

	if err == nil || !strings.Contains(err.Error(), "fixer") {
		t.Fatalf("a fixer chain with a fallback was applied: %v", err)
	}
	if cfg.Agent != types.AgentCodex || cfg.Agents != nil || cfg.ReviewAgents != nil {
		t.Fatalf("a refused selection changed the configuration: %+v", cfg)
	}
}
