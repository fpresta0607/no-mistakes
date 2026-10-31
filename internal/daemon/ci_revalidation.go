package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
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
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

func (m *RunManager) ciRepairHeld(ctx context.Context, run *db.Run) (bool, error) {
	event, err := m.db.GetCIRepairAdoption(run.ID)
	if err != nil {
		return true, err
	}
	if held, err := pipeline.CIAdoptionHeld(m.db, run, event); held || err != nil {
		return held, err
	}
	return pipeline.RetainedCIRepairHeld(m.db, ctx, run, worktrees.RecordedDir(m.paths, run.WorktreePath(), run.RepoID, run.ID))
}

func (m *RunManager) cancelPreservedCIRepair(runID string) error {
	run, err := m.db.GetRun(runID)
	if err != nil || run == nil {
		return fmt.Errorf("cannot read preserved run %s", runID)
	}
	_, err = m.withBranchLock(run.RepoID, run.Branch, func() (string, error) {
		m.mu.Lock()
		cancel := m.cancels[runID]
		m.mu.Unlock()
		if cancel != nil {
			cancel(fmt.Errorf(types.RunCancelReasonAbortedByUser))
			return runID, nil
		}
		run, err := m.db.GetRun(runID)
		if err != nil || run == nil || run.Status != types.RunRunning {
			return "", fmt.Errorf("no active run %s", runID)
		}
		if held, err := m.ciRepairHeld(context.Background(), run); !held || err != nil {
			return "", fmt.Errorf("no exclusively preserved CI repair for run %s", runID)
		}
		repo, err := m.db.GetRepo(run.RepoID)
		if err != nil || repo == nil {
			return "", fmt.Errorf("cannot read preserved repository")
		}
		global, err := config.LoadGlobal(m.paths.ConfigFile())
		if err != nil {
			return "", err
		}
		cfg := config.Merge(global, &config.RepoConfig{})
		if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
			return "", err
		}
		workDir := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
		m.sweepRunWorktreeProcesses(repo.ID, runID, workDir)
		head, verified := preserveRunHead(m.db, workDir, run)
		if !verified {
			return "", fmt.Errorf("cannot preserve the adopted source before cancellation")
		}
		if err := m.db.ClearRunAwaitingAgent(runID); err != nil {
			return "", err
		}
		if err := m.db.UpdateRunErrorStatusWithVerifiedHead(runID, types.RunCancelReasonAbortedByUser, types.RunCancelled, head); err != nil {
			return "", err
		}
		status, reason := string(types.RunCancelled), types.RunCancelReasonAbortedByUser
		m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: runID, RepoID: repo.ID, Branch: &run.Branch, Status: &status, Error: &reason, PRURL: run.PRURL})
		m.closeSubscribers(runID)
		m.removeRunWorktree(repo.ID, runID, m.paths.RepoDir(repo.ID), workDir, "preserved_adoption_cancelled")
		m.cleanupRunEvidence(cfg, runID)
		return runID, nil
	})
	return err
}

func (m *RunManager) refuseCIRepairReplacement(ctx context.Context, repoID, branch string) error {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		return fmt.Errorf("cannot read active CI ownership; preserve existing runs")
	}
	for _, run := range runs {
		if run.RepoID == repoID && run.Branch == branch {
			if held, err := m.ciRepairHeld(ctx, run); held || err != nil {
				return fmt.Errorf("run %s retains unpublished CI contents; replacement is refused", run.ID)
			}
		}
	}
	return nil
}

