package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// HandleStepHandback releases a parked Test or Lint failure to its
// contributor, or submits the contributor's correction to the same run.
func (m *RunManager) HandleStepHandback(ctx context.Context, params ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	if ipc.PeerPID(ctx) <= 0 || !filepath.IsAbs(params.CallerDir) {
		return nil, fmt.Errorf("handback requires an authenticated local operator and absolute caller directory")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var result *ipc.StepHandbackResult
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
			return "", fmt.Errorf("handback request does not identify the latest run on this repository branch")
		}
		m.mu.Lock()
		executor := m.executors[params.RunID]
		m.mu.Unlock()
		if executor == nil {
			return "", fmt.Errorf("no active executor for handback run %s", params.RunID)
		}
		result, err = executor.HandbackStep(ctx, params)
		return "", err
	})
	return result, err
}
