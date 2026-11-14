package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/quotarouting"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// decodeLaunchParams validates the launch assertion before the typed decode.
// encoding/json matches field names case-insensitively and lets a later key
// win, so a case variant such as LAUNCH_ASSERTION:null would silently erase a
// validated assertion. Only the exact key may carry it.
func decodeLaunchParams(data json.RawMessage, target interface{}) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		if !strings.EqualFold(key, "launch_assertion") {
			continue
		}
		if key != "launch_assertion" {
			return fmt.Errorf("non-canonical launch assertion field %q", key)
		}
		var expected launchassert.Expectation
		if err := json.Unmarshal(raw, &expected); err != nil {
			return err
		}
	}
	return json.Unmarshal(data, target)
}

// resolvePipelineAgentRoles resolves the primary agent and every candidate of
// each review role, fallbacks included, so a launch proof covers each agent the
// run could start.
func resolvePipelineAgentRoles(ctx context.Context, cfg *config.Config, lookPath func(string) (string, error)) (map[string][]*config.Config, error) {
	if err := cfg.ResolveAgent(ctx, lookPath); err != nil {
		return nil, err
	}
	roleConfigs := make(map[string][]*config.Config, len(cfg.ReviewAgents))
	for _, role := range config.ReviewAgentRoles {
		entry, hasRole := cfg.ReviewAgents[role]
		if !hasRole {
			continue
		}
		for _, roleConfig := range reviewCandidateConfigs(cfg, entry) {
			roleConfig.QuotaRouting.Enabled = false
			if err := roleConfig.ResolveAgent(ctx, lookPath); err != nil {
				return nil, fmt.Errorf("resolve review_agents.%s: %w", role, err)
			}
			roleConfigs[role] = append(roleConfigs[role], roleConfig)
		}
	}
	return roleConfigs, nil
}

// replayQuotaRouting reads the quota order a run stored, for its recovery or
// its successor. A run that stored none stays off the feature instead of
// reading a fresh order.
func replayQuotaRouting(cfg *config.Config, stored string) (*quotarouting.Record, error) {
	record, err := storedQuotaRoutingRecord(stored)
	if err != nil {
		return nil, err
	}
	if record == nil {
		cfg.QuotaRouting.Enabled = false
	}
	return record, nil
}

func (m *RunManager) newRunPipelineAgent(ctx context.Context, run *db.Run, cfg *config.Config, evidenceRoot string, lookPath func(string) (string, error), environment runenv.Overlay, quota *quotarouting.Record) (agent.Agent, error) {
	family, err := m.db.ValidationFamily(run.ID)
	if err != nil {
		return nil, err
	}
	if family != nil {
		if family.HasChildRepairs {
			return nil, fmt.Errorf("validation-only child contains a forbidden repair selection")
		}
		cfg.AutoFix, cfg.SideBySide = config.AutoFix{}, false
	}
	if err := run.LaunchAssertionProof.Check(run.LaunchAssertion); err != nil {
		return nil, err
	}
	if run.LaunchAssertion == nil {
		ag, _, err := newPipelineAgent(ctx, cfg, evidenceRoot, lookPath, environment, quota)
		return ag, err
	}
	expected := run.LaunchAssertion
	event, err := m.db.GetTerminalPredecessor(run.ID)
	if err != nil {
		return nil, err
	}
	if event != nil {
		if !event.Assertion.Matches(run.LaunchAssertion) || event.Snapshot.Run.LaunchValidationGeneration == nil || run.LaunchValidationGeneration == nil || *event.Snapshot.Run.LaunchValidationGeneration != *run.LaunchValidationGeneration {
			return nil, fmt.Errorf("terminal recovery launch provenance conflicts with the original receipt")
		}
		if err := event.Proof.Check(event.Assertion); err != nil {
			return nil, err
		}
		expected = event.Assertion
		freezeTerminalLimits(cfg, &event.Snapshot)
	}
	roleConfigs, _, err := prepareLaunchAssertion(ctx, expected, cfg, lookPath)
	if err != nil {
		return nil, err
	}
	ag, _, err := newResolvedPipelineAgent(ctx, cfg, roleConfigs, evidenceRoot, environment, quota)
	return ag, err
}

func prepareTerminalLaunch(ctx context.Context, run *db.Run, cfg *config.Config, lookPath func(string) (string, error)) (*launchassert.Expectation, *launchassert.Proof, map[string][]*config.Config, error) {
	if run.LaunchAssertion == nil || run.LaunchValidationGeneration == nil || *run.LaunchValidationGeneration == "" {
		return nil, nil, nil, fmt.Errorf("terminal recovery needs the original bound launch selection and frozen policy")
	}
	if err := run.LaunchAssertionProof.Check(run.LaunchAssertion); err != nil {
		return nil, nil, nil, err
	}
	fresh := *run.LaunchAssertion
	fresh.TrustedSHA = cfg.TrustedConfigSHA
	roles, proof, err := prepareLaunchAssertion(ctx, &fresh, cfg, lookPath)
	if err != nil {
		return nil, nil, nil, err
	}
	return &fresh, proof, roles, nil
}

// applyLaunchSelection gives a run the agents its launch selection names,
// before prepareLaunchAssertion proves them. An assertion that applies
// nothing leaves cfg as the configuration resolved it.
func applyLaunchSelection(cfg *config.Config, expected *launchassert.Expectation) error {
	if expected == nil || !expected.Apply {
		return nil
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	return cfg.ApplyLaunchSelection(expected.Profiles["primary"], expected.Profiles[config.RoleReviewer], expected.Profiles[config.RoleFixer])
}

func prepareLaunchAssertion(ctx context.Context, expected *launchassert.Expectation, cfg *config.Config, lookPath func(string) (string, error)) (map[string][]*config.Config, *launchassert.Proof, error) {
	if expected.TrustedSHA != cfg.TrustedConfigSHA {
		return nil, nil, fmt.Errorf("launch assertion trusted source differs after fresh fetch")
	}
	if steps.IsDemoMode() {
		return nil, nil, fmt.Errorf("demo mode cannot prove an asserted agent launch")
	}
	// The proof records the chain in order, so quota routing may not reorder it.
	// Authored children and terminal successors descend from an asserted run, so
	// none of them inherits a stored quota order either.
	cfg.QuotaRouting.Enabled = false
	roleConfigs, err := resolvePipelineAgentRoles(ctx, cfg, lookPath)
	if err != nil {
		return nil, nil, err
	}
	profiles := make(map[string][]agentcfg.Selection, 1+len(config.ReviewAgentRoles))
	for _, role := range append([]string{"primary"}, config.ReviewAgentRoles...) {
		candidates := roleConfigs[role]
		if len(candidates) == 0 {
			if role == config.RoleReviewerAfterRound || role == config.RoleFixerAfterRound {
				continue
			}
			candidates = []*config.Config{cfg}
		}
		for _, roleConfig := range candidates {
			for _, name := range roleConfig.Agents {
				selection, err := agentcfg.EffectiveSelection(name, roleConfig.AgentProfileFor(name), roleConfig.AgentArgsFor(name))
				if err != nil {
					return nil, nil, fmt.Errorf("prove %s agent selection: %w", role, err)
				}
				profiles[role] = append(profiles[role], selection)
			}
		}
	}
	proof, err := expected.Verify(cfg.TrustedConfigSHA, profiles)
	if err != nil {
		return nil, nil, err
	}
	return roleConfigs, proof, nil
}