func (m *RunManager) HandleCIRevalidation(ctx context.Context, params ipc.RespondParams) error {
	request := params.CIRevalidation
	if params.Action != types.ActionRevalidate || request == nil || params.Step != types.StepCI || len(params.FindingIDs) != 0 || len(params.Instructions) != 0 || len(params.AddedFindings) != 0 || params.ApprovalReason != "" {
		return fmt.Errorf("CI revalidation needs an exact candidate, snapshot, reason, caller and explicit publication credentials")
	}
	if ipc.PeerPID(ctx) <= 0 {
		return fmt.Errorf("cannot authenticate the local operator process")
	}
	if strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 || !filepath.IsAbs(request.CallerDir) {
		return fmt.Errorf("CI revalidation requires an absolute caller directory and a bounded operator reason")
	}
	run, err := m.db.GetRun(params.RunID)
	if err != nil || run == nil {
		return fmt.Errorf("cannot read CI revalidation run")
	}
	_, err = m.withBranchLock(run.RepoID, run.Branch, func() (string, error) {
		if m.shuttingDown.Load() {
			return "", fmt.Errorf("daemon is shutting down")
		}
		proofCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		event, err := m.db.GetCIRepairAdoption(run.ID)
		if err != nil {
			return "", err
		}
		if event != nil && !request.ReselectPublication {
			digest, err := pipeline.CIRepairSnapshotDigest(event)
			if err != nil || digest != request.SnapshotDigest || event.CandidateSHA != request.CandidateSHA || event.Reason != request.Reason || event.CallerDir != request.CallerDir || event.CallerHeadSHA != request.CallerHeadSHA {
				return "", fmt.Errorf("CI adoption already has a different binding; no validation was reset")
			}
			return run.ID, nil
		}
		snapshot, err := m.db.ReadCIRepairSnapshot(run.ID)
		if err != nil {
			return "", err
		}
		digest, err := pipeline.CIRepairSnapshotDigest(snapshot)
		if err != nil || digest != request.SnapshotDigest {
			return "", fmt.Errorf("CI revalidation snapshot changed")
		}
		if snapshot.ExpectedRun.Status != types.RunRunning {
			return "", fmt.Errorf("CI revalidation refuses a terminal or replaced run")
		}
		activeRuns, err := m.db.GetActiveRuns()
		if err != nil {
			return "", fmt.Errorf("cannot read competing CI ownership")
		}
		for _, active := range activeRuns {
			if active.RepoID == run.RepoID && active.Branch == run.Branch && active.ID != run.ID {
				return "", fmt.Errorf("another active run owns this CI branch")
			}
		}
		for _, current := range snapshot.Steps {
			if current.Snapshot.AgentPID != nil && *current.Snapshot.AgentPID != 0 {
				return "", fmt.Errorf("an agent still owns the CI worktree")
			}
			if request.ReselectPublication && current.Snapshot.StepName == types.StepPush && current.Snapshot.FindingsJSON != nil {
				findings, err := types.ParseFindingsJSON(*current.Snapshot.FindingsJSON)
				if err != nil {
					return "", fmt.Errorf("publication gate history is unreadable")
				}
				for _, finding := range findings.Items {
					if finding.ID == pipeline.PublicationRevalidationFindingID {
						return "", fmt.Errorf("fresh validation is required; publication selection cannot reset it")
					}
				}
			}
		}
		if !request.ReselectPublication {
			for _, original := range snapshot.Steps {
				if original.Snapshot.StepName == types.StepCI && original.Snapshot.FindingsJSON != nil && pipeline.HasProtectedPathRefusal(*original.Snapshot.FindingsJSON) {
					return "", fmt.Errorf("CI adoption cannot consume a protected-path refusal")
				}
			}
		}
		selectPublication := func() (*forgecontext.Context, error) {
			publication, err := forgecontext.ResolvePublication(proofCtx, snapshot.ExpectedRepo.UpstreamURL, snapshot.ExpectedRepo.ForkURL, forgecontext.PublicationSelection{ConfigDir: request.GHConfigDir, ExpectedLogin: request.ExpectedLogin})
			if err != nil {
				return nil, err
			}
			if err := forgecontext.VerifyPublication(proofCtx, request.CallerDir, publication, request.ExpectedLogin); err != nil {
				return nil, err
			}
			if err := m.proveCIRepairGit(proofCtx, snapshot, request, request.CandidateSHA, publication); err != nil {
				return nil, err
			}
			return publication, nil
		}
		if request.ReselectPublication {
			if event == nil || event.CandidateSHA != request.CandidateSHA {
				return "", fmt.Errorf("reselection requires the exact accepted adoption")
			}
			if held, err := m.ciRepairHeld(proofCtx, &snapshot.ExpectedRun); !held || err != nil {
				return "", fmt.Errorf("adopted publication is no longer held")
			}
			m.mu.Lock()
			executor := m.executors[run.ID]
			m.mu.Unlock()
			if executor != nil {
				return run.ID, executor.ReselectPublication(snapshot, selectPublication, request.ExpectedLogin)
			}
			if err := m.db.CompareCIRepairSnapshot(snapshot); err != nil {
				return "", err
			}
			publication, err := selectPublication()
			if err != nil {
				return "", err
			}
			plan, err := m.prepareCIAdoption(proofCtx, &snapshot.ExpectedRun, event, publication, request.ExpectedLogin)
			if err != nil {
				return "", err
			}
			if err := m.db.CompareCIRepairSnapshot(snapshot); err != nil {
				_ = plan.agent.Close()
				return "", err
			}
			plan.publicationSnapshot = snapshot
			m.resumeRecoveredRun(*plan)
			return run.ID, nil
		}
		publication, err := forgecontext.ResolvePublication(proofCtx, snapshot.ExpectedRepo.UpstreamURL, snapshot.ExpectedRepo.ForkURL, forgecontext.PublicationSelection{ConfigDir: request.GHConfigDir, ExpectedLogin: request.ExpectedLogin})
		if err != nil {
			return "", err
		}
		if err := forgecontext.VerifyPublication(proofCtx, request.CallerDir, publication, request.ExpectedLogin); err != nil {
			return "", err
		}

		snapshot.CandidateSHA, snapshot.CallerDir, snapshot.CallerHeadSHA = request.CandidateSHA, request.CallerDir, request.CallerHeadSHA
		snapshot.ManagedDir = worktrees.RecordedDir(m.paths, snapshot.ExpectedRun.WorktreePath(), run.RepoID, run.ID)
		snapshot.GateDir, snapshot.GateBranch = m.paths.RepoDir(run.RepoID), run.Branch
		snapshot.Operator, snapshot.Reason = fmt.Sprintf("authenticated local IPC process %d", ipc.PeerPID(ctx)), request.Reason
		adopt := func() (*db.CIRepairAdoption, error) {
			tree, err := git.Run(proofCtx, snapshot.ManagedDir, "rev-parse", "--verify", request.CandidateSHA+"^{tree}")
			if err != nil {
				return nil, fmt.Errorf("cannot read candidate tree")
			}
			snapshot.CandidateTreeSHA = tree
			if err := m.proveCIRepairGit(proofCtx, snapshot, request, request.CandidateSHA, publication); err != nil {
				return nil, err
			}
			accepted, _, err := m.db.AdoptCIRepairForRevalidation(*snapshot)
			return accepted, err
		}
		m.mu.Lock()
		executor := m.executors[run.ID]
		m.mu.Unlock()
		if executor != nil {
			return run.ID, executor.AdoptCIRepair(adopt, publication, request.ExpectedLogin)
		}
		accepted, err := adopt()
		if err != nil {
			return "", err
		}
		fresh, err := m.db.GetRun(run.ID)
		if err != nil {
			return "", fmt.Errorf("adoption is durable; cannot read fresh validation state")
		}
		plan, err := m.prepareCIAdoption(proofCtx, fresh, accepted, publication, request.ExpectedLogin)
		if err != nil {
			return "", fmt.Errorf("adoption is durable and preserved; fresh validation unavailable: %w", err)
		}
		m.resumeRecoveredRun(*plan)
		return run.ID, nil
	})
	return err
}

