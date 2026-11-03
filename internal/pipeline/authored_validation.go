package pipeline

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func ValidateAuthoredCandidate(ctx context.Context, database *db.DB, run *db.Run, workDir string, steps []Step, event *db.ValidationAdmission) error {
	if event == nil || run.ID != event.ChildID || run.HeadSHA != event.Request.CandidateSHA || len(steps) != len(event.Steps) {
		return fmt.Errorf("authored candidate or fresh step sequence changed")
	}
	for index, step := range steps {
		if step.Name() != event.Steps[index] {
			return fmt.Errorf("authored step sequence changed")
		}
	}
	if err := database.ValidatePendingAdmission(event); err != nil {
		return err
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil || head != event.Request.CandidateSHA {
		return fmt.Errorf("authored candidate moved before fresh validation")
	}
	tree, err := git.Run(ctx, workDir, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != event.Request.CandidateTree {
		return fmt.Errorf("authored candidate content changed before fresh validation")
	}
	status, err := git.Run(ctx, workDir, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil || status != "" {
		return fmt.Errorf("authored validation checkout is not clean")
	}
	return nil
}
