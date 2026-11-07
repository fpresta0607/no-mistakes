package daemon

import (
	"fmt"
	"os"
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
				if err := removeStaleDaemonArtifacts(p); err != nil {
					lock.Release()
					return nil, err
				}
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

// prepareAbsentDaemonStartup retains both locks while startup replaces stale
// artifacts and service definitions. A live owner requires shutdown preparation
// through the ordinary or explicitly forced lifecycle path instead.
func prepareAbsentDaemonStartup(p *paths.Paths) (func(), error) {
	releaseLifecycle, err := acquireLifecycleLock(p, true)
	if err != nil {
		return nil, err
	}
	lock, err := acquireSingletonLock(p)
	if err != nil {
		releaseLifecycle()
		return nil, fmt.Errorf("cannot establish absent daemon ownership; refusing start: %w", err)
	}
	release := sync.OnceFunc(func() {
		lock.Release()
		releaseLifecycle()
	})
	if err := reconcileCollidingDaemons(p); err != nil {
		release()
		return nil, err
	}
	isStale, err := staleDaemonArtifacts(p)
	if err != nil {
		release()
		return nil, fmt.Errorf("inspect absent daemon ownership; refusing start: %w", err)
	}
	if !isStale {
		release()
		return nil, fmt.Errorf("daemon ownership is not proven absent; refusing start")
	}
	if err := removeStaleDaemonArtifacts(p); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func removeStaleDaemonArtifacts(p *paths.Paths) error {
	for _, artifact := range []string{p.Socket(), p.PIDFile()} {
		if err := os.Remove(artifact); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale daemon artifact %s: %w; refusing lifecycle operation", artifact, err)
		}
	}
	return nil
}
