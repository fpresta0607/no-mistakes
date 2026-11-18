package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

// terminalPRReadTimeout bounds the forge read a launch refusal may make.
const terminalPRReadTimeout = 30 * time.Second

// refuseUnlinkedValidation is the daemon's launch refusal for a branch with
// validation-only authored lineage. db.RefuseUnlinkedValidation decides on the
// recorded PR state, which only an active CI step writes, so a newest child
// that ended with its PR open would keep refusing after that PR was merged or
// closed on the forge (issue #194). For that child alone the live state is read
// and recorded first, and a read that cannot be made keeps the refusal. A
// branch without such a child never reaches the forge.
func (m *RunManager) refuseUnlinkedValidation(ctx context.Context, repoID, branch string) error {
	child, err := m.db.TerminalAuthoredChildWithOpenPR(repoID, branch)
	if err != nil {
		return err
	}
	if child != nil {
		if err := m.observeTerminalPR(ctx, child); err != nil {
			return fmt.Errorf("this branch has validation-only authored lineage whose newest child ended with its PR recorded open, and the live PR state could not be read: %w", err)
		}
	}
	return m.db.RefuseUnlinkedValidation(repoID, branch)
}

func (m *RunManager) observeTerminalPR(ctx context.Context, run *db.Run) error {
	ctx, cancel := context.WithTimeout(ctx, terminalPRReadTimeout)
	defer cancel()
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return err
	}
	if repo == nil {
		return fmt.Errorf("unknown repo %s", run.RepoID)
	}
	cfg, forge, err := m.globalForgeRouting(ctx, repo)
	if err != nil {
		return err
	}
	return steps.ObserveTerminalPRState(&pipeline.StepContext{Ctx: ctx, Run: run, Repo: repo, WorkDir: repo.WorkingPath, GateDir: m.paths.RepoDir(repo.ID), Config: cfg, ForgeContext: forge, DB: m.db})
}
