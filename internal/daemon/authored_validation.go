package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

var authoredPRCommandOutput = (*exec.Cmd).Output

func (m *RunManager) validateAuthored(ctx context.Context, params ipc.ValidateAuthoredParams, operator int) (*ipc.ValidateAuthoredResult, error) {
	request := params.Request
	if len(request.SourceAnchors) == 0 {
		request.SourceAnchors = nil
	}
	if operator <= 0 {
		return nil, fmt.Errorf("authored admission requires an authenticated local operator")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var result *ipc.ValidateAuthoredResult
	_, err := m.withBranchLock(request.RepoID, request.Branch, func() (string, error) {
		if m.shuttingDown.Load() {
			return "", fmt.Errorf("daemon is shutting down")
		}
		snapshot, err := m.db.ReadTerminalSnapshot(request.PredecessorID)
		if err != nil {
			return "", fmt.Errorf("read terminal predecessor: %w", err)
		}
		if err := snapshot.ValidateAuthoredRequest(request); err != nil {
			return "", err
		}
		repo, err := m.db.GetRepo(snapshot.Run.RepoID)
		if err != nil {
			return "", err
		}
		if repo == nil {
			return "", fmt.Errorf("predecessor repository is no longer registered")
		}
		if err := proveAuthoredCheckout(ctx, params.CallerDir, &request, repo); err != nil {
			return "", err
		}
		gateDir := m.paths.RepoDir(repo.ID)
		anchors, err := branchsync.TerminalValidationAnchors(ctx, m.db, gateDir, request.CallerDir, snapshot.Run.ID)
		if err != nil {
			return "", err
		}
		if request.TerminalGeneration != "" && !maps.Equal(anchors, request.SourceAnchors) {
			return "", fmt.Errorf("terminal recovery anchors changed; inspect and bind every known anchor")
		}
		m.mu.Lock()
		isPredecessorScheduled := m.runScheduledLocked(snapshot.Run.ID)
		m.mu.Unlock()
		if isPredecessorScheduled || !snapshot.Run.Status.Terminal() || snapshot.Run.PushActive {
			return "", fmt.Errorf("predecessor must be terminal with no executor or publisher; it will never be restarted")
		}
		for _, step := range snapshot.Steps {
			if step.AgentPID != nil && *step.AgentPID != 0 {
				isRunning, err := processRunning(*step.AgentPID)
				if err != nil || isRunning {
					return "", fmt.Errorf("predecessor agent %d is still running or cannot be inspected", *step.AgentPID)
				}
			}
		}
		event, err := m.db.GetValidationAdmission(snapshot.Run.ID)
		if err != nil {
			return "", err
		}
		isNewAdmission := event == nil
		runs, err := m.db.GetRunsByRepo(repo.ID)
		if err != nil {
			return "", err
		}
		for _, run := range runs {
			if run.Branch != request.Branch || run.ID == snapshot.Run.ID || event != nil && run.ID == event.ChildID {
				continue
			}
			m.mu.Lock()
			isRunScheduled := m.runScheduledLocked(run.ID)
			m.mu.Unlock()
			if !run.Status.Terminal() || run.ID > snapshot.Run.ID || isRunScheduled {
				return "", fmt.Errorf("a newer or active run owns this branch; authored admission cannot replace it")
			}
		}
		if event != nil {
			if !reflect.DeepEqual(event.Request, request) {
				return "", fmt.Errorf("predecessor already has a different authored candidate, controller policy assertion or launch binding")
			}
			child, err := m.db.GetRun(event.ChildID)
			if err != nil || child == nil {
				return "", fmt.Errorf("accepted authored child is unavailable")
			}
			family, err := m.db.ValidationFamily(child.ID)
			if err != nil {
				return "", err
			}
			if family == nil || family.HasChildRepairs {
				return "", fmt.Errorf("validation-only child has conflicting repair history")
			}
			receipt, err := receiptForRun(child, false)
			if err != nil {
				return "", err
			}
			result = &ipc.ValidateAuthoredResult{Receipt: receipt, Family: family}
			// A scheduled child stays pending until its executor starts, so a
			// child that holds a slot or is queued for one is already owned and
			// a replay only observes.
			m.mu.Lock()
			isChildScheduled := m.runScheduledLocked(child.ID)
			m.mu.Unlock()
			if child.Status != types.RunPending || isChildScheduled {
				return child.ID, nil
			}
			if err := m.db.ValidatePendingAdmission(event); err != nil {
				return "", err
			}
		}
		if m.restartPending() {
			return "", errRestartPending
		}
		global, err := config.LoadGlobal(m.paths.ConfigFile())
		if err != nil {
			return "", err
		}
		layout, err := validatedWorktreeLayout(m.db, m.paths, global)
		if err != nil {
			return "", err
		}
		if symbolic, err := git.Run(ctx, gateDir, "symbolic-ref", "-q", "refs/heads/"+request.Branch); err == nil && symbolic != "" {
			return "", fmt.Errorf("gate branch is symbolic")
		}
		gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+request.Branch+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("read existing gate branch: %w", err)
		}
		if _, err := m.db.ValidationFamily(snapshot.Run.ID); err != nil {
			return "", err
		}
		isBoundGateHead := false
		history, err := m.db.ValidationHistory(snapshot.Run.ID)
		if err != nil {
			return "", err
		}
		for _, entry := range history {
			historical := entry.Snapshot
			heads := []*string{&historical.Run.HeadSHA, historical.Run.SubmittedHeadSHA, historical.Run.LastPushedSHA}
			if entry.Adoption != nil {
				heads = append(heads, &entry.Adoption.GateHead)
			}
			for _, rounds := range historical.Rounds {
				for _, round := range rounds {
					heads = append(heads, round.StartingHeadSHA, round.ReviewedHeadSHA)
				}
			}
			for _, head := range heads {
				if head != nil && gateHead == *head {
					isBoundGateHead = true
				}
			}
		}
		if !isBoundGateHead {
			return "", fmt.Errorf("gate branch moved outside the immutable predecessor lineage; reconcile its owner before admission")
		}
		// Import only the exact object, without changing a branch, tracking ref or FETCH_HEAD.
		if _, err := git.Run(ctx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", "--refmap=", request.CallerDir, request.CandidateSHA); err != nil {
			return "", fmt.Errorf("read exact authored commit from caller: %w", err)
		}
		candidateConfig := &config.RepoConfig{}
		entry, err := git.Run(ctx, gateDir, "ls-tree", request.CandidateSHA, "--", ".no-mistakes.yaml")
		if err != nil {
			return "", err
		}
		if entry != "" {
			content, err := git.ShowFile(ctx, gateDir, request.CandidateSHA, ".no-mistakes.yaml")
			if err != nil {
				return "", err
			}
			candidateConfig, err = config.LoadRepoFromBytes([]byte(content))
			if err != nil {
				return "", err
			}
		}
		// Like an ordinary launch, publish to the registration verified against the
		// working clone, so the child binds the target a later admission proves.
		if refreshed, _, refreshErr := gate.RefreshRepoURLs(ctx, m.db, repo); refreshErr != nil {
			slog.Warn("repository URL refresh skipped; continuing with existing registration", "repo_id", repo.ID, "reason", gate.ReasonForRefreshFailure(refreshErr))
		} else {
			repo = refreshed
		}
		// Init records the gate's origin as the clone resolves it through
		// url.*.insteadOf, while the registry keeps the clone's literal origin.
		sourceURLs, err := git.GetConfiguredRemoteURLs(ctx, gateDir, "origin")
		if err != nil || len(sourceURLs) != 1 {
			return "", fmt.Errorf("trusted source repository differs from the registered repository")
		}
		registeredSource, err := git.Run(ctx, repo.WorkingPath, "ls-remote", "--get-url", repo.UpstreamURL)
		if err != nil || safeurl.Redact(sourceURLs[0]) != repo.UpstreamURL && safeurl.Redact(sourceURLs[0]) != safeurl.Redact(registeredSource) {
			return "", fmt.Errorf("trusted source repository differs from the registered repository")
		}
		trustedRef := fmt.Sprintf("refs/no-mistakes/authored-trusted/%d-%d", os.Getpid(), time.Now().UnixNano())
		defer func() {
			cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cleanupCancel()
			_, _ = git.Run(cleanupContext, gateDir, "update-ref", "--no-deref", "-d", trustedRef)
		}()
		if err := git.FetchRemoteBranchToPrivateRef(ctx, gateDir, sourceURLs[0], repo.DefaultBranch, trustedRef); err != nil {
			return "", fmt.Errorf("fetch bound trusted source: %w", err)
		}
		trustedSHA, err := git.ResolveRef(ctx, gateDir, trustedRef)
		if err != nil {
			return "", err
		}
		pin := snapshot.Run.PiProfile
		storedQuota := snapshot.Run.QuotaRouting
		if request.TerminalGeneration != "" {
			pin, storedQuota = nil, ""
		}
		cfg, _, _, err := m.loadRunConfig(ctx, repo, global, candidateConfig, gateDir, trustedSHA, "", request.Branch, pin)
		if err != nil {
			return "", err
		}
		cfg.AutoFix, cfg.SideBySide = config.AutoFix{}, false
		if err := applyLaunchSelection(cfg, request.LaunchAssertion); err != nil {
			return "", err
		}
		roles, proof, err := prepareLaunchAssertion(ctx, request.LaunchAssertion, cfg, exec.LookPath)
		if err != nil {
			return "", err
		}
		executionSteps := steps.WithCustomGates(m.steps(), cfg.Gates)
		stepNames := make([]types.StepName, len(executionSteps))
		for index, step := range executionSteps {
			stepNames[index] = step.Name()
		}
		gates, err := config.MarshalGates(cfg.Gates)
		if err != nil {
			return "", err
		}
		forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
		if err != nil {
			return "", err
		}
		if err := proveAuthoredPublication(ctx, &snapshot.Run, repo, request.CallerDir, cfg, forge); err != nil {
			return "", err
		}
		quotaRecord, err := replayQuotaRouting(cfg, storedQuota)
		if err != nil {
			return "", err
		}
		// Agent construction holds fail-closed launch checks, so it comes before
		// acceptance and a refused launch records nothing. A new child has no
		// directory yet, so its agent is steered to the root it will go under.
		var evidenceRoot string
		if event != nil {
			evidenceRoot = pipelineEvidenceRoot(m.paths, cfg, &event.InitialChild)
		} else if evidenceRoot, err = m.newRunEvidenceRoot(global); err != nil {
			return "", err
		}
		ag, _, err := newResolvedPipelineAgent(ctx, cfg, roles, evidenceRoot, forgeEnvironment(forge), quotaRecord)
		if err != nil {
			return "", err
		}
		isScheduled := false
		defer func() {
			if !isScheduled {
				_ = ag.Close()
			}
		}()
		if err := proveAuthoredCheckout(ctx, params.CallerDir, &request, repo); err != nil {
			return "", err
		}
		confirmedGateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+request.Branch+"^{commit}")
		if err != nil || confirmedGateHead != gateHead {
			return "", fmt.Errorf("gate branch changed during authored admission")
		}
		if err := proveAuthoredPublication(ctx, &snapshot.Run, repo, request.CallerDir, cfg, forge); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		confirmedAnchors, err := branchsync.TerminalValidationAnchors(ctx, m.db, gateDir, request.CallerDir, snapshot.Run.ID)
		if err != nil || !maps.Equal(anchors, confirmedAnchors) {
			return "", fmt.Errorf("terminal recovery anchors changed before acceptance: %v", err)
		}
		if event == nil {
			var plan *verificationplan.Snapshot
			if snapshot.Run.VerificationPlan != nil {
				if _, err := snapshot.Run.VerificationPlan.Read(); err != nil {
					return "", err
				}
				plan, err = verificationplan.Capture(m.paths.RunInputsDir(), snapshot.Run.VerificationPlan.Path, repo.ID, request.Branch, request.CandidateSHA)
				if err != nil {
					return "", err
				}
			}
			worktreeRoot := filepath.Dir(layout.Dir(repo.ID, repo.WorkingPath, "pending"))
			event, _, err = m.db.AcceptValidationAdmission(ctx, db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: worktreeRoot, EvidenceRoot: evidenceRoot, Plan: plan, Gates: gates, Steps: stepNames, Operator: strconv.Itoa(operator)})
			if err != nil {
				return "", err
			}
		} else if event.Gates != gates || !reflect.DeepEqual(event.Steps, stepNames) || !samePath(event.WorktreeRoot, filepath.Dir(layout.Dir(repo.ID, repo.WorkingPath, event.ChildID))) {
			return "", fmt.Errorf("accepted authored stage sequence or worktree placement changed")
		}
		child, err := m.db.GetRun(event.ChildID)
		if err != nil || child == nil || child.WorktreeDir == nil {
			return "", fmt.Errorf("accepted authored child has no registered worktree")
		}
		if err := m.db.ValidatePendingAdmission(event); err != nil {
			return "", err
		}
		if err := custody.PreserveRecoveryAnchor(ctx, gateDir, "refs/no-mistakes/authored-candidate/"+event.ID, request.CandidateSHA); err != nil {
			return "", err
		}
		workDir := *child.WorktreeDir
		if _, err := os.Stat(workDir); os.IsNotExist(err) {
			if err := git.WorktreeAdd(ctx, gateDir, workDir, request.CandidateSHA); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, workDir); err != nil {
			return "", err
		}
		if err := pipeline.ValidateAuthoredCandidate(ctx, m.db, child, workDir, executionSteps, event); err != nil {
			return "", err
		}
		family, err := m.db.ValidationFamily(child.ID)
		if err != nil {
			return "", err
		}
		receipt, err := receiptForRun(child, isNewAdmission)
		if err != nil {
			return "", err
		}
		result = &ipc.ValidateAuthoredResult{Receipt: receipt, Family: family}
		if ctx.Err() != nil {
			return child.ID, nil
		}
		m.resumeRecoveredRun(recoveredRunPlan{run: child, repo: repo, workDir: workDir, gateDir: gateDir, cfg: cfg, agent: ag, steps: executionSteps, forge: forge, admission: event})
		isScheduled, result.Started = true, true
		return child.ID, nil
	})
	return result, err
}

