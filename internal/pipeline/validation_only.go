package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	if action != types.ActionFix && action != types.ActionSkip {
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
