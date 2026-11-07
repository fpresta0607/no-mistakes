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

func ciHandbackPR(sctx *pipeline.StepContext, run *db.Run, publishedHead string) (scm.Host, *scm.PR, []scm.Check, error) {
	if resolvedProvider(sctx) != scm.ProviderGitHub {
		return nil, nil, nil, fmt.Errorf("published CI handback currently requires GitHub head verification")
	}
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		return nil, nil, nil, fmt.Errorf("cannot verify handback PR: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, nil, nil, err
	}
	pr, err := host.FindPR(sctx.Ctx, run.Branch, "")
	if err != nil {
		return nil, nil, nil, err
	}
	if run.PRURL == nil || pr == nil || pr.URL != *run.PRURL || pr.BaseBranch == "" {
		return nil, nil, nil, fmt.Errorf("live PR does not match handback ownership")
	}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil || state != scm.PRStateOpen {
		return nil, nil, nil, fmt.Errorf("handback PR is not verifiably open")
	}
	pr.HeadSHA = publishedHead
	checks, err := host.GetChecks(sctx.Ctx, pr)
	if err != nil {
		return nil, nil, nil, err
	}
	if pr.ObservedHeadSHA != publishedHead {
		return nil, nil, nil, fmt.Errorf("live PR head moved before handback")
	}
	return host, pr, checks, nil
}

func VerifyCIHandbackSource(sctx *pipeline.StepContext, receipt *db.CIHandback) error {
	if receipt.Source.Validation != nil {
		if err := sctx.DB.VerifyCIHandbackValidation(receipt); err != nil {
			return err
		}
	}
	anchor := fmt.Sprintf("refs/no-mistakes/ci-handback/%s/%d", receipt.RunID, receipt.PushGeneration)
	for _, directory := range []string{receipt.CallerDir, sctx.GateDir} {
		for _, source := range []struct{ ref, head string }{{anchor + "/published", receipt.ReleasedHead}, {anchor + "/author", receipt.Source.CallerHead}} {
			observed, err := git.Run(sctx.Ctx, directory, "for-each-ref", "--format=%(objectname) %(objecttype) %(symref)", source.ref)
			if err != nil || observed != source.head+" commit" {
				return fmt.Errorf("ordinary CI handback source anchor changed: %s", source.ref)
			}
		}
	}
	return nil
}

func verifyCIHandbackValidationHeads(sctx *pipeline.StepContext, run *db.Run, validation *db.CIHandbackValidation) error {
	if run.SubmittedHeadSHA == nil || run.ReviewApprovedHeadSHA == nil || len(validation.Steps) != len(validation.Rounds) {
		return fmt.Errorf("publication revalidation requires historical validation proof")
	}
	testedHead := ""
	firstReviewedHead := ""
	hasCompletedRebase := false
	hasReviewedHead := false
	for index, step := range validation.Steps {
		rounds := validation.Rounds[index]
		if step.RunID != run.ID || len(rounds) == 0 {
			return fmt.Errorf("publication revalidation has no historical %s round", step.StepName)
		}
		for _, round := range rounds {
			if round.StepResultID != step.ID {
				return fmt.Errorf("publication revalidation round ownership changed")
			}
		}
		latest := rounds[len(rounds)-1]
		if step.StepName == types.StepRebase {
			hasCompletedRebase = step.Status == types.StepStatusCompleted && step.ExitCode != nil && *step.ExitCode == 0
		}
		if step.StepName == types.StepReview {
			hasReviewedHead = latest.ReviewedHeadSHA != nil && *latest.ReviewedHeadSHA == *run.ReviewApprovedHeadSHA
			if hasCompletedRebase && rounds[0].ReviewedHeadSHA != nil {
				firstReviewedHead = *rounds[0].ReviewedHeadSHA
			}
		}
		if step.StepName == types.StepTest {
			if step.FindingsJSON == nil || latest.FindingsJSON == nil || *step.FindingsJSON != *latest.FindingsJSON {
				return fmt.Errorf("publication revalidation Test round and evidence disagree")
			}
			evidence, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil {
				return err
			}
			testedHead = evidence.TestedHeadSHA
		}
	}
	if !hasReviewedHead || testedHead == "" {
		return fmt.Errorf("publication revalidation requires recorded Review and Test heads")
	}
	heads := []string{*run.SubmittedHeadSHA, *run.ReviewApprovedHeadSHA, testedHead, run.HeadSHA}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", heads[0], heads[1]); err != nil {
		// Rebase may rewrite the submitted object before Review. Prove that
		// exact replay at the first reviewed boundary, never at a later head
		// that could include repair or post-validation commits.
		if firstReviewedHead == "" {
			return fmt.Errorf("publication revalidation requires a completed Rebase and its first Review head")
		}
		fork, err := stepGitRun(sctx, "merge-base", heads[0], firstReviewedHead)
		if err != nil {
			return fmt.Errorf("publication revalidation submitted source has no Rebase history")
		}
		commits, err := linearChain(sctx, fork, heads[0])
		if err != nil || len(commits) == 0 {
			return fmt.Errorf("publication revalidation submitted source is not a replayable chain")
		}
		base, err := stepGitRun(sctx, "rev-parse", "--verify", fmt.Sprintf("%s~%d^{commit}", firstReviewedHead, len(commits)))
		if err != nil {
			return fmt.Errorf("publication revalidation Rebase target is missing")
		}
		if reason := pureReplayGap(sctx, heads[0], base, firstReviewedHead); reason != "" {
			return fmt.Errorf("publication revalidation cannot prove submitted source through Rebase: %s", reason)
		}
		heads = append([]string{heads[0], firstReviewedHead}, heads[1:]...)
	}
	for index, head := range heads {
		if resolved, err := stepGitRun(sctx, "rev-parse", "--verify", head+"^{commit}"); err != nil || resolved != head {
			return fmt.Errorf("publication revalidation head is missing or ambiguous")
		}
		if index > 0 && !(index == 1 && head == firstReviewedHead) {
			if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", heads[index-1], head); err != nil {
				return fmt.Errorf("publication revalidation requires submitted, reviewed, tested and published ancestry")
			}
		}
	}
	return nil
}

