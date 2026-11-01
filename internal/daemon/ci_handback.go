package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (m *RunManager) HandleCIHandback(ctx context.Context, params ipc.CIHandbackParams) (*ipc.CIHandbackResult, error) {
	if ipc.PeerPID(ctx) <= 0 || !filepath.IsAbs(params.CallerDir) {
		return nil, fmt.Errorf("CI handback requires an authenticated local operator and absolute caller directory")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var result *ipc.CIHandbackResult
	_, err := m.withBranchLock(params.RepoID, params.Branch, func() (string, error) {
		runs, err := m.db.GetRunsByRepo(params.RepoID)
		if err != nil {
			return "", err
		}
		isLatest := false
		for _, run := range runs {
			if run.Branch == params.Branch {
				isLatest = run.ID == params.RunID
				break
			}
		}
		if !isLatest {
			return "", fmt.Errorf("CI handback request does not identify the latest run on this repository branch")
		}
		receipt, err := m.db.LatestCIHandback(params.RunID)
		if err != nil {
			return "", err
		}
		if params.Resume && receipt != nil && receipt.AcceptedHead != nil {
			run, err := m.db.GetRun(params.RunID)
			if err != nil || run == nil || run.Status != types.RunRunning || run.RepoID != params.RepoID || run.Branch != params.Branch || run.HeadSHA != *receipt.AcceptedHead || params.HeadSHA != *receipt.AcceptedHead || receipt.ID != params.ReceiptID || params.PublishedHead != receipt.ReleasedHead || params.PushGeneration != receipt.PushGeneration || params.RoundID != receipt.RoundID || run.PushActive || run.CustodyReturnedAt != nil || run.PRURL == nil || *run.PRURL != receipt.PRURL || run.PRState == nil || *run.PRState != "open" {
				return "", fmt.Errorf("accepted CI handback identity changed; no validation was restarted")
			}
			if run.LastPushedSHA == nil || run.PushGeneration == nil {
				return "", fmt.Errorf("accepted CI handback publication binding is missing")
			}
			isOriginalPublication := *run.LastPushedSHA == receipt.ReleasedHead && *run.PushGeneration == receipt.PushGeneration
			isAcceptedPublication := *run.LastPushedSHA == *receipt.AcceptedHead && *run.PushGeneration == receipt.PushGeneration+1
			if !isOriginalPublication && !isAcceptedPublication {
				return "", fmt.Errorf("accepted CI handback publication generation changed")
			}
			repo, err := m.db.GetRepo(run.RepoID)
			if err != nil || repo == nil || run.PushTargetFingerprint == nil || *run.PushTargetFingerprint != receipt.TargetFingerprint || receipt.TargetFingerprint != branchsync.TargetFingerprint(repo.PushURL()) || run.PushRef == nil || *run.PushRef != receipt.PushRef {
				return "", fmt.Errorf("accepted CI handback target changed")
			}
			caller, err := git.FindGitRoot(params.CallerDir)
			if err != nil {
				return "", err
			}
			caller, err = filepath.EvalSymlinks(caller)
			if err != nil || !samePath(caller, receipt.CallerDir) {
				return "", fmt.Errorf("accepted CI handback caller changed")
			}
			binding := ipc.CIHandbackBinding{RepoID: repo.ID, RunID: run.ID, Branch: run.Branch, CallerDir: caller, CallerHead: params.HeadSHA, CurrentHead: receipt.ReleasedHead, PublishedHead: receipt.ReleasedHead, PushGeneration: receipt.PushGeneration}
			if params.Expect != nil && *params.Expect != binding {
				return "", fmt.Errorf("accepted CI handback expectation changed")
			}
			service := &branchsync.Service{DB: m.db, Repo: repo, WorkDir: caller, GateDir: m.paths.RepoDir(repo.ID)}
			state := service.InspectCached(ctx)
			if state.Pipeline.RunID != run.ID || !state.Local.Clean || state.Local.Branch != run.Branch || state.Local.Head != params.HeadSHA {
				return "", fmt.Errorf("accepted CI handback worktree or latest ownership changed")
			}
			if receipt.Source != nil {
				if err := steps.VerifyCIHandbackSource(&pipeline.StepContext{Ctx: ctx, GateDir: service.GateDir}, receipt); err != nil {
					return "", err
				}
				binding.CallerHead = receipt.Source.CallerHead
			}
			if head, err := git.HeadSHA(ctx, caller); err != nil || head != params.HeadSHA {
				return "", fmt.Errorf("accepted CI handback caller moved during replay")
			}
			result = &ipc.CIHandbackResult{RunID: run.ID, PRURL: receipt.PRURL, ReleasedHead: receipt.ReleasedHead, AcceptedHead: *receipt.AcceptedHead, ReceiptID: receipt.ID, RoundID: receipt.RoundID, Binding: binding}
			return run.ID, nil
		}
		m.mu.Lock()
		executor := m.executors[params.RunID]
		m.mu.Unlock()
		if executor == nil {
			return "", fmt.Errorf("no active executor for handback run %s", params.RunID)
		}
		result, err = executor.HandbackCI(ctx, params)
		return "", err
	})
	return result, err
}