func (m *RunManager) proveCIRepairLocal(ctx context.Context, snapshot *db.CIRepairAdoption, request *ipc.CIRevalidationParams, candidate string) (string, error) {
	run, repo := &snapshot.ExpectedRun, &snapshot.ExpectedRepo
	managed := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
	gateDir := m.paths.RepoDir(repo.ID)
	if run.LastPushedSHA == nil || run.PushRef == nil || *run.PushRef != "refs/heads/"+run.Branch || run.PushTargetKind == nil || run.PushTargetFingerprint == nil {
		return "", fmt.Errorf("CI publication binding is incomplete")
	}
	kind := "upstream"
	if repo.ForkURL != "" {
		kind = "fork"
	}
	if *run.PushTargetKind != kind || *run.PushTargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) {
		return "", fmt.Errorf("CI publication target changed")
	}
	writerStates := []string{"index.lock", "HEAD.lock", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"}
	writerArguments := []string{"rev-parse"}
	for _, state := range writerStates {
		writerArguments = append(writerArguments, "--git-path", state)
	}
	for _, directory := range []string{managed, request.CallerDir} {
		status, err := git.Run(ctx, directory, "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil || status != "" {
			return "", fmt.Errorf("CI revalidation requires readable clean caller and managed worktrees")
		}
		output, err := git.Run(ctx, directory, writerArguments...)
		writerPaths := strings.Split(output, "\n")
		if err != nil || len(writerPaths) != len(writerStates) {
			return "", fmt.Errorf("cannot read Git writer state")
		}
		for _, path := range writerPaths {
			if _, err := os.Stat(resolveGitPath(directory, path)); !os.IsNotExist(err) {
				return "", fmt.Errorf("Git writer or interrupted operation requires preservation")
			}
		}
	}
	managedIdentity, err := git.Run(ctx, managed, "rev-parse", "--git-common-dir", "HEAD", candidate+"^{commit}")
	managedParts := strings.Split(managedIdentity, "\n")
	if err != nil || len(managedParts) != 3 || !samePath(resolveGitPath(managed, managedParts[0]), gateDir) || managedParts[1] != candidate || managedParts[2] != candidate {
		return "", fmt.Errorf("managed gate identity or exact candidate changed")
	}
	callerIdentity, err := git.Run(ctx, request.CallerDir, "rev-parse", "--git-common-dir", "HEAD", "--abbrev-ref", "HEAD")
	callerParts := strings.Split(callerIdentity, "\n")
	if err != nil || len(callerParts) != 3 || callerParts[2] != run.Branch {
		return "", fmt.Errorf("caller repository or branch changed")
	}
	repoCommon := callerParts[0]
	if !samePath(request.CallerDir, repo.WorkingPath) {
		repoCommon, err = git.Run(ctx, repo.WorkingPath, "rev-parse", "--git-common-dir")
		if err != nil {
			return "", fmt.Errorf("caller repository identity is unreadable")
		}
	}
	if !samePath(resolveGitPath(request.CallerDir, callerParts[0]), resolveGitPath(repo.WorkingPath, repoCommon)) {
		return "", fmt.Errorf("caller repository identity changed")
	}
	callerRun := run
	if request.ReselectPublication {
		event, err := m.db.GetCIRepairAdoption(run.ID)
		if err != nil || event == nil || !samePath(event.CallerDir, request.CallerDir) || event.CallerHeadSHA != request.CallerHeadSHA || event.CandidateSHA != candidate {
			return "", fmt.Errorf("accepted caller or candidate binding changed")
		}
		callerRun = &event.ExpectedRun
	}
	callerHead := callerParts[1]
	if callerHead != request.CallerHeadSHA || (callerHead != *callerRun.LastPushedSHA && (callerRun.SubmittedHeadSHA == nil || callerHead != *callerRun.SubmittedHeadSHA)) {
		return "", fmt.Errorf("caller head changed or has unrelated work")
	}
	directories := []string{managed, gateDir, repo.WorkingPath}
	if !samePath(request.CallerDir, repo.WorkingPath) {
		directories = append(directories, request.CallerDir)
	}
	for _, directory := range directories {
		remotes, err := git.GetConfiguredRemoteURLs(ctx, directory, "origin")
		if err != nil || len(remotes) != 1 {
			return "", fmt.Errorf("repository origin identity is ambiguous")
		}
		remote, err := url.Parse(remotes[0])
		if err != nil || remote.Scheme != "https" || remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" || branchsync.TargetFingerprint(remotes[0]) != branchsync.TargetFingerprint(repo.UpstreamURL) {
			return "", fmt.Errorf("repository origin identity changed")
		}
	}
	gateHead, exists, err := git.DirectRefTarget(ctx, gateDir, *run.PushRef)
	if err != nil || !exists {
		return "", fmt.Errorf("gate branch identity is unreadable")
	}
	if _, err := git.Run(ctx, managed, "merge-base", "--is-ancestor", *run.LastPushedSHA, candidate); err != nil {
		return "", fmt.Errorf("candidate does not retain the exact public history")
	}
	return gateHead, nil
}

