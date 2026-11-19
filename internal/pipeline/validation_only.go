package pipeline

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (sctx *StepContext) IsValidationOnly() (bool, error) {
	if sctx.DB == nil || sctx.Run == nil {
		return false, nil
	}
	return validationOnlyAuthority(sctx.DB, sctx.Run.ID)
}

func (sctx *StepContext) AssertValidationSource() error {
	isValidationOnly, err := sctx.IsValidationOnly()
	if err != nil || !isValidationOnly {
		return err
	}
	event, err := sctx.DB.GetValidationPredecessor(sctx.Run.ID)
	if err != nil {
		return err
	}
	if event == nil || sctx.Fixing || sctx.Run.HeadSHA != event.Request.CandidateSHA {
		return fmt.Errorf("validation-only authored admission has no source repair authority")
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("%w: validation-only authored commit could not be read; no commit or publication is authorized: %w", ErrValidationSourceUnverified, err)
	}
	if head != event.Request.CandidateSHA {
		return fmt.Errorf("validation-only authored commit changed; no commit or publication is authorized")
	}
	tree, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return fmt.Errorf("%w: validation-only authored tree could not be read; no commit or publication is authorized: %w", ErrValidationSourceUnverified, err)
	}
	if tree != event.Request.CandidateTree {
		return fmt.Errorf("validation-only authored tree changed; no commit or publication is authorized")
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("%w: validation-only authored source status could not be read; no commit or publication is authorized: %w", ErrValidationSourceUnverified, err)
	}
	if status != "" {
		return fmt.Errorf("validation-only authored source is not clean; no commit or publication is authorized")
	}
	return nil
}

// ErrValidationSourceUnverified marks an AssertValidationSource refusal that
// comes from git failing to read the source, not from git proving the commit,
// tree or cleanliness changed. Every caller still refuses on it; only a parked
// gate's reconciliation keeps the gate instead of failing the run (issue #222).
var ErrValidationSourceUnverified = errors.New("validation source unverified")

func validationOnlyAuthority(database *db.DB, runID string) (bool, error) {
	family, err := database.ValidationFamily(runID)
	if err != nil {
		return false, fmt.Errorf("validation family ledger: %w", err)
	}
	if family == nil {
		return false, nil
	}
	if family.HasChildRepairs {
		return false, fmt.Errorf("validation-only lineage contains a forbidden repair selection or round; no dispatch is authorized")
	}
	return true, nil
}

func (e *Executor) initializeValidationAuthority(run *db.Run) error {
	isValidationOnly, err := validationOnlyAuthority(e.db, run.ID)
	if err != nil {
		return err
	}
	e.runID, e.validationOnly = run.ID, isValidationOnly
	if isValidationOnly && len(e.skips) != 0 {
		return fmt.Errorf("validation-only authored admission requires every stage; skips are refused")
	}
	return nil
}

func (e *Executor) refuseValidationRepair(action types.ApprovalAction) error {
	if action != types.ActionFix && action != types.ActionSkip && action != actionCIFollowup && action != actionStepHandback {
		return nil
	}
	isValidationOnly, err := validationOnlyAuthority(e.db, e.runID)
	if err != nil {
		return err
	}
	if e.validationOnly || isValidationOnly {
		return fmt.Errorf("validation-only authored admission has zero automatic/manual repair authority and requires every stage; %s is refused", action)
	}
	return nil
}
