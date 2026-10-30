package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func ciHandbackPR(sctx *pipeline.StepContext, run *db.Run, publishedHead string) (scm.Host, *scm.PR, error) {
	if resolvedProvider(sctx) != scm.ProviderGitHub {
		return nil, nil, fmt.Errorf("published CI handback currently requires GitHub head verification")
	}
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		return nil, nil, fmt.Errorf("cannot verify handback PR: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, nil, err
	}
	pr, err := host.FindPR(sctx.Ctx, run.Branch, "")
	if err != nil {
		return nil, nil, err
	}
	if run.PRURL == nil || pr == nil || pr.URL != *run.PRURL || pr.BaseBranch == "" {
		return nil, nil, fmt.Errorf("live PR does not match handback ownership")
	}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil || state != scm.PRStateOpen {
		return nil, nil, fmt.Errorf("handback PR is not verifiably open")
	}
	pr.HeadSHA = publishedHead
	if _, err := host.GetChecks(sctx.Ctx, pr); err != nil {
		return nil, nil, err
	}
	if pr.HeadSHA != publishedHead {
		return nil, nil, fmt.Errorf("live PR head moved before handback")
	}
	return host, pr, nil
}

func CIHandbackManagedHead(sctx *pipeline.StepContext, receipt *db.CIHandback) (string, error) {
	candidate := receipt.Candidate
	if candidate == nil {
		return "", fmt.Errorf("CI handback candidate is missing")
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil || (head != receipt.ReleasedHead && head != candidate.Head) {
		return "", fmt.Errorf("managed handback head changed or is unreadable")
	}
	branch, err := git.CurrentBranch(sctx.Ctx, sctx.WorkDir)
	if err != nil || branch != candidate.ManagedBranch {
		return "", fmt.Errorf("managed handback checkout changed")
	}
	dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
	if err != nil || dirty || mergeInProgress(sctx.Ctx, sctx.WorkDir) || rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		return "", fmt.Errorf("managed handback worktree is not settled and clean")
	}
	laneHead := receipt.ReleasedHead
	if branch != "HEAD" {
		if "refs/heads/"+branch != receipt.PushRef {
			return "", fmt.Errorf("managed handback branch does not match its lane")
		}
		laneHead = head
	}
	ref, err := git.Run(sctx.Ctx, sctx.GateDir, "for-each-ref", "--format=%(objectname) %(objecttype) %(symref)", receipt.PushRef)
	if err != nil || ref != laneHead+" commit" {
		return "", fmt.Errorf("managed handback lane changed")
	}
	return head, nil
}

func restoreCIHandbackHead(sctx *pipeline.StepContext, receipt *db.CIHandback) error {
	head, err := CIHandbackManagedHead(sctx, receipt)
	if err != nil {
		return err
	}
	if head != receipt.ReleasedHead {
		if _, err := stepGitRun(sctx, "reset", "--keep", receipt.ReleasedHead); err != nil {
			return err
		}
	}
	if head, err := stepGitHeadSHA(sctx); err != nil || head != receipt.ReleasedHead {
		return fmt.Errorf("released CI head was not restored")
	}
	return sctx.DB.ClearCIHandbackCandidate(receipt)
}

