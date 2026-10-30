package steps

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// HandbackCI runs only inside the executor's serialized, idle CI gate.
// Release changes no Git state and leaves the conflict round undecided.
func (s *CIStep) HandbackCI(sctx *pipeline.StepContext, request ipc.CIHandbackParams) (*ipc.CIHandbackResult, error) {
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("cannot read handback run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("handback run no longer exists")
	}
	repo, err := sctx.DB.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("cannot read handback repository: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("handback repository no longer exists")
	}
	if request.RunID != run.ID || request.RepoID != repo.ID || request.Branch != run.Branch || run.Status != types.RunRunning || run.PushActive || run.CustodyReturnedAt != nil || run.HeadSHA != sctx.Run.HeadSHA {
		return nil, fmt.Errorf("handback run ownership changed")
	}
	if repo.UpstreamURL != sctx.Repo.UpstreamURL || repo.ForkURL != sctx.Repo.ForkURL {
		return nil, fmt.Errorf("handback repository targets changed")
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != run.HeadSHA || run.PushGeneration == nil || *run.PushGeneration < 1 || run.PushTargetFingerprint == nil || *run.PushTargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) || run.PushRef == nil || *run.PushRef != "refs/heads/"+run.Branch || run.PRURL == nil || run.PRState == nil || *run.PRState != "open" {
		return nil, fmt.Errorf("handback requires exact published head, PR and push provenance")
	}
	caller, err := git.FindGitRoot(request.CallerDir)
	if err != nil {
		return nil, err
	}
	caller, err = filepath.EvalSymlinks(caller)
	if err != nil {
		return nil, err
	}
	service := &branchsync.Service{DB: sctx.DB, Repo: repo, WorkDir: caller, GateDir: sctx.GateDir}
	checkCaller := func() error {
		state := service.InspectCached(sctx.Ctx)
		if state.Pipeline.RunID != run.ID || !state.Local.Clean || state.Local.Branch != request.Branch || state.Local.Head != request.HeadSHA ||
			(state.State != branchsync.StateSynchronized && (!request.Resume || state.State != branchsync.StateLocalAhead)) {
			return fmt.Errorf("handback requires the exact clean caller and unambiguous ownership: %s", state.Safety)
		}
		return nil
	}
	if err := checkCaller(); err != nil {
		return nil, err
	}
	if !request.Resume && request.HeadSHA != run.HeadSHA {
		return nil, fmt.Errorf("synchronize the caller to the published head before handback")
	}
	step, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil || step == nil {
		return nil, fmt.Errorf("cannot read parked CI step")
	}
	if step.Status != types.StepStatusAwaitingApproval || step.AutoFixLimit != nil || step.FindingsJSON == nil {
		return nil, fmt.Errorf("handback requires a parked CI gate with automatic repair disabled")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		return nil, err
	}
	hasConflict := false
	for _, finding := range findings.Items {
		hasConflict = hasConflict || finding.Category == types.FindingCategoryCIMergeConflict
	}
	if !hasConflict {
		return nil, fmt.Errorf("handback requires an unresolved CI merge-conflict finding")
	}
	results, err := sctx.DB.GetStepsByRun(run.ID)
	if err != nil {
		return nil, err
	}
	for _, result := range results {
		if result.StepName != types.StepCI && result.Status != types.StepStatusCompleted {
			return nil, fmt.Errorf("handback requires completed validation and no active writers: %s is %s", result.StepName, result.Status)
		}
	}
	checkManaged := func() error {
		freshRepo, err := sctx.DB.GetRepo(repo.ID)
		if err != nil || freshRepo == nil || freshRepo.UpstreamURL != repo.UpstreamURL || freshRepo.ForkURL != repo.ForkURL {
			return fmt.Errorf("handback repository targets changed")
		}
		freshRun, err := sctx.DB.GetRun(run.ID)
		if err != nil || freshRun == nil || freshRun.Status != types.RunRunning || freshRun.PushActive || freshRun.HeadSHA != run.HeadSHA || freshRun.LastPushedSHA == nil || *freshRun.LastPushedSHA != run.HeadSHA || freshRun.PushGeneration == nil || *freshRun.PushGeneration != *run.PushGeneration || freshRun.PRURL == nil || *freshRun.PRURL != *run.PRURL || freshRun.PushTargetFingerprint == nil || *freshRun.PushTargetFingerprint != *run.PushTargetFingerprint {
			return fmt.Errorf("handback publication binding changed")
		}
		head, err := stepGitHeadSHA(sctx)
		if err != nil || head != run.HeadSHA {
			return fmt.Errorf("managed head moved before handback")
		}
		dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
		if err != nil || dirty {
			return fmt.Errorf("managed worktree is not clean")
		}
		if mergeInProgress(sctx.Ctx, sctx.WorkDir) || rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
			return fmt.Errorf("managed worktree has an unfinished integration")
		}
		ref, err := git.Run(sctx.Ctx, sctx.GateDir, "for-each-ref", "--format=%(objectname) %(objecttype) %(symref)", *run.PushRef)
		if err != nil || ref != run.HeadSHA+" commit" {
			return fmt.Errorf("gate lane does not match the published head")
		}
		return nil
	}
	if err := checkManaged(); err != nil {
		return nil, err
	}
	if resolvedProvider(sctx) != scm.ProviderGitHub {
		return nil, fmt.Errorf("published CI handback currently requires GitHub head verification")
	}
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		return nil, fmt.Errorf("cannot verify handback PR: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, err
	}
	pr, err := host.FindPR(sctx.Ctx, run.Branch, "")
	if err != nil {
		return nil, err
	}
	if pr == nil || pr.URL != *run.PRURL || pr.BaseBranch == "" {
		return nil, fmt.Errorf("live PR does not match handback ownership")
	}
	prState, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil || prState != scm.PRStateOpen {
		return nil, fmt.Errorf("handback PR is not verifiably open")
	}
	pr.HeadSHA = run.HeadSHA
	if _, err := host.GetChecks(sctx.Ctx, pr); err != nil {
		return nil, err
	}
	if pr.HeadSHA != run.HeadSHA {
		return nil, fmt.Errorf("live PR head moved before handback")
	}
	if !request.Resume {
		mergeable, err := host.GetMergeableState(sctx.Ctx, pr)
		if err != nil || mergeable != scm.MergeableConflict {
			return nil, fmt.Errorf("live PR no longer reports the parked merge conflict")
		}
	}
	baseRef := "refs/heads/" + pr.BaseBranch
	baseObservation, err := stepGitRun(sctx, "ls-remote", repo.UpstreamURL, baseRef)
	if err != nil {
		return nil, err
	}
	baseFields := strings.Fields(baseObservation)
	if len(baseFields) != 2 || baseFields[1] != baseRef {
		return nil, fmt.Errorf("handback base branch is missing or ambiguous")
	}
	baseHead := baseFields[0]
	receipt, err := sctx.DB.LatestCIHandback(run.ID)
	if err != nil {
		return nil, err
	}
	if receipt != nil && receipt.AcceptedHead == nil {
		if receipt.CallerDir != caller || receipt.ReleasedHead != run.HeadSHA || receipt.PRURL != pr.URL || receipt.BaseBranch != pr.BaseBranch || receipt.PushGeneration != *run.PushGeneration || receipt.TargetFingerprint != *run.PushTargetFingerprint || receipt.PushRef != *run.PushRef {
			return nil, fmt.Errorf("released CI handback binding changed")
		}
	} else if request.Resume {
		return nil, fmt.Errorf("release the exact published CI head before submitting a follow-up")
	} else {
		rounds, err := sctx.DB.GetRoundsByStep(step.ID)
		if err != nil || len(rounds) == 0 {
			return nil, fmt.Errorf("parked CI has no durable round")
		}
		receipt = &db.CIHandback{RunID: run.ID, RoundID: rounds[len(rounds)-1].ID, CallerDir: caller, ReleasedHead: run.HeadSHA, PRURL: pr.URL, BaseBranch: pr.BaseBranch, BaseHead: baseHead, PushGeneration: *run.PushGeneration, TargetFingerprint: *run.PushTargetFingerprint, PushRef: *run.PushRef}
	}
	result := &ipc.CIHandbackResult{RunID: run.ID, PRURL: pr.URL, ReleasedHead: receipt.ReleasedHead}
	if request.Resume {
		if request.HeadSHA == receipt.ReleasedHead {
			return nil, fmt.Errorf("follow-up head must include an additive merge resolving the conflict")
		}
		for _, ancestor := range []string{receipt.ReleasedHead, receipt.BaseHead, baseHead} {
			if _, err := git.Run(sctx.Ctx, caller, "merge-base", "--is-ancestor", ancestor, request.HeadSHA); err != nil {
				return nil, fmt.Errorf("follow-up must preserve published head and both recorded and current base heads through an additive merge")
			}
		}
		merges, err := git.Run(sctx.Ctx, caller, "rev-list", "--min-parents=2", receipt.ReleasedHead+".."+request.HeadSHA)
		if err != nil || merges == "" {
			return nil, fmt.Errorf("follow-up must contain an additive merge, never a replay")
		}
	}
	// Recheck every local assumption after the provider calls, before any write.
	if err := checkCaller(); err != nil {
		return nil, err
	}
	if err := checkManaged(); err != nil {
		return nil, err
	}
	published, err := publishedBranchHead(sctx)
	if err != nil || published != receipt.ReleasedHead {
		return nil, fmt.Errorf("published head changed before handback")
	}
	if err := sctx.Ctx.Err(); err != nil {
		return nil, err
	}
	if !request.Resume {
		if receipt.ID == "" {
			if err := sctx.DB.ReleaseCIHandback(receipt, run, repo, *step.FindingsJSON); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	// Import objects without firing the gate's receive hook or changing the caller.
	if _, err := stepGitRun(sctx, "fetch", "--no-tags", "--no-write-fetch-head", caller, request.HeadSHA); err != nil {
		return nil, err
	}
	if err := checkCaller(); err != nil {
		return nil, err
	}
	if err := checkManaged(); err != nil {
		return nil, err
	}
	if _, err := stepGitRun(sctx, "merge", "--ff-only", "--no-edit", request.HeadSHA); err != nil {
		return nil, err
	}
	acceptedHead, err := stepGitHeadSHA(sctx)
	if err != nil || acceptedHead != request.HeadSHA {
		return nil, fmt.Errorf("managed follow-up head changed during acceptance")
	}
	if err := sctx.DB.AcceptCIHandback(receipt, request.HeadSHA, repo); err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = request.HeadSHA
	sctx.Run.ReviewApprovedHeadSHA = nil
	sctx.Run.CIReadyAt = nil
	sctx.Run.CIReadyNoCI = false
	result.AcceptedHead = request.HeadSHA
	return result, nil
}
