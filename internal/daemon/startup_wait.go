package daemon

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const maxDaemonStartupElapsed = 10 * time.Minute

type startupWait struct {
	timeout      time.Duration
	deadline     time.Time
	hardDeadline time.Time
	notBefore    time.Time
	pid          int
	startedAt    time.Time
	snapshot     string
	total        int
	completed    int
}

func (wait *startupWait) observe(p *paths.Paths, now time.Time) {
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil || record.Startup == nil || record.StartedAt.IsZero() || record.StartedAt.Before(wait.notBefore) {
		return
	}
	work := record.Startup
	if work.Snapshot == "" || work.Total <= 0 || work.Completed < 0 || work.Completed > work.Total || (wait.pid != 0 && record.PID != wait.pid) {
		return
	}
	if !wait.startedAt.IsZero() && !record.StartedAt.Equal(wait.startedAt) {
		return
	}
	if wait.total != 0 && (work.Snapshot != wait.snapshot || work.Total != wait.total || work.Completed <= wait.completed) {
		return
	}
	actualStart, err := daemonProcessStartTime(record.PID)
	if err != nil || !actualStart.Equal(record.StartedAt) {
		return
	}
	if wait.total == 0 {
		// The first observation pins the generation, finite snapshot and current
		// position. Old completed work cannot buy a fresh waiting window.
		wait.pid, wait.startedAt = record.PID, record.StartedAt
		wait.snapshot = work.Snapshot
		wait.total, wait.completed = work.Total, work.Completed
		return
	}
	wait.completed = work.Completed
	wait.deadline = now.Add(wait.timeout)
	if wait.deadline.After(wait.hardDeadline) {
		wait.deadline = wait.hardDeadline
	}
}
