package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func (m *RunManager) HandleCIHandback(ctx context.Context, params ipc.CIHandbackParams) (*ipc.CIHandbackResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var result *ipc.CIHandbackResult
	_, err := m.withBranchLock(params.RepoID, params.Branch, func() (string, error) {
		m.mu.Lock()
		executor := m.executors[params.RunID]
		m.mu.Unlock()
		if executor == nil {
			return "", fmt.Errorf("no active executor for handback run %s", params.RunID)
		}
		var err error
		result, err = executor.HandbackCI(ctx, params)
		return "", err
	})
	return result, err
}
