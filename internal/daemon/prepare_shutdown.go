package daemon

import (
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// PrepareShutdownIfIdle closes the running daemon's admission before any
// destructive lifecycle operation. It deliberately does not stop the process:
// managed services must be stopped before their keep-alive can restart it.
func PrepareShutdownIfIdle(p *paths.Paths) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	release, err := acquireLifecycleLock(p, true)
	if err != nil {
		return nil, err
	}
	isPrepared := false
	defer func() {
		if !isPrepared {
			release()
		}
	}()
	client, err := daemonDial(p.Socket())
	if err != nil {
		isStale, inspectErr := staleDaemonArtifacts(p)
		if inspectErr == nil && isStale {
			lock, lockErr := acquireSingletonLock(p)
			if lockErr != nil {
				return nil, fmt.Errorf("cannot close daemon admission: %w; refusing lifecycle operation", lockErr)
			}
			isStale, inspectErr = staleDaemonArtifacts(p)
			if inspectErr == nil && isStale {
				isPrepared = true
				return sync.OnceFunc(func() {
					lock.Release()
					release()
				}), nil
			}
			lock.Release()
		}
		return nil, fmt.Errorf("cannot close daemon admission: %w (daemon state: %v); refusing lifecycle operation", err, inspectErr)
	}
	defer client.Close()
	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodPrepareShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		return nil, fmt.Errorf("cannot atomically prepare daemon shutdown: %w; refusing lifecycle operation", err)
	}
	if !result.OK {
		return nil, fmt.Errorf("daemon did not confirm admission closure; refusing lifecycle operation")
	}
	isPrepared = true
	return release, nil
}