func proveAuthoredCheckout(ctx context.Context, actualDir string, request *authoredvalidation.Request, repo *db.Repo) error {
	if !filepath.IsAbs(actualDir) || !filepath.IsAbs(request.CallerDir) {
		return fmt.Errorf("authored caller must identify an absolute canonical checkout")
	}
	canonical, err := filepath.EvalSymlinks(actualDir)
	if err != nil || !samePath(canonical, request.CallerDir) {
		return fmt.Errorf("invoking checkout differs from the declared authored source")
	}
	if filepath.Clean(request.CallerDir) != canonical {
		return fmt.Errorf("declared caller_dir must be the canonical authored checkout path")
	}
	top, err := git.Run(ctx, canonical, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil || !samePath(top, canonical) {
		return fmt.Errorf("authored caller must be its canonical repository root")
	}
	common, err := git.Run(ctx, canonical, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	registered, err := git.Run(ctx, repo.WorkingPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	common, err = filepath.EvalSymlinks(resolveGitPath(canonical, common))
	if err != nil {
		return err
	}
	registered, err = filepath.EvalSymlinks(resolveGitPath(repo.WorkingPath, registered))
	if err != nil || !samePath(common, registered) {
		return fmt.Errorf("authored checkout does not belong to the registered repository")
	}
	branch, err := git.CurrentBranch(ctx, canonical)
	if err != nil || branch != request.Branch {
		return fmt.Errorf("authored checkout branch changed or is detached")
	}
	head, err := git.HeadSHA(ctx, canonical)
	if err != nil || head != request.CandidateSHA {
		return fmt.Errorf("authored checkout HEAD differs from the declared exact commit")
	}
	tree, err := git.Run(ctx, canonical, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != request.CandidateTree {
		return fmt.Errorf("authored checkout tree differs from the declared exact tree")
	}
	status, err := git.Run(ctx, canonical, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil || status != "" {
		return fmt.Errorf("authored checkout must be clean, including staged, tracked, untracked and submodule changes")
	}
	return nil
}

func proveAuthoredPublication(ctx context.Context, run *db.Run, repo *db.Repo, caller string, cfg *config.Config, forge *forgecontext.Context) error {
	if run.LastPushedSHA == nil {
		if run.PRURL != nil || run.PushGeneration != nil {
			return fmt.Errorf("terminal publication binding is incomplete")
		}
		return nil
	}
	kind := "upstream"
	if repo.ForkURL != "" {
		kind = "fork"
	}
	if run.PushGeneration == nil || *run.PushGeneration <= 0 || run.PushTargetKind == nil || *run.PushTargetKind != kind || run.PushTargetFingerprint == nil || *run.PushTargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) || run.PushRef == nil || *run.PushRef != "refs/heads/"+run.Branch || run.LastPushedAt == nil || run.PRURL == nil || run.PRState == nil || *run.PRState != "open" {
		return fmt.Errorf("terminal publication repository, target, ref or generation is incomplete or changed")
	}
	ctx = git.WithEnvironment(ctx, forgeEnvironment(forge))
	head, err := git.LsRemote(ctx, caller, repo.PushURL(), *run.PushRef)
	if err != nil || head != *run.LastPushedSHA {
		return fmt.Errorf("bound public head moved or cannot be read")
	}
	if _, err := git.Run(ctx, caller, "merge-base", "--is-ancestor", head, "HEAD"); err != nil {
		return fmt.Errorf("authored candidate must retain the bound published history")
	}
	if scm.DetectProviderStaticContext(ctx, repo.UpstreamURL) != scm.ProviderGitHub {
		return fmt.Errorf("published authored admission requires verifiable GitHub PR identity")
	}
	number, err := scm.ExtractPRNumber(*run.PRURL)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "gh", "api", "--hostname", scm.ResolveHost(ctx, repo.UpstreamURL), "repos/"+github.RepoSlug(repo.UpstreamURL)+"/pulls/"+number)
	command.Dir, command.Env = caller, forgeEnvironment(forge).Apply(nil)
	winproc.Harden(command)
	output, err := authoredPRCommandOutput(command)
	if err != nil {
		return fmt.Errorf("cannot verify the bound terminal PR")
	}
	var pr struct {
		URL   string `json:"html_url"`
		State string `json:"state"`
		Head  struct {
			SHA, Ref string
			Repo     struct {
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
	if json.Unmarshal(output, &pr) != nil || pr.URL != *run.PRURL || pr.State != "open" || pr.Head.SHA != head || pr.Head.Ref != run.Branch || pr.Head.Repo.FullName != github.RepoSlug(repo.PushURL()) || pr.Base.Repo.FullName != github.RepoSlug(repo.UpstreamURL) {
		return fmt.Errorf("terminal PR repository, branch, state or public head changed")
	}
	base := strings.TrimSpace(repo.DefaultBranch)
	if cfg.PR.BaseBranch != "" {
		base = cfg.PR.BaseBranch
	}
	if run.PRBaseBranch != nil && *run.PRBaseBranch != "" {
		base = *run.PRBaseBranch
	}
	if pr.Base.Ref != base {
		return fmt.Errorf("live terminal PR base differs from the fresh validation base")
	}
	confirmed, err := git.LsRemote(ctx, caller, repo.PushURL(), *run.PushRef)
	if err != nil || confirmed != head {
		return fmt.Errorf("bound public head changed during PR verification")
	}
	return nil
}
