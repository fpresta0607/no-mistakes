package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func decodeLaunchParams(data json.RawMessage, target interface{}) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, hasAssertion := fields["launch_assertion"]; hasAssertion {
		var expected launchassert.Expectation
		if err := json.Unmarshal(raw, &expected); err != nil {
			return err
		}
	}
	return json.Unmarshal(data, target)
}

func resolvePipelineAgentRoles(ctx context.Context, cfg *config.Config, lookPath func(string) (string, error)) (map[string]*config.Config, error) {
	if err := cfg.ResolveAgent(ctx, lookPath); err != nil {
		return nil, err
	}
	roleConfigs := make(map[string]*config.Config, len(cfg.ReviewAgents))
	for _, role := range config.ReviewAgentRoles {
		if entry, hasRole := cfg.ReviewAgents[role]; hasRole {
			roleConfig := cfg.ForReviewAgent(entry)
			if err := roleConfig.ResolveAgent(ctx, lookPath); err != nil {
				return nil, fmt.Errorf("resolve review_agents.%s: %w", role, err)
			}
			roleConfigs[role] = roleConfig
		}
	}
	return roleConfigs, nil
}

func (m *RunManager) newRunPipelineAgent(ctx context.Context, run *db.Run, cfg *config.Config, evidenceRoot string, lookPath func(string) (string, error), environment runenv.Overlay) (agent.Agent, error) {
	if err := run.LaunchAssertionProof.Check(run.LaunchAssertion); err != nil {
		return nil, err
	}
	if run.LaunchAssertion == nil {
		return newPipelineAgent(ctx, cfg, evidenceRoot, lookPath, environment)
	}
	roleConfigs, _, err := prepareLaunchAssertion(ctx, run.LaunchAssertion, cfg, lookPath)
	if err != nil {
		return nil, err
	}
	return newResolvedPipelineAgent(cfg, roleConfigs, evidenceRoot, environment)
}

func prepareLaunchAssertion(ctx context.Context, expected *launchassert.Expectation, cfg *config.Config, lookPath func(string) (string, error)) (map[string]*config.Config, *launchassert.Proof, error) {
	if expected.TrustedSHA != cfg.TrustedConfigSHA {
		return nil, nil, fmt.Errorf("launch assertion trusted source differs after fresh fetch")
	}
	if steps.IsDemoMode() {
		return nil, nil, fmt.Errorf("demo mode cannot prove an asserted agent launch")
	}
	roleConfigs, err := resolvePipelineAgentRoles(ctx, cfg, lookPath)
	if err != nil {
		return nil, nil, err
	}
	profiles := make(map[string][]agentcfg.Selection, 1+len(config.ReviewAgentRoles))
	for _, role := range append([]string{"primary"}, config.ReviewAgentRoles...) {
		roleConfig := roleConfigs[role]
		if roleConfig == nil {
			if role == config.RoleReviewerAfterRound || role == config.RoleFixerAfterRound {
				continue
			}
			roleConfig = cfg
		}
		for _, name := range roleConfig.Agents {
			selection, err := agentcfg.EffectiveSelection(name, roleConfig.AgentProfileFor(name), roleConfig.AgentArgsFor(name))
			if err != nil {
				return nil, nil, fmt.Errorf("prove %s agent selection: %w", role, err)
			}
			profiles[role] = append(profiles[role], selection)
		}
	}
	proof, err := expected.Verify(cfg.TrustedConfigSHA, profiles)
	if err != nil {
		return nil, nil, err
	}
	return roleConfigs, proof, nil
}
