package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// HandbackStep releases a parked Test failure to its contributor, or accepts
// the contributor's correction (see handbackParkedStep).
func (s *TestStep) HandbackStep(sctx *pipeline.StepContext, request ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	return handbackParkedStep(sctx, s.Name(), request)
}

// HandbackStep releases a parked Lint failure to its contributor, or accepts
// the contributor's correction (see handbackParkedStep).
func (s *LintStep) HandbackStep(sctx *pipeline.StepContext, request ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	return handbackParkedStep(sctx, s.Name(), request)
}

// StepHandbackRef keeps a released head reachable in the contributor's
// worktree, so the correction can be built on it.
func StepHandbackRef(runID, roundID string) string {
	return fmt.Sprintf("refs/no-mistakes/step-handback/%s/%s", runID, roundID)
}

// handbackParkedStep runs only inside the executor's serialized, idle gate.
// Release hands the parked round to the contributor's worktree with a receipt;
// resume moves the run to exactly one contributor commit on the released head,
// after which the executor validates the same step again in the same run.
func handbackParkedStep(sctx *pipeline.StepContext, name types.StepName, request ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("cannot read handback run: %w", err)
	}
	if run == nil || request.RunID != run.ID || request.RepoID != run.RepoID || request.Branch != run.Branch || run.Status != types.RunRunning || run.PushActive || run.HeadSHA != sctx.Run.HeadSHA {
		return nil, fmt.Errorf("handback run ownership changed")
	}
	step, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil || step == nil || step.StepName != name || step.Status != types.StepStatusAwaitingApproval || step.AutoFixLimit != nil || step.FindingsJSON == nil || sctx.Config.AutoFixLimit(name) != 0 {
		return nil, fmt.Errorf("handback requires a parked %s failure with auto_fix.%s set to 0", name, name)
	}
	if err := requireContributorFailure(*step.FindingsJSON); err != nil {
		return nil, err
	}
	rounds, err := sctx.DB.GetRoundsByStep(step.ID)
	if err != nil || len(rounds) == 0 {
		return nil, fmt.Errorf("parked %s gate has no durable round", name)
	}
	round := rounds[len(rounds)-1]
	if round.FindingsJSON == nil || *round.FindingsJSON != *step.FindingsJSON {
		return nil, fmt.Errorf("parked %s round changed", name)
	}
	caller, err := git.FindGitRoot(request.CallerDir)
	if err != nil {
		return nil, err
	}
	caller, err = realpath.Resolve(caller)
	if err != nil {
		return nil, err
	}
	if branch, err := git.CurrentBranch(sctx.Ctx, caller); err != nil || branch != run.Branch {
		return nil, fmt.Errorf("handback caller must be on branch %s", run.Branch)
	}
	if head, err := git.HeadSHA(sctx.Ctx, caller); err != nil || head != request.HeadSHA {
		return nil, fmt.Errorf("handback caller head moved")
	}
	if err := checkHandbackManagedHead(sctx, run.HeadSHA); err != nil {
		return nil, err
	}
	result := &ipc.StepHandbackResult{RunID: run.ID, Step: name, RoundID: round.ID, CallerDir: caller, ReleasedHead: run.HeadSHA, ReleasedRef: StepHandbackRef(run.ID, round.ID)}
	if request.Resume {
		return acceptStepHandback(sctx, step, round, request, result)
	}
	return releaseStepHandback(sctx, step, round, request, result)
}

// requireContributorFailure admits only a failure the gate's repair agent
// would otherwise have fixed; anything an operator must decide stays parked.
func requireContributorFailure(findingsJSON string) error {
	if pipeline.HasProtectedPathRefusal(findingsJSON) || pipeline.HasSafetyRefusal(findingsJSON) {
		return fmt.Errorf("handback cannot consume a refusal; it needs an operator decision")
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return err
	}
	if len(findings.Items) == 0 {
		return fmt.Errorf("handback requires a parked failure")
	}
	for _, finding := range findings.Items {
		if finding.ActionOrDefault() != types.ActionAutoFix {
			return fmt.Errorf("finding %s needs an operator decision, not a contributor correction", finding.ID)
		}
	}
	return nil
}

func checkHandbackManagedHead(sctx *pipeline.StepContext, head string) error {
	managed, err := stepGitHeadSHA(sctx)
	if err != nil || managed != head {
		return fmt.Errorf("managed head moved before handback")
	}
	dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
	if err != nil || dirty {
		return fmt.Errorf("managed worktree is not clean")
	}
	if mergeInProgress(sctx.Ctx, sctx.WorkDir) || rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		return fmt.Errorf("managed worktree has an unfinished integration")
	}
	return nil
}

