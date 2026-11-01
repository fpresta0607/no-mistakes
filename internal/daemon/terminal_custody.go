package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func (m *RunManager) recoverTerminal(ctx context.Context, request ipc.RecoverTerminalParams, operator int) (*ipc.RecoverTerminalResult, error) {
	if operator <= 0 || !filepath.IsAbs(request.CallerDir) || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 {
		return nil, fmt.Errorf("terminal recovery requires an authenticated caller directory and explicit reason")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var result *ipc.RecoverTerminalResult
	_, err := m.withBranchLock(request.RepoID, request.Branch, func() (string, error) {
		if m.shuttingDown.Load() {
			return "", fmt.Errorf("daemon is shutting down")
		}
		runs, err := m.db.GetRunsByRepo(request.RepoID)
		if err != nil {
			return "", err
		}
		var selected *db.Run
		for _, run := range runs {
			if run.Branch == request.Branch {
				selected = run
				break
			}
		}
		if selected == nil || selected.ID != request.RunID {
			return "", fmt.Errorf("terminal request no longer identifies this branch's run")
		}
		snapshot, err := m.db.ReadTerminalSnapshot(selected.ID)
		if err != nil {
			return "", err
		}
		repo := &snapshot.Repo
		branch, err := git.CurrentBranch(ctx, request.CallerDir)
		if err != nil || branch != selected.Branch {
			return "", fmt.Errorf("caller branch changed")
		}
		callerCommon, err := git.Run(ctx, request.CallerDir, "rev-parse", "--git-common-dir")
		if err != nil {
			return "", err
		}
		repoCommon, err := git.Run(ctx, repo.WorkingPath, "rev-parse", "--git-common-dir")
		if err != nil || !samePath(resolveGitPath(request.CallerDir, callerCommon), resolveGitPath(repo.WorkingPath, repoCommon)) {
			return "", fmt.Errorf("caller repository does not match the run")
		}
		event, err := m.db.GetTerminalAdoption(selected.ID)
		if err != nil {
			return "", err
		}
		original := snapshot
		if event != nil {
			original = &event.Snapshot
		}
		if request.SnapshotDigest != original.Digest() {
			return "", fmt.Errorf("terminal run, generation, receipt or findings changed since observation")
		}
		m.mu.Lock()
		executor := m.executors[selected.ID]
		m.mu.Unlock()
		if event != nil {
			result = &ipc.RecoverTerminalResult{RunID: selected.ID, CandidateSHA: event.CandidateSHA, AdoptionID: event.ID}
			if executor != nil {
				return selected.ID, nil
			}
			if selected.HeadSHA != event.CandidateSHA || len(snapshot.Rounds) != len(original.Rounds) || !reflect.DeepEqual(snapshot.Rounds, original.Rounds) {
				return selected.ID, nil
			}
		} else if executor != nil || !selected.Status.Terminal() || selected.Status == types.RunCompleted {
			return "", fmt.Errorf("only a terminal run without an executor can adopt source")
		}
		if err := validateTerminalAgentInactivity(snapshot); err != nil {
			return "", err
		}
		service := &branchsync.Service{DB: m.db, Repo: repo, WorkDir: request.CallerDir, GateDir: m.paths.RepoDir(repo.ID), Paths: m.paths}
		var source *branchsync.TerminalSource
		if event == nil {
			source, err = service.TerminalSource(ctx, &original.Run)
		} else {
			source, err = service.AcceptedTerminalSource(ctx, event, selected)
		}
		if err != nil {
			return "", err
		}
		if event != nil && (source.Head != event.CandidateSHA || source.Tree != event.CandidateTree || source.GateHead != event.GateHead) {
			return "", fmt.Errorf("accepted source or gate lane changed")
		}
		if source.Missing {
			if err := git.WorktreeAdd(ctx, service.GateDir, source.ManagedDir, source.Head); err != nil {
				return "", fmt.Errorf("reconstruct exact preserved source: %w", err)
			}
		}
		if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, source.ManagedDir); err != nil {
			return "", fmt.Errorf("configure terminal source identity: %w", err)
		}
		cfg, err := m.loadRecoveredConfig(ctx, &original.Run, repo, source.ManagedDir)
		if err != nil {
			return "", err
		}
		execSteps := steps.WithCustomGates(m.steps(), cfg.Gates)
		if err := validateTerminalSteps(original, execSteps); err != nil {
			return "", err
		}
		freezeTerminalLimits(cfg, original)
		assertion, proof, roleConfigs, err := prepareTerminalLaunch(ctx, &original.Run, cfg, exec.LookPath)
		if err != nil {
			return "", err
		}
		forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
		if err != nil {
			return "", err
		}
		if err := proveTerminalPublication(ctx, &original.Run, repo, source.ManagedDir, forge, cfg); err != nil {
			return "", err
		}
		ag, err := newResolvedPipelineAgent(cfg, roleConfigs, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), forgeEnvironment(forge))
		if err != nil {
			return "", err
		}
		isScheduled := false
		defer func() {
			if !isScheduled {
				_ = ag.Close()
			}
		}()
		callerHead, err := git.HeadSHA(ctx, request.CallerDir)
		if err != nil {
			return "", err
		}
		if source.Missing {
			source.Missing = false
		}
		var rechecked *branchsync.TerminalSource
		if event == nil {
			rechecked, err = service.TerminalSource(ctx, &original.Run)
		} else {
			rechecked, err = service.AcceptedTerminalSource(ctx, event, selected)
		}
		if err != nil || !reflect.DeepEqual(source, rechecked) {
			return "", fmt.Errorf("candidate source changed during verification")
		}
		if event == nil {
			candidateRef := "refs/no-mistakes/terminal-candidate/" + selected.ID
			for _, anchor := range []struct{ ref, head string }{{candidateRef, source.Head}, {"refs/no-mistakes/terminal-lane/" + selected.ID, source.GateHead}} {
				if err := custody.PreserveRecoveryAnchor(ctx, service.GateDir, anchor.ref, anchor.head); err != nil {
					return "", err
				}
			}
			event, _, err = m.db.AdoptTerminalCandidate(ctx, db.TerminalAdoption{Snapshot: *original, CandidateSHA: source.Head, CandidateTree: source.Tree, SourceRef: candidateRef, GateHead: source.GateHead, RecoveryHead: source.RecoveryHead, CallerDir: request.CallerDir, CallerHead: callerHead, ManagedDir: source.ManagedDir, Reason: request.Reason, Operator: strconv.Itoa(operator), Assertion: assertion, Proof: proof})
			if err != nil {
				return "", err
			}
		} else {
			if !assertion.Matches(event.Assertion) || proof.Check(event.Assertion) != nil {
				return "", fmt.Errorf("trusted source changed after acceptance; existing provenance cannot be rewritten")
			}
			if err := m.db.RestartAcceptedTerminal(ctx, snapshot); err != nil {
				return "", err
			}
		}
		fresh, err := m.db.GetRun(selected.ID)
		if err != nil {
			return "", err
		}
		if err := pipeline.ValidateTerminalRestart(m.db, fresh, execSteps, event); err != nil {
			return "", err
		}
		result = &ipc.RecoverTerminalResult{RunID: fresh.ID, CandidateSHA: event.CandidateSHA, AdoptionID: event.ID}
		if ctx.Err() != nil {
			return fresh.ID, nil
		}
		m.subMu.Lock()
		delete(m.completedRuns, fresh.ID)
		m.subMu.Unlock()
		m.resumeRecoveredRun(recoveredRunPlan{run: fresh, repo: repo, workDir: source.ManagedDir, gateDir: service.GateDir, cfg: cfg, agent: ag, steps: execSteps, forge: forge, terminal: event})
		isScheduled, result.Started = true, true
		return fresh.ID, nil
	})
	return result, err
}

