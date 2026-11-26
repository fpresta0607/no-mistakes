package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// preservedWorktrees remembers the default-tree run worktrees discovery last
// decided to keep, each under the freshness key it was decided at, so a
// worktree kept for a reason that has not changed is not observed with Git
// again on every finished run's sweep (issue #367). A long-lived daemon keeps
// dozens of such worktrees, and re-observing each one started Git processes
// that competed with live runs for the disk.
//
// Only a keep decision is remembered, and it is never a removal decision: a
// worktree discovery finds removable is decided again by reapWorktrees and by
// removeOrphanWorktree after its process sweep. A stale key can therefore keep
// a worktree longer than necessary, never remove one. The memory is the
// RunManager's, so a daemon restart observes every leftover once again.
type preservedWorktrees struct {
	mu   sync.Mutex
	keys map[string]string // worktree directory -> freshness key at the keep decision
}

// removable is removableOrphanWorktree for discovery: while wt's freshness key
// is the one it was kept at, the keep decision stands without being observed
// again.
func (preserved *preservedWorktrees) removable(d *db.DB, wt orphanWorktree) bool {
	key, hasKey := worktreeFreshnessKey(d, wt)
	preserved.mu.Lock()
	isUnchanged := hasKey && preserved.keys[wt.dir] == key
	preserved.mu.Unlock()
	if isUnchanged {
		return false
	}
	isRemovable := removableOrphanWorktree(d, wt)
	preserved.mu.Lock()
	defer preserved.mu.Unlock()
	if isRemovable || !hasKey {
		delete(preserved.keys, wt.dir)
		return isRemovable
	}
	if preserved.keys == nil {
		preserved.keys = make(map[string]string)
	}
	preserved.keys[wt.dir] = key
	return false
}

// forgetOthers drops every remembered worktree outside visited, the complete
// set of directories a walk of the default tree just saw, so the memory never
// outgrows the worktrees that still exist.
func (preserved *preservedWorktrees) forgetOthers(visited map[string]bool) {
	preserved.mu.Lock()
	defer preserved.mu.Unlock()
	for dir := range preserved.keys {
		if !visited[dir] {
			delete(preserved.keys, dir)
		}
	}
}

// worktreeFreshnessKey digests every input of removableOrphanWorktree's
// decision that can be read without starting a process: the run's database
// state, and the Git metadata of its worktree and preservation anchors. It
// reports false when an input cannot be read, so nothing is remembered.
//
// What it cannot see is a working file edited in place below the worktree's top
// directory. Such an edit alone keeps a kept worktree kept until another input
// moves or the daemon restarts; it can never remove one.
func worktreeFreshnessKey(d *db.DB, wt orphanWorktree) (string, bool) {
	run, err := d.GetRun(wt.runID)
	if err != nil || run == nil {
		return "", false
	}
	mayContinue, err := d.SourceMayContinue(run)
	if err != nil {
		return "", false
	}
	adoption, err := d.GetTerminalAdoption(run.ID)
	if err != nil {
		return "", false
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		return "", false
	}
	anchors := []string{custody.RecoveryRef(run.ID)}
	if adoption != nil {
		anchors = append(anchors, adoption.SourceRef)
	}
	// The decision records its own source observation on the run. That is its
	// output, not an input, so it must not make the next sweep observe again.
	run.TerminalSourceState = nil
	payload, err := json.Marshal(struct {
		Run         *db.Run
		MayContinue bool
		Adoption    *db.TerminalAdoption
		Steps       []*db.StepResult
		Metadata    []string
	}{run, mayContinue, adoption, steps, worktreeMetadataStats(wt, anchors)})
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), true
}

// worktreeMetadataStats describes the files Git reads to observe wt: its top
// directory (whose entries change when a file is added or removed there), its
// .git link, the HEAD and index of its own Git directory, and the loose refs of
// its symbolic HEAD and preservation anchors in the gate. Git replaces each of
// these files by rename when it changes one, and a packed ref that changes is
// written loose again, so a change shows in these stats.
func worktreeMetadataStats(wt orphanWorktree, anchors []string) []string {
	files := []string{wt.dir, filepath.Join(wt.dir, ".git")}
	if gitDir := worktreeGitDir(wt.dir); gitDir != "" {
		head := filepath.Join(gitDir, "HEAD")
		files = append(files, head, filepath.Join(gitDir, "index"))
		if content, err := os.ReadFile(head); err == nil {
			if target, isSymbolic := strings.CutPrefix(strings.TrimSpace(string(content)), "ref: "); isSymbolic {
				anchors = append(anchors, target)
			}
		}
	}
	for _, ref := range anchors {
		files = append(files, filepath.Join(wt.gateDir, filepath.FromSlash(ref)))
	}
	stats := make([]string, 0, len(files))
	for _, file := range files {
		info, err := os.Lstat(file)
		if err != nil {
			stats = append(stats, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		stats = append(stats, fmt.Sprintf("%s: %s %d %d", file, info.Mode(), info.Size(), info.ModTime().UnixNano()))
	}
	return stats
}

// worktreeGitDir is the Git directory of the checkout at dir: its .git
// directory, or the directory a linked worktree's .git file names. It is ""
// when dir has neither.
func worktreeGitDir(dir string) string {
	link := filepath.Join(dir, ".git")
	info, err := os.Lstat(link)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return link
	}
	content, err := os.ReadFile(link)
	if err != nil {
		return ""
	}
	gitDir, isLink := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir: ")
	if !isLink {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	return filepath.Clean(gitDir)
}
