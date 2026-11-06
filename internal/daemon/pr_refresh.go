package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (manager *RunManager) HandleRefreshPR(ctx context.Context, request ipc.RefreshPRParams) (*ipc.RefreshPRResult, error) {
	release, err := manager.beginGateResponse()
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	branchLock := manager.branchMutex(request.RepoID, request.Branch)
	if !branchLock.TryLock() {
		return nil, fmt.Errorf("PR refresh refused: repository branch is busy")
	}
	defer branchLock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	run, err := manager.db.GetRun(request.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("PR refresh run was not found: %v", err)
	}
	repo, err := manager.db.GetRepo(run.RepoID)
	if err != nil || repo == nil {
		return nil, fmt.Errorf("PR refresh repository was not found: %v", err)
	}
	if err := validatePRRefreshIdentity(run, repo, request); err != nil {
		return nil, err
	}
	manager.mu.Lock()
	isActive := manager.executors[run.ID] != nil || manager.dones[run.ID] != nil
	manager.mu.Unlock()
	if isActive {
		return nil, fmt.Errorf("PR refresh refused: run still has an active executor")
	}
	runs, err := manager.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return nil, err
	}
	for _, candidate := range runs {
		if candidate.Branch == run.Branch {
			if candidate.ID != run.ID {
				return nil, fmt.Errorf("PR refresh refused: requested run is not the latest run on its branch")
			}
			break
		}
	}
	rows, err := manager.db.GetStepsByRun(run.ID)
	if err != nil {
		return nil, err
	}
	var hasNativePR, hasTestException bool
	for _, row := range rows {
		hasNativePR = hasNativePR || row.StepName == types.StepPR && row.Status == types.StepStatusCompleted
		hasTestException = hasTestException || row.TestOverrideReason() != ""
	}
	if !hasNativePR {
		return nil, fmt.Errorf("PR refresh requires a completed native PR step")
	}
	if !hasTestException {
		return nil, fmt.Errorf("PR refresh requires a recorded Test exception")
	}
	globalConfig, err := config.LoadGlobal(manager.paths.ConfigFile())
	if err != nil {
		return nil, err
	}
	// Only global forge routing is used. No repo command, agent or validation is launched.
	cfg := config.MergeForRemote(globalConfig, &config.RepoConfig{}, repo.UpstreamURL)
	forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err == nil {
		err = forgecontext.RefuseProviderPluginOverlap(ctx, forge, cfg.ProviderPlugins, repo.UpstreamURL)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	isUpdated, err := manager.refreshPR(&pipeline.StepContext{Ctx: ctx, Run: run, Repo: repo, WorkDir: repo.WorkingPath, GateDir: manager.paths.RepoDir(repo.ID), Config: cfg, ForgeContext: forge, DB: manager.db})
	if err != nil {
		return nil, err
	}
	return &ipc.RefreshPRResult{RunID: run.ID, HeadSHA: run.HeadSHA, PRURL: *run.PRURL, Updated: isUpdated}, nil
}

func validatePRRefreshIdentity(run *db.Run, repo *db.Repo, request ipc.RefreshPRParams) error {
	if request.RunID != run.ID || request.RepoID != repo.ID || run.RepoID != repo.ID || request.Branch != run.Branch || request.HeadSHA != run.HeadSHA || run.PRURL == nil || request.PRURL != *run.PRURL || run.PRState == nil || *run.PRState != "open" {
		return fmt.Errorf("PR refresh refused: run publication identity changed")
	}
	switch run.Status {
	case types.RunCompleted, types.RunChecksPassed, types.RunHandedOff:
	default:
		return fmt.Errorf("PR refresh requires a settled run; it cannot run or resolve validation")
	}
	if run.PushActive || run.LastPushedSHA == nil || *run.LastPushedSHA != run.HeadSHA || run.PushGeneration == nil || *run.PushGeneration < 1 || request.PushGeneration != *run.PushGeneration || run.PushRef == nil || *run.PushRef != "refs/heads/"+run.Branch || run.PushTargetFingerprint == nil || *run.PushTargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) {
		return fmt.Errorf("PR refresh refused: exact published head, target or generation changed")
	}
	return nil
}