func verifyCIHandbackPublication(sctx *pipeline.StepContext, receipt *db.CIHandback, head string) error {
	if receipt.Source == nil || receipt.Source.Validation == nil {
		return nil
	}
	if err := sctx.DB.VerifyCIHandbackValidation(receipt); err != nil {
		return err
	}
	run, err := sctx.DB.GetRun(receipt.RunID)
	if err != nil || run == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != head {
		return fmt.Errorf("CI handback publication requires fresh Review of the exact final head")
	}
	results, err := sctx.DB.GetStepsByRun(receipt.RunID)
	if err != nil {
		return err
	}
	for _, result := range results {
		if result.StepName != types.StepReview && result.StepName != types.StepTest {
			continue
		}
		rounds, err := sctx.DB.GetRoundsByStep(result.ID)
		if err != nil {
			return err
		}
		originalCount := 0
		for index, original := range receipt.Source.Validation.Steps {
			if original.ID == result.ID {
				originalCount = len(receipt.Source.Validation.Rounds[index])
			}
		}
		if result.StepName == types.StepReview {
			if result.Status != types.StepStatusCompleted || len(rounds) <= originalCount || rounds[len(rounds)-1].ReviewedHeadSHA == nil || *rounds[len(rounds)-1].ReviewedHeadSHA != head {
				return fmt.Errorf("CI handback publication requires fresh Review of the exact final head")
			}
			continue
		}
		if result.Status != types.StepStatusCompleted || result.FindingsJSON == nil || len(rounds) <= originalCount || rounds[len(rounds)-1].FindingsJSON == nil || *rounds[len(rounds)-1].FindingsJSON != *result.FindingsJSON {
			break
		}
		evidence, err := types.ParseFindingsJSON(*result.FindingsJSON)
		if err == nil && evidence.TestedHeadSHA == head {
			return nil
		}
		break
	}
	return fmt.Errorf("CI handback publication requires fresh Test of the exact final head")
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
			if receipt.Source != nil && receipt.AcceptedHead == nil && (result.FindingsJSON == nil || *result.FindingsJSON != receipt.Source.Findings) {
				return fmt.Errorf("interrupted ordinary CI handback findings changed")
			}
		}
	}
	if receipt.AcceptedHead == nil {
		if reviewRoundID, err := sctx.DB.LatestReviewRoundID(run.ID); err != nil || reviewRoundID != candidate.ReviewRoundID {
			return fmt.Errorf("interrupted CI handback review checkpoint changed")
		}
	}
	ancestors := []string{receipt.ReleasedHead, receipt.BaseHead, candidate.BaseHead}
	if receipt.Source != nil {
		if err := VerifyCIHandbackSource(sctx, receipt); err != nil {
			return err
		}
		if run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != receipt.Source.SubmittedHead || run.LastPushedAt == nil || *run.LastPushedAt != receipt.Source.PushedAt || *run.PushTargetKind != receipt.Source.PushKind {
			return fmt.Errorf("interrupted ordinary CI handback source changed")
		}
		if (receipt.Source.Validation == nil && receipt.Source.ReviewHead != receipt.ReleasedHead) || (receipt.AcceptedHead == nil && (run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != receipt.Source.ReviewHead)) {
			return fmt.Errorf("interrupted ordinary CI handback review identity changed")
		}
		if receipt.Source.Validation != nil {
			historical := *run
			historical.HeadSHA = receipt.ReleasedHead
			historical.ReviewApprovedHeadSHA = &receipt.Source.ReviewHead
			if err := verifyCIHandbackValidationHeads(sctx, &historical, receipt.Source.Validation); err != nil {
				return err
			}
		}
		ancestors = []string{receipt.ReleasedHead, receipt.Source.CallerHead}
	}
	for _, ancestor := range ancestors {
		if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", ancestor, candidate.Head); err != nil {
			return fmt.Errorf("interrupted CI handback lost its additive ancestry")
		}
	}
	_, pr, _, err := ciHandbackPR(sctx, run, receipt.ReleasedHead)
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