func validateTerminalAgentInactivity(snapshot *db.TerminalSnapshot) error {
	for _, step := range snapshot.Steps {
		if step.AgentPID == nil || *step.AgentPID == 0 {
			continue
		}
		isRunning, err := processRunning(*step.AgentPID)
		if err != nil || isRunning {
			return fmt.Errorf("terminal agent %d is still running or cannot be inspected", *step.AgentPID)
		}
	}
	return nil
}

func validateTerminalSteps(snapshot *db.TerminalSnapshot, sequence []pipeline.Step) error {
	if len(snapshot.Steps) != len(sequence) || len(snapshot.Rounds) != len(sequence) {
		return fmt.Errorf("terminal recovery requires the complete original step sequence")
	}
	for index, step := range snapshot.Steps {
		if step.StepName != sequence[index].Name() || step.StepOrder != step.StepName.Order() {
			return fmt.Errorf("terminal step identity changed")
		}
		if step.StepOrder < types.StepReview.Order() && step.Status != types.StepStatusCompleted {
			return fmt.Errorf("terminal recovery requires completed intent and rebase")
		}
		if step.Status == types.StepStatusSkipped && !(step.StepName == types.StepCI && step.Error != nil && *step.Error == types.RunCIMonitorInterruptedReason) {
			return fmt.Errorf("terminal recovery refuses an unvalidated skipped step")
		}
	}
	return nil
}

