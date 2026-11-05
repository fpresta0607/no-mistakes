package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

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
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
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
		// Validation-only authored lineage never regains repair authority.
		if err := m.db.RefuseUnlinkedValidation(request.RepoID, request.Branch); err != nil {
			return "", err
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
		event, err := m.db.GetTerminalAdoption(request.RunID)
		if err != nil {
			return "", err
		}
		if selected == nil || selected.ID != request.RunID && (event == nil || selected.ID != event.Successor.ID) {
			return "", fmt.Errorf("terminal request no longer identifies this branch's run")
		}
		snapshot, err := m.db.ReadContinuationSnapshot(request.RunID)
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
		original := snapshot
		if event != nil {
			original = &event.Snapshot
		}
		if request.SnapshotDigest != original.Digest() {
			return "", fmt.Errorf("terminal run, generation, receipt or findings changed since observation")
		}
		m.mu.Lock()
		executor := m.executors[original.Run.ID]
		isChildScheduled := event != nil && m.runScheduledLocked(event.Successor.ID)
		m.mu.Unlock()
		if m.restartPending() && !isChildScheduled {
			return "", errRestartPending
		}
		if executor != nil || !original.Run.Status.Terminal() || original.Run.Status == types.RunCompleted {
			return "", fmt.Errorf("only a terminal run without an executor can adopt source")
		}
		if event != nil {
			if err := m.db.ValidateTerminalSuccessor(event); err != nil {
				return "", err
			}

		}
		if err := validateTerminalAgentInactivity(snapshot); err != nil {
			return "", err
		}
		service := &branchsync.Service{DB: m.db, Repo: repo, WorkDir: request.CallerDir, GateDir: m.paths.RepoDir(repo.ID), Paths: m.paths}
		var source *branchsync.TerminalSource
		if event == nil {
			source, err = service.TerminalSource(ctx, &original.Run)
		} else {
			source, err = service.AcceptedTerminalSource(ctx, event, &event.Successor)
		}
		if err != nil {
			return "", err
		}
		if event != nil && (source.Head != event.CandidateSHA || source.Tree != event.CandidateTree || source.GateHead != event.GateHead) {
			return "", fmt.Errorf("accepted source or gate lane changed")
		}
		successorID := ulid.Make().String()
		var plan *verificationplan.Snapshot
		if event != nil {
			successorID, plan = event.Successor.ID, event.Plan
		} else if original.Run.VerificationPlan != nil {
			if _, err := original.Run.VerificationPlan.Read(); err != nil {
				return "", err
			}
			plan, err = verificationplan.Capture(m.paths.RunInputsDir(), original.Run.VerificationPlan.Path, repo.ID, original.Run.Branch, source.Head)
			if err != nil {
				return "", err
			}
			successorID = plan.ID
		}
		globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
		if err != nil {
			return "", err
		}
		layout, err := validatedWorktreeLayout(m.db, m.paths, globalCfg)
		if err != nil {
			return "", err
		}
		workDir := layout.Dir(repo.ID, repo.WorkingPath, successorID)
		if event != nil && (event.Successor.WorktreeDir == nil || !samePath(workDir, *event.Successor.WorktreeDir)) {
			return "", fmt.Errorf("successor worktree placement changed")
		}
		candidateRef := "refs/no-mistakes/terminal-candidate/" + successorID
		if _, err := git.Run(ctx, service.GateDir, "cat-file", "-e", source.Head+"^{commit}"); err != nil {
			if _, err := git.Run(ctx, service.GateDir, "fetch", "--no-tags", request.CallerDir, source.Head); err != nil {
				return "", fmt.Errorf("preserve exact caller candidate: %w", err)
			}
		}

		candidateConfig := &config.RepoConfig{}
		entry, err := git.Run(ctx, service.GateDir, "ls-tree", source.Head, "--", ".no-mistakes.yaml")
		if err != nil {
			return "", fmt.Errorf("read candidate config tree: %w", err)
		}
		if entry != "" {
			content, err := git.ShowFile(ctx, service.GateDir, source.Head, ".no-mistakes.yaml")
			if err != nil {
				return "", fmt.Errorf("read candidate config: %w", err)
			}
			candidateConfig, err = config.LoadRepoFromBytes([]byte(content))
			if err != nil {
				return "", err
			}
		}
		cfg, err := m.loadRecoveredConfigWithRepo(ctx, &original.Run, repo, service.GateDir, candidateConfig)
		if err != nil {
			return "", err
		}
		execSteps := steps.WithCustomGates(m.steps(), cfg.Gates)
		originalGates, err := m.pinnedRunGates(original.Run.ID)
		if err != nil {
			return "", err
		}
		if err := validateTerminalSteps(original, steps.WithCustomGates(m.steps(), originalGates)); err != nil {
			return "", err
		}
		gates, err := config.MarshalGates(cfg.Gates)
		if err != nil {
			return "", err
		}
		stepNames := make([]types.StepName, 0, len(execSteps))
		for _, step := range execSteps {
			stepNames = append(stepNames, step.Name())
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
		if err := proveTerminalPublication(ctx, &original.Run, repo, request.CallerDir, forge, cfg); err != nil {
			return "", err
		}
		quotaRecord, err := replayQuotaRouting(cfg, original.Run.QuotaRouting)
		if err != nil {
			return "", err
		}
		callerHead, err := git.HeadSHA(ctx, request.CallerDir)
		if err != nil {
			return "", err
		}
		var rechecked *branchsync.TerminalSource
		if event == nil {
			rechecked, err = service.TerminalSource(ctx, &original.Run)
		} else {
			rechecked, err = service.AcceptedTerminalSource(ctx, event, &event.Successor)
		}
		if err != nil || !reflect.DeepEqual(source, rechecked) {
			return "", fmt.Errorf("candidate source changed during verification")
		}
		for _, anchor := range []struct{ ref, head string }{{candidateRef, source.Head}, {"refs/no-mistakes/terminal-lane/" + successorID, source.GateHead}} {
			if err := custody.PreserveRecoveryAnchor(ctx, service.GateDir, anchor.ref, anchor.head); err != nil {
				return "", err
			}
		}
		if event == nil {
			evidenceRoot, err := m.newRunEvidenceRoot(globalCfg)
			if err != nil {
				return "", err
			}
			event, _, err = m.db.AdoptTerminalCandidate(ctx, db.TerminalAdoption{Snapshot: *original, SuccessorID: successorID, WorktreeRoot: filepath.Dir(workDir), EvidenceRoot: evidenceRoot, Plan: plan, Gates: gates, StepNames: stepNames, CandidateSHA: source.Head, RetainedSHA: source.RetainedHead, CandidateTree: source.Tree, SourceRef: candidateRef, GateHead: source.GateHead, RecoveryHead: source.RecoveryHead, CallerDir: request.CallerDir, CallerHead: callerHead, ManagedDir: source.ManagedDir, Reason: request.Reason, Operator: strconv.Itoa(operator), Assertion: assertion, Proof: proof})
			if err != nil {
				return "", err
			}
		} else {
			if request.Reason != event.Reason || request.CallerDir != event.CallerDir || callerHead != event.CallerHead || gates != event.Gates || !assertion.Matches(event.Assertion) || proof.Check(event.Assertion) != nil {
				return "", fmt.Errorf("accepted source, reason or trusted launch provenance changed")
			}
			if err := m.db.ValidateTerminalSuccessor(event); err != nil {
				return "", err
			}
		}
		fresh, err := m.db.GetRun(event.Successor.ID)
		if err != nil {
			return "", err
		}
		if err := pipeline.ValidateTerminalSuccessor(m.db, fresh, execSteps, event); err != nil {
			return "", err
		}
		if _, err := os.Stat(workDir); os.IsNotExist(err) {
			if err := git.WorktreeAdd(ctx, service.GateDir, workDir, source.Head); err != nil {
				return "", fmt.Errorf("create registered successor checkout: %w", err)
			}
		} else if err != nil {
			return "", err
		}
		if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, workDir); err != nil {
			return "", fmt.Errorf("configure successor identity: %w", err)
		}
		result = &ipc.RecoverTerminalResult{RunID: fresh.ID, PredecessorID: original.Run.ID, CandidateSHA: event.CandidateSHA, CandidateTree: event.CandidateTree, AdoptionID: event.ID}
		result.PhaseBudgets = make(map[string]db.TerminalPhaseBudget)
		for _, step := range execSteps {
			budget, _, err := m.db.TerminalPhaseRemaining(fresh.ID, step.Name(), cfg.AutoFixLimit(step.Name()))
			if err != nil {
				return "", err
			}
			result.PhaseBudgets[string(step.Name())] = budget
		}
		if ctx.Err() != nil || isChildScheduled {
			return fresh.ID, nil
		}
		// The agent is built only now, because the accepted successor names the
		// directory its evidence goes under.
		ag, _, err := newResolvedPipelineAgent(ctx, cfg, roleConfigs, pipelineEvidenceRoot(m.paths, cfg, fresh), forgeEnvironment(forge), quotaRecord)
		if err != nil {
			return "", err
		}
		m.subMu.Lock()
		delete(m.completedRuns, fresh.ID)
		m.subMu.Unlock()
		m.resumeRecoveredRun(recoveredRunPlan{run: fresh, repo: repo, workDir: workDir, gateDir: service.GateDir, cfg: cfg, agent: ag, steps: execSteps, forge: forge, terminal: event})
		result.Started = true
		return fresh.ID, nil
	})
	return result, err
}

func validateTerminalAgentInactivity(snapshot *db.ContinuationSnapshot) error {
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

func validateTerminalSteps(snapshot *db.ContinuationSnapshot, sequence []pipeline.Step) error {
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

func freezeTerminalLimits(cfg *config.Config, snapshot *db.ContinuationSnapshot) {
	for _, step := range snapshot.Steps {
		if step.StartedAt == nil {
			continue
		}
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
