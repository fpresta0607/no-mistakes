package daemon

import (
	"crypto/rand"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

type startupWork struct {
	Snapshot  string `json:"snapshot"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
}

type startupProgress struct {
	path      string
	record    daemonPIDFile
	remaining map[string]struct{}
}

func newStartupProgress(path string, record daemonPIDFile, runs []*db.Run) *startupProgress {
	remaining := make(map[string]struct{})
	for _, phase := range []string{"orphan_servers", "gate_migration", "terminal_pr_runs", "parked_runs", "stale_runs", "orphan_processes", "worktree_cleanup", "evidence_cleanup", "ipc_bind"} {
		remaining[phase] = struct{}{}
	}
	for _, run := range runs {
		remaining["run:"+run.ID] = struct{}{}
	}
	record.Startup = &startupWork{Snapshot: rand.Text(), Total: len(remaining)}
	return &startupProgress{path: path, record: record, remaining: remaining}
}

func (progress *startupProgress) complete(work string) {
	if progress == nil {
		return
	}
	if _, exists := progress.remaining[work]; !exists {
		return
	}
	delete(progress.remaining, work)
	progress.record.Startup.Completed++
	if err := writeDaemonPIDFile(progress.path, progress.record); err != nil {
		slog.Warn("publish daemon startup progress", "work", work, "error", err)
	}
}
