package pipeline

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func ValidateTerminalSuccessor(database *db.DB, run *db.Run, steps []Step, event *db.TerminalAdoption) error {
	if event == nil || run.ID != event.Successor.ID || run.HeadSHA != event.CandidateSHA {
		return fmt.Errorf("terminal successor identity changed")
	}
	if len(steps) != len(event.StepNames) {
		return fmt.Errorf("terminal step sequence changed")
	}
	for index, step := range steps {
		if step.Name() != event.StepNames[index] {
			return fmt.Errorf("terminal step sequence changed")
		}
	}
	return database.ValidateTerminalSuccessor(event)
}

func (e *Executor) ExecuteTerminalSuccessor(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, event *db.TerminalAdoption) error {
	e.workDir = workDir
	if err := ValidateTerminalSuccessor(e.db, run, e.steps, event); err != nil {
		return e.failRun(run, repo, err, ctx)
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil || head != event.CandidateSHA {
		return e.failRun(run, repo, fmt.Errorf("accepted candidate moved before validation"), ctx)
	}
	tree, err := git.Run(ctx, workDir, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != event.CandidateTree {
		return e.failRun(run, repo, fmt.Errorf("accepted candidate content changed"), ctx)
	}
	dirty, err := git.HasUncommittedChangesIncludingHidden(ctx, workDir)
	if err != nil || dirty {
		return e.failRun(run, repo, fmt.Errorf("accepted candidate became dirty before validation"), ctx)
	}
	return e.Execute(ctx, run, repo, workDir)
}