func RecoverCIHandback(sctx *pipeline.StepContext, receipt *db.CIHandback) error {
	run, repo, candidate := sctx.Run, sctx.Repo, receipt.Candidate
	targetKind := "upstream"
	if repo.ForkURL != "" {
		targetKind = "fork"
	}
	if candidate == nil || run.Status != types.RunRunning || run.PushActive || run.CustodyReturnedAt != nil || receipt.RunID != run.ID ||
		run.PushTargetKind == nil || *run.PushTargetKind != targetKind ||
		run.LastPushedSHA == nil || *run.LastPushedSHA != receipt.ReleasedHead || run.PushGeneration == nil || *run.PushGeneration != receipt.PushGeneration ||
		run.PushTargetFingerprint == nil || *run.PushTargetFingerprint != receipt.TargetFingerprint || receipt.TargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) ||
		run.PushRef == nil || *run.PushRef != receipt.PushRef || receipt.PushRef != "refs/heads/"+run.Branch ||
		run.PRURL == nil || *run.PRURL != receipt.PRURL || run.PRState == nil || *run.PRState != "open" ||
		candidate.UpstreamURL != repo.UpstreamURL || candidate.ForkURL != repo.ForkURL || sctx.Config.AutoFixLimit(types.StepCI) != 0 {
		return fmt.Errorf("interrupted CI handback binding changed")
	}
	expectedHead := receipt.ReleasedHead
	if receipt.AcceptedHead != nil {
		expectedHead = candidate.Head
		if *receipt.AcceptedHead != expectedHead || run.ReviewApprovedHeadSHA != nil || run.CIReadyAt != nil || run.CIReadyNoCI {
			return fmt.Errorf("accepted CI handback validation binding changed")
		}
	}
	if run.HeadSHA != expectedHead {
		return fmt.Errorf("interrupted CI handback run head changed")
	}
	results, err := sctx.DB.GetStepsByRun(run.ID)
	if err != nil {
		return err
	}
	for _, result := range results {
		if result.AgentPID != nil {
			return fmt.Errorf("interrupted CI handback has an active writer")
		}
		if result.StepName == types.StepCI {
			rounds, err := sctx.DB.GetRoundsByStep(result.ID)
			if err != nil || len(rounds) == 0 || rounds[len(rounds)-1].ID != receipt.RoundID {
				return fmt.Errorf("interrupted CI handback conflict round changed")
			}
			if receipt.AcceptedHead == nil && (result.Status != types.StepStatusAwaitingApproval || result.AutoFixLimit != nil || result.FindingsJSON == nil || rounds[len(rounds)-1].FindingsJSON == nil || *result.FindingsJSON != *rounds[len(rounds)-1].FindingsJSON) {
				return fmt.Errorf("interrupted CI handback conflict gate changed")
			}
		}
	}
	if receipt.AcceptedHead == nil {
		if reviewRoundID, err := sctx.DB.LatestReviewRoundID(run.ID); err != nil || reviewRoundID != candidate.ReviewRoundID {
			return fmt.Errorf("interrupted CI handback review checkpoint changed")
		}
	}
	for _, ancestor := range []string{receipt.ReleasedHead, receipt.BaseHead, candidate.BaseHead} {
		if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", ancestor, candidate.Head); err != nil {
			return fmt.Errorf("interrupted CI handback lost its additive ancestry")
		}
	}
	_, pr, err := ciHandbackPR(sctx, run, receipt.ReleasedHead)
	if err != nil {
		return err
	}
	if pr.BaseBranch != receipt.BaseBranch {
		return fmt.Errorf("interrupted CI handback PR base changed")
	}
	if published, err := publishedBranchHead(sctx); err != nil || published != receipt.ReleasedHead {
		return fmt.Errorf("interrupted CI handback publication changed")
	}
	service := &branchsync.Service{DB: sctx.DB, Repo: repo, WorkDir: receipt.CallerDir, GateDir: sctx.GateDir}
	caller := service.InspectCached(sctx.Ctx)
	if caller.Pipeline.RunID != run.ID || !caller.Local.Clean || caller.Local.Branch != run.Branch || caller.Local.Head != candidate.Head {
		return fmt.Errorf("interrupted CI handback caller changed or is ambiguous")
	}
	worktrees, err := git.Run(sctx.Ctx, receipt.CallerDir, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	branchCount := 0
	for _, line := range strings.Split(worktrees, "\n") {
		if line == "branch "+receipt.PushRef {
			branchCount++
		}
	}
	if branchCount != 1 {
		return fmt.Errorf("interrupted CI handback caller branch is ambiguous")
	}
	if receipt.AcceptedHead == nil {
		return restoreCIHandbackHead(sctx, receipt)
	}
	head, err := CIHandbackManagedHead(sctx, receipt)
	if err != nil || head != candidate.Head {
		return fmt.Errorf("accepted CI handback worktree changed")
	}
	return nil
}