func (m *RunManager) proveCIRepairGit(ctx context.Context, snapshot *db.CIRepairAdoption, request *ipc.CIRevalidationParams, candidate string, publication *forgecontext.Context) error {
	run, repo := &snapshot.ExpectedRun, &snapshot.ExpectedRepo
	managed := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
	gateHead, err := m.proveCIRepairLocal(ctx, snapshot, request, candidate)
	if err != nil {
		return err
	}
	networkCtx := git.WithEnvironment(ctx, publication.Environment)
	remoteHead, err := git.LsRemote(networkCtx, managed, repo.PushURL(), *run.PushRef)
	canReplayPublication := request.ReselectPublication && run.HeadSHA == request.CandidateSHA && run.ReviewApprovedHeadSHA != nil && *run.ReviewApprovedHeadSHA == run.HeadSHA
	if canReplayPublication {
		for _, current := range snapshot.Steps {
			if current.Snapshot.StepName.Order() < types.StepPush.Order() && current.Snapshot.StepName != types.StepIntent && current.Snapshot.Status != types.StepStatusCompleted {
				canReplayPublication = false
			}
			if current.Snapshot.StepName == types.StepTest {
				if current.Snapshot.FindingsJSON == nil {
					canReplayPublication = false
					continue
				}
				findings, parseErr := types.ParseFindingsJSON(*current.Snapshot.FindingsJSON)
				if parseErr != nil || findings.TestedHeadSHA != candidate {
					canReplayPublication = false
				}
			}
		}
	}
	if err != nil || (remoteHead != *run.LastPushedSHA && !(canReplayPublication && remoteHead == candidate)) {
		return fmt.Errorf("public branch moved or cannot be read")
	}
	if gateHead != *run.LastPushedSHA && !(canReplayPublication && remoteHead == candidate && gateHead == candidate) {
		return fmt.Errorf("gate branch no longer has the exact publication binding")
	}
	if run.PRURL == nil {
		return fmt.Errorf("CI revalidation has no bound PR")
	}
	number, err := scm.ExtractPRNumber(*run.PRURL)
	if err != nil {
		return fmt.Errorf("CI PR identity is unreadable")
	}
	command := exec.CommandContext(ctx, "gh", "api", "--hostname", publication.Host, "repos/"+github.RepoSlug(repo.UpstreamURL)+"/pulls/"+number)
	command.Dir, command.Env = managed, publication.Environment.Apply(nil)
	winproc.Harden(command)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("cannot verify the live CI PR identity")
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Head    struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if json.Unmarshal(output, &pr) != nil || pr.State != "open" || pr.HTMLURL != *run.PRURL || pr.Head.SHA != remoteHead || pr.Head.Ref != run.Branch || pr.Head.Repo.FullName != github.RepoSlug(repo.PushURL()) || pr.Base.Repo.FullName != github.RepoSlug(repo.UpstreamURL) || pr.Base.Ref == "" {
		return fmt.Errorf("live CI PR, head, branch or repository identity changed")
	}
	finalGateHead, err := m.proveCIRepairLocal(ctx, snapshot, request, candidate)
	if err != nil {
		return err
	}
	if finalGateHead != gateHead {
		return fmt.Errorf("gate branch moved during adoption proof")
	}
	return nil
}

func (m *RunManager) prepareCIAdoption(ctx context.Context, run *db.Run, event *db.CIRepairAdoption, publication *forgecontext.Context, login string) (*recoveredRunPlan, error) {
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil || repo == nil {
		return nil, fmt.Errorf("cannot read adopted repository")
	}
	workDir := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
	cfg, err := m.loadRecoveredConfig(ctx, run, repo, workDir)
	if err != nil {
		return nil, err
	}
	execSteps := steps.WithCustomGates(m.steps(), cfg.Gates)
	if run.AwaitingAgentSince != nil {
		err = pipeline.ValidateRecoveredRun(m.db, run, execSteps)
	} else {
		_, err = pipeline.CIAdoptionPendingIndex(m.db, run, execSteps, event)
	}
	if err != nil {
		return nil, err
	}
	forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, err
	}
	ag, err := m.newRunPipelineAgent(ctx, run, cfg, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(forge))
	if err != nil {
		return nil, err
	}
	return &recoveredRunPlan{run: run, repo: repo, workDir: workDir, gateDir: m.paths.RepoDir(repo.ID), cfg: cfg, agent: ag, steps: execSteps, forge: forge, adoption: event, publication: publication, publicationLogin: login}, nil
}
