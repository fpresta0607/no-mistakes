package daemon

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
)

var errAdmissionClosed = errors.New("daemon is shutting down")

func (m *RunManager) beginAdmission() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.admissionClosed || m.shuttingDown.Load() {
		return errAdmissionClosed
	}
	m.admissions++
	return nil
}

func (m *RunManager) endAdmission() {
	m.mu.Lock()
	m.admissions--
	m.mu.Unlock()
}

// prepareShutdown closes admission before a caller may stop a service or use
// a PID fallback. An in-flight launch counts as busy before it creates a row.
func (m *RunManager) prepareShutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) > 0 {
		return fmt.Errorf("refusing shutdown because %d active pipeline runs are in progress; pass --force to stop the daemon anyway\n%s", len(runs), lifecycle.RunList(runs))
	}
	if m.admissions > 0 || len(m.runQueue) > 0 || len(m.executors) > 0 {
		return fmt.Errorf("refusing shutdown because pipeline admission or execution is in progress; retry when idle or pass --force")
	}
	m.admissionClosed = true
	return nil
}
