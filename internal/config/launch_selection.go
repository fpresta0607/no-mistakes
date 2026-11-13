package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ApplyLaunchSelection narrows one run to the agent chains its launch
// selection names: the primary runs its chain, in order, each harness at the
// selected model and effort, whatever agent list, review roles and
// agent_config model or effort the global and trusted repository
// configuration name. A reviewer or fixer chain equal to the primary leaves
// that role on the primary chain; one that differs pins review_agents for that
// role, the reviewer's later profiles as its fallback. Fresh slices and maps
// keep that configuration, and every concurrent run, as they were. Raw
// agent_args_override flags stay the operator's, so the launch proof that
// follows still refuses a flag that selects something else, and a Codex
// service tier is proved from those flags, never set here.
func (c *Config) ApplyLaunchSelection(primary, reviewer, fixer []agentcfg.Selection) error {
	if len(primary) == 0 {
		return fmt.Errorf("launch selection names no agent")
	}
	reviewAgents := make(map[string]ReviewAgent, 2)
	if !slices.Equal(reviewer, primary) {
		pinned, err := launchSelectionReviewAgent(reviewer)
		if err != nil {
			return err
		}
		reviewAgents[RoleReviewer] = pinned
	}
	if !slices.Equal(fixer, primary) {
		if len(fixer) > 1 {
			return fmt.Errorf("launch selection names one fixer profile unless the fixer runs the primary chain")
		}
		pinned, err := launchSelectionReviewAgent(fixer)
		if err != nil {
			return err
		}
		reviewAgents[RoleFixer] = pinned
	}
	agents := make([]types.AgentName, 0, len(primary))
	profiles := maps.Clone(c.AgentConfig)
	if profiles == nil {
		profiles = make(map[string]agentcfg.Profile, len(primary))
	}
	for _, selected := range primary {
		if slices.Contains(agents, selected.Harness) {
			return fmt.Errorf("launch selection names an agent harness once")
		}
		agents = append(agents, selected.Harness)
		profiles[string(selected.Harness)] = agentcfg.Profile{Model: selected.Model, Effort: selected.Effort}
	}
	c.Agent = agents[0]
	c.Agents = agents
	c.ReviewAgents = nil
	if len(reviewAgents) > 0 {
		c.ReviewAgents = reviewAgents
	}
	c.AgentConfig = profiles
	return nil
}

// launchSelectionReviewAgent pins a review role to a selected chain: its first
// profile serves the role and the rest are its fallback, in order.
func launchSelectionReviewAgent(chain []agentcfg.Selection) (ReviewAgent, error) {
	if len(chain) == 0 {
		return ReviewAgent{}, fmt.Errorf("launch selection names no agent for a review role")
	}
	var harnesses []types.AgentName
	entries := make([]ReviewAgent, 0, len(chain))
	for _, selected := range chain {
		if slices.Contains(harnesses, selected.Harness) {
			return ReviewAgent{}, fmt.Errorf("launch selection names an agent harness once")
		}
		harnesses = append(harnesses, selected.Harness)
		entries = append(entries, ReviewAgent{Agent: selected.Harness, Model: selected.Model, Effort: selected.Effort})
	}
	pinned := entries[0]
	if len(entries) > 1 {
		pinned.Fallback = entries[1:]
	}
	return pinned, nil
}
