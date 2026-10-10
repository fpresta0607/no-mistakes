package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ApplyLaunchSelection narrows one run to the agent chain its launch selection
// names: every role runs that chain, in order, each harness at the selected
// model and effort, whatever agent list, review roles and agent_config model
// or effort the global and trusted repository configuration name. Fresh
// slices and maps keep that configuration, and every concurrent run, as they
// were. Raw agent_args_override flags stay the operator's, so the launch
// proof that follows still refuses a flag that selects something else, and a
// Codex service tier is proved from those flags, never set here.
func (c *Config) ApplyLaunchSelection(chain []agentcfg.Selection) error {
	if len(chain) == 0 {
		return fmt.Errorf("launch selection names no agent")
	}
	agents := make([]types.AgentName, 0, len(chain))
	profiles := maps.Clone(c.AgentConfig)
	if profiles == nil {
		profiles = make(map[string]agentcfg.Profile, len(chain))
	}
	for _, selected := range chain {
		if slices.Contains(agents, selected.Harness) {
			return fmt.Errorf("launch selection names an agent harness once")
		}
		agents = append(agents, selected.Harness)
		profiles[string(selected.Harness)] = agentcfg.Profile{Model: selected.Model, Effort: selected.Effort}
	}
	c.Agent = agents[0]
	c.Agents = agents
	c.ReviewAgents = nil
	c.AgentConfig = profiles
	return nil
}
