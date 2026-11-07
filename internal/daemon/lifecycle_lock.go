package daemon

import (
	"fmt"
	"os"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Startup parents and their daemon children share this lock. Maintenance takes
// it exclusively before closing admission and retains it through all teardown.
func acquireLifecycleLock(p *paths.Paths, isMaintenance bool) (func(), error) {
	file, err := os.OpenFile(p.LifecycleLockFile(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open daemon lifecycle lock: %w", err)
	}
	if isMaintenance {
		err = tryLockFile(file)
	} else {
		err = trySharedLockFile(file)
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("daemon startup or maintenance is in progress; refusing lifecycle operation: %w", err)
	}
	return sync.OnceFunc(func() {
		_ = unlockFile(file)
		_ = file.Close()
	}), nil
}

// PrepareForcedShutdown retains startup exclusion without an idle check.
// Only a caller with explicit force authority may use it.
func PrepareForcedShutdown(p *paths.Paths) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	return acquireLifecycleLock(p, true)
}
