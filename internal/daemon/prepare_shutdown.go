package daemon

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// PrepareShutdownIfIdle closes the running daemon's admission before any
// destructive lifecycle operation. It deliberately does not stop the process:
// managed services must be stopped before their keep-alive can restart it.
func PrepareShutdownIfIdle(p *paths.Paths) error {
	if p == nil {
		return nil
	}
	client, err := daemonDial(p.Socket())
	if err != nil {
		isStale, inspectErr := staleDaemonArtifacts(p)
		if inspectErr == nil && isStale {
			lock, lockErr := acquireSingletonLock(p)
			if lockErr != nil {
				return fmt.Errorf("cannot close daemon admission: %w; refusing lifecycle operation", lockErr)
			}
			defer lock.Release()
			isStale, inspectErr = staleDaemonArtifacts(p)
			if inspectErr == nil && isStale {
				return nil
			}
		}
		return fmt.Errorf("cannot close daemon admission: %w (daemon state: %v); refusing lifecycle operation", err, inspectErr)
	}
	defer client.Close()
	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodPrepareShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		return fmt.Errorf("cannot atomically prepare daemon shutdown: %w; refusing lifecycle operation", err)
	}
	if !result.OK {
		return fmt.Errorf("daemon did not confirm admission closure; refusing lifecycle operation")
	}
	return nil
}