func freezeTerminalLimits(cfg *config.Config, snapshot *db.TerminalSnapshot) {
	for _, step := range snapshot.Steps {
		limit := 0
		if step.AutoFixLimit != nil {
			limit = min(*step.AutoFixLimit, cfg.AutoFixLimit(step.StepName))
		}
		switch step.StepName {
		case types.StepReview:
			cfg.AutoFix.Review = limit
		case types.StepTest:
			cfg.AutoFix.Test = limit
		case types.StepDocument:
			cfg.AutoFix.Document = limit
		case types.StepLint:
			cfg.AutoFix.Lint = limit
		case types.StepCI:
			cfg.AutoFix.CI = limit
		case types.StepRebase:
			cfg.AutoFix.Rebase = limit
		}
	}
}

func proveTerminalPublication(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, forge *forgecontext.Context, cfg *config.Config) error {
	if run.LastPushedSHA == nil {
		if run.PRURL != nil || run.PushGeneration != nil {
			return fmt.Errorf("terminal publication binding is incomplete")
		}
		return nil
	}
	if run.PushGeneration == nil || *run.PushGeneration <= 0 || run.PushRef == nil || *run.PushRef != "refs/heads/"+run.Branch || run.PushTargetFingerprint == nil || *run.PushTargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) || run.PRURL == nil || run.PRState == nil || *run.PRState != "open" {
		return fmt.Errorf("terminal publication binding is incomplete or changed")
	}
	kind := "upstream"
	if repo.ForkURL != "" {
		kind = "fork"
	}
	if run.PushTargetKind == nil || *run.PushTargetKind != kind || run.LastPushedAt == nil {
		return fmt.Errorf("terminal publication target kind changed")
	}
	ctx = git.WithEnvironment(ctx, forgeEnvironment(forge))
	head, err := git.LsRemote(ctx, workDir, repo.PushURL(), *run.PushRef)
	if err != nil || head != *run.LastPushedSHA {
		return fmt.Errorf("public head moved or cannot be read")
	}
	if scm.DetectProviderStaticContext(ctx, repo.UpstreamURL) != scm.ProviderGitHub {
		return fmt.Errorf("terminal published recovery requires verifiable GitHub PR identity")
	}
	number, err := scm.ExtractPRNumber(*run.PRURL)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "gh", "api", "--hostname", scm.ResolveHost(ctx, repo.UpstreamURL), "repos/"+github.RepoSlug(repo.UpstreamURL)+"/pulls/"+number)
	command.Dir, command.Env = workDir, forgeEnvironment(forge).Apply(nil)
	winproc.Harden(command)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("cannot read the bound terminal PR")
	}
	var pr struct {
		URL   string `json:"html_url"`
		State string `json:"state"`
		Head  struct {
			SHA  string
			Ref  string
			Repo struct {
				FullName string `json:"full_name"`
			}
		}
		Base struct {
			Ref  string
			Repo struct {
				FullName string `json:"full_name"`
			}
		}
	}
	if json.Unmarshal(output, &pr) != nil || pr.URL != *run.PRURL || pr.State != "open" || pr.Head.SHA != head || pr.Head.Ref != run.Branch || pr.Head.Repo.FullName != github.RepoSlug(repo.PushURL()) || pr.Base.Repo.FullName != github.RepoSlug(repo.UpstreamURL) || pr.Base.Ref == "" {
		return fmt.Errorf("terminal PR identity or public head changed")
	}
	base := strings.TrimSpace(repo.DefaultBranch)
	if base == "" {
		base = "main"
	}
	if cfg != nil && strings.TrimSpace(cfg.PR.BaseBranch) != "" {
		base = strings.TrimSpace(cfg.PR.BaseBranch)
	}
	if run.PRBaseBranch != nil && strings.TrimSpace(*run.PRBaseBranch) != "" {
		base = strings.TrimSpace(*run.PRBaseBranch)
	}
	if pr.Base.Ref != base {
		return fmt.Errorf("live terminal PR base differs from the fresh validation base")
	}
	confirmed, err := git.LsRemote(ctx, workDir, repo.PushURL(), *run.PushRef)
	if err != nil || confirmed != head {
		return fmt.Errorf("public head changed during terminal PR verification")
	}
	return nil
}
