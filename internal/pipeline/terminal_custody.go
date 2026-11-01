package pipeline

import (
	"context"
	"fmt"
	"os"
	"reflect"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ValidateTerminalRestart admits only the acceptance-to-first-review window.
// Once a new turn has run, ordinary parked recovery owns it; this cannot replay it.
func ValidateTerminalRestart(database *db.DB, run *db.Run, steps []Step, event *db.TerminalAdoption) error {
	if event == nil || run.ID != event.Snapshot.Run.ID || run.HeadSHA != event.CandidateSHA || run.Status != types.RunRunning || run.ReviewApprovedHeadSHA != nil || run.CIReadyAt != nil {
		return fmt.Errorf("terminal acceptance is not awaiting fresh validation")
	}
	current, err := database.ReadTerminalSnapshot(run.ID)
	if err != nil {
		return err
	}
	if len(current.Steps) != len(steps) || len(current.Steps) != len(event.Snapshot.Steps) {
		return fmt.Errorf("terminal step sequence changed")
	}
	for index, step := range current.Steps {
		if step.StepName != steps[index].Name() || step.ID != event.Snapshot.Steps[index].ID || !reflect.DeepEqual(current.Rounds[index], event.Snapshot.Rounds[index]) {
			return fmt.Errorf("terminal validation already progressed or its history changed")
		}
		if step.StepOrder >= types.StepReview.Order() && step.Status != types.StepStatusPending {
			return fmt.Errorf("terminal validation already started")
		}
	}
	return nil
}

func (e *Executor) ResumeTerminalValidation(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, event *db.TerminalAdoption) error {
	if err := ValidateTerminalRestart(e.db, run, e.steps, event); err != nil {
		return err
	}
	e.workDir = workDir
	ctx = e.runContext(ctx)
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil || head != event.CandidateSHA {
		return fmt.Errorf("accepted candidate moved before validation")
	}
	dirty, err := git.HasUncommittedChanges(ctx, workDir)
	if err != nil || dirty {
		return fmt.Errorf("accepted candidate became dirty before validation")
	}
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	if err := recordCommandConfiguration(logDir, "terminal-recovery", e.config); err != nil {
		return err
	}
	e.initializeRunScopes(run.ID)
	defer e.shared.Close()
	e.sessions.Forget(SessionRoleReviewer)
	e.sessions.Forget(SessionRoleFixer)
	index, err := e.stepIndex(types.StepReview)
	if err != nil {
		return err
	}
	e.emitRunEvent("run_updated", run, repo)
	return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, index, true)
}