func releaseStepHandback(sctx *pipeline.StepContext, step *db.StepResult, round *db.StepRound, request ipc.StepHandbackParams, result *ipc.StepHandbackResult) (*ipc.StepHandbackResult, error) {
	receipt, err := sctx.DB.GetStepHandbackByRound(round.ID)
	if err != nil {
		return nil, err
	}
	if receipt != nil && (receipt.CallerDir != result.CallerDir || receipt.ReleasedHead != result.ReleasedHead || receipt.AcceptedHead != nil) {
		return nil, fmt.Errorf("this %s round was already released to %s", step.StepName, receipt.CallerDir)
	}
	// The contributor builds on the released head, which the gate may have
	// moved past the caller's own commits; a caller holding commits the run
	// does not is refused, since no single commit on the run's head holds them.
	if _, err := git.Run(sctx.Ctx, result.CallerDir, "fetch", "--no-tags", sctx.WorkDir, "HEAD:"+result.ReleasedRef); err != nil {
		return nil, fmt.Errorf("make the released head reachable in the caller: %w", err)
	}
	if anchored, err := git.Run(sctx.Ctx, result.CallerDir, "rev-parse", "--verify", result.ReleasedRef+"^{commit}"); err != nil || anchored != result.ReleasedHead {
		return nil, fmt.Errorf("released head anchor %s does not name %s", result.ReleasedRef, result.ReleasedHead)
	}
	if _, err := git.Run(sctx.Ctx, result.CallerDir, "merge-base", "--is-ancestor", request.HeadSHA, result.ReleasedHead); err != nil {
		return nil, fmt.Errorf("the caller holds commits the run does not; a handback correction is one commit on the released head %s", result.ReleasedHead)
	}
	if err := checkHandbackManagedHead(sctx, result.ReleasedHead); err != nil {
		return nil, err
	}
	if receipt == nil {
		receipt = &db.StepHandback{RunID: result.RunID, StepResultID: step.ID, StepName: step.StepName, RoundID: round.ID, CallerDir: result.CallerDir, ReleasedHead: result.ReleasedHead}
		if err := sctx.DB.ReleaseStepHandback(receipt); err != nil {
			return nil, err
		}
	}
	result.ReceiptID = receipt.ID
	return result, nil
}

func acceptStepHandback(sctx *pipeline.StepContext, step *db.StepResult, round *db.StepRound, request ipc.StepHandbackParams, result *ipc.StepHandbackResult) (_ *ipc.StepHandbackResult, returnErr error) {
	receipt, err := sctx.DB.GetStepHandback(request.ReceiptID)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.RunID != result.RunID {
		return nil, fmt.Errorf("handback receipt %s does not belong to run %s", request.ReceiptID, result.RunID)
	}
	if receipt.StepResultID != step.ID || receipt.StepName != step.StepName || receipt.RoundID != round.ID || receipt.ReleasedHead != result.ReleasedHead || receipt.AcceptedHead != nil {
		return nil, fmt.Errorf("handback receipt %s no longer matches the parked %s gate", receipt.ID, step.StepName)
	}
	if receipt.CallerDir != result.CallerDir {
		return nil, fmt.Errorf("resume from the worktree the failure was released to: %s", receipt.CallerDir)
	}
	parents, err := git.Run(sctx.Ctx, result.CallerDir, "rev-list", "--parents", "-n", "1", request.HeadSHA)
	if fields := strings.Fields(parents); err != nil || len(fields) != 2 || fields[0] != request.HeadSHA || fields[1] != receipt.ReleasedHead {
		return nil, fmt.Errorf("the correction must be exactly one commit on the released head %s", receipt.ReleasedHead)
	}
	if _, err := stepGitRun(sctx, "fetch", "--no-tags", "--no-write-fetch-head", result.CallerDir, request.HeadSHA); err != nil {
		return nil, fmt.Errorf("import the correction: %w", err)
	}
	if err := checkHandbackManagedHead(sctx, receipt.ReleasedHead); err != nil {
		return nil, err
	}
	if _, err := stepGitRun(sctx, "merge", "--ff-only", request.HeadSHA); err != nil {
		return nil, err
	}
	defer func() {
		if returnErr == nil {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), 30*time.Second)
		defer cancel()
		cleanup := *sctx
		cleanup.Ctx = cleanupContext
		if _, err := stepGitRun(&cleanup, "reset", "--hard", receipt.ReleasedHead); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore released head: %w", err))
		} else if err := updateNonSharedBranchRef(&cleanup, receipt.ReleasedHead); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore released branch: %w", err))
		}
	}()
	if head, err := stepGitHeadSHA(sctx); err != nil || head != request.HeadSHA {
		return nil, fmt.Errorf("managed head changed during handback acceptance")
	}
	if err := updateNonSharedBranchRef(sctx, request.HeadSHA); err != nil {
		return nil, err
	}
	if err := sctx.DB.AcceptStepHandback(receipt, request.HeadSHA); err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = request.HeadSHA
	stopStaleSideLanes(sctx, request.HeadSHA)
	result.ReceiptID = receipt.ID
	result.AcceptedHead = request.HeadSHA
	return result, nil
}
