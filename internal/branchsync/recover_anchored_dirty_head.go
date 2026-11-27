package branchsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// recordedHeadIsTheRunsAnchoredDirtyHead is the proof for issue #262: a run
// whose Rebase moved its head and which then failed with dirty source in its
// managed worktree (a Review fix agent killed mid-edit). Terminalization
// observed the managed worktree exactly at the recorded head, pinned that head
// at the run's own recovery ref, and recorded it with the "dirty:<head>"
// source observation; only the dirty source left it unverified. Neither other
// proof can apply: the rebased head is a rewrite of the gate branch, not a
// descendant, and a run that failed in Review has no review authority.
//
// All facts are read fresh: the source observation from the run row and the
// anchor as an exact, non-symbolic gate ref naming a commit the gate holds. The
// managed worktree still holding that dirty source at the recorded head is
// read by unverifiedTerminalHeadShape before any shape is chosen
// (managedSourceToPreserve).
// This proves only which head the run reached. It is never containment
// evidence: the operator's local work is still proven present by ancestry or
// the merge-tree proof before the branch moves.
func (s *Service) recordedHeadIsTheRunsAnchoredDirtyHead(ctx context.Context, run *db.Run) bool {
	recorded := strings.TrimSpace(run.HeadSHA)
	if recorded == "" || run.TerminalSourceState == nil || *run.TerminalSourceState != "dirty:"+recorded {
		return false
	}
	anchor := custody.RecoveryRef(run.ID)
	if symbolic, err := git.Run(ctx, s.GateDir, "symbolic-ref", "-q", anchor); err == nil && symbolic != "" {
		return false
	}
	anchored, exists, err := git.ExactRefTarget(ctx, s.GateDir, anchor)
	return err == nil && exists && anchored == recorded && objectExists(ctx, s.GateDir, recorded)
}

// managedSourceToPreserve is the one read of a terminal run's managed source
// that every unverified-head shape rests on (issue #414). It reports whether
// that source holds an edit preserveDirtyManagedSource must commit before
// custody returns, or why recovery cannot know. A "dirty:<recorded head>"
// observation needs the run's own managed worktree still at that head and
// still dirty; a dirty observation of any other head cannot be bound to the
// recorded one. A source observed clean at the recorded head holds nothing. Any
// other observation (unreadable, absent, or clean at another head) proves
// nothing about the source, so the managed worktree is read live: a missing one
// holds no edit, one at the recorded head is preserved when dirty, and one that
// exists but cannot be read there refuses.
func (s *Service) managedSourceToPreserve(ctx context.Context, run *db.Run) (bool, error) {
	recorded := strings.TrimSpace(run.HeadSHA)
	observed := ""
	if run.TerminalSourceState != nil {
		observed = *run.TerminalSourceState
	}
	if strings.HasPrefix(observed, "dirty:") {
		if observed != "dirty:"+recorded {
			return false, fmt.Errorf("the terminal run recorded dirty managed source (%s) at another head than its recorded head %s, so that source cannot be preserved before custody returns", observed, recorded)
		}
		managed, err := s.managedSourceAtRecordedHead(ctx, run)
		if err != nil {
			return false, fmt.Errorf("the terminal run recorded dirty managed source (%s), but it cannot be preserved before custody returns: %v", observed, err)
		}
		if clean, _ := terminalSourceClean(ctx, managed); clean {
			return false, fmt.Errorf("the terminal run recorded dirty managed source (%s), but its managed worktree %s no longer holds it", observed, managed)
		}
		return true, nil
	}
	if recorded != "" && observed == "clean:"+recorded {
		return false, nil
	}
	if s.Paths != nil {
		if _, err := os.Lstat(worktrees.RecordedDir(s.Paths, run.WorktreePath(), run.RepoID, run.ID)); os.IsNotExist(err) {
			return false, nil
		}
	}
	managed, err := s.managedSourceAtRecordedHead(ctx, run)
	if err != nil {
		return false, fmt.Errorf("the terminal run's managed source was never observed clean at its recorded head %s, and its managed worktree still exists but cannot be read there (%v), so an edit it may hold cannot be preserved before custody returns; make that worktree readable at the recorded head, or remove it once anything it holds is saved, then re-run recovery", recorded, err)
	}
	clean, _ := terminalSourceClean(ctx, managed)
	return !clean, nil
}

// preserveDirtyManagedSource runs before any unverified-head proof stamps a run
// whose managed source managedSourceToPreserve reads as dirty (issue #414). The dirty source exists only
// in the managed worktree, which custody return makes eligible for removal, so
// it is committed on top of the recorded head and anchored create-only at the
// run's recovery source ref. It returns the refusal to report when either
// fails.
func (s *Service) preserveDirtyManagedSource(ctx context.Context, state State, run *db.Run) (State, bool) {
	snapshot, err := s.commitManagedDirtySource(ctx, run)
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", fmt.Sprintf("the dirty managed source of the terminal run at its recorded head %s cannot be preserved: %v; no files or refs were changed", run.HeadSHA, err)), false
	}
	sourceRef := custody.RecoverySourceRef(run.ID)
	if err := custody.PreserveRecoveryAnchor(ctx, s.GateDir, sourceRef, snapshot); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", fmt.Sprintf("the dirty managed source could not be anchored at %s: %v; no files or worktree refs were changed", sourceRef, err)), false
	}
	return State{}, true
}

// verifyAnchoredDirtyTerminalHead records the proof from
// recordedHeadIsTheRunsAnchoredDirtyHead once Recover has preserved the dirty
// source: it re-reads the proof and stamps the head verified with a
// compare-and-swap on the source observation. It returns the refusal to report
// when either fails.
func (s *Service) verifyAnchoredDirtyTerminalHead(ctx context.Context, state State, run *db.Run) (State, bool) {
	sourceRef := custody.RecoverySourceRef(run.ID)
	if !s.recordedHeadIsTheRunsAnchoredDirtyHead(ctx, run) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", fmt.Sprintf("the run recovery ref changed while the recorded head was being verified; the dirty managed source remains anchored at %s and custody was not returned", sourceRef)), false
	}
	updated, err := s.DB.VerifyTerminalRunAnchoredDirtyHead(run.ID, run.Status, run.HeadSHA, *run.TerminalSourceState)
	if err != nil || !updated {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", fmt.Sprintf("the terminal run or its source observation changed while the recorded head was being verified; the dirty managed source remains anchored at %s and custody was not returned", sourceRef)), false
	}
	return State{}, true
}

// commitManagedDirtySource commits the run's managed worktree content - tracked
// changes, staged files and untracked files that are not ignored - on top of
// the recorded head through a scratch index, leaving the worktree, its index
// and every ref untouched. The identity and dates are fixed by the run, so a
// repeated recovery of the same content produces the same commit and the
// create-only anchor accepts it.
func (s *Service) commitManagedDirtySource(ctx context.Context, run *db.Run) (string, error) {
	managed, err := s.managedSourceAtRecordedHead(ctx, run)
	if err != nil {
		return "", err
	}
	head := run.HeadSHA
	if clean, _ := terminalSourceClean(ctx, managed); clean {
		return "", fmt.Errorf("the managed worktree no longer holds the dirty source it was recorded with")
	}
	indexDir, err := os.MkdirTemp("", "no-mistakes-recover-source-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(indexDir)
	date := fmt.Sprintf("@%d +0000", run.CreatedAt)
	env := []string{
		"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index"),
		"GIT_AUTHOR_NAME=no-mistakes", "GIT_AUTHOR_EMAIL=no-mistakes@localhost", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=no-mistakes", "GIT_COMMITTER_EMAIL=no-mistakes@localhost", "GIT_COMMITTER_DATE=" + date,
	}
	if _, err := git.RunWithEnv(ctx, managed, env, "read-tree", head); err != nil {
		return "", fmt.Errorf("read the recorded head into a scratch index: %w", err)
	}
	if _, err := git.RunWithEnv(ctx, managed, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("stage the managed source into a scratch index: %w", err)
	}
	tree, err := git.RunWithEnv(ctx, managed, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write the managed source tree: %w", err)
	}
	commit, err := git.RunWithEnv(ctx, managed, env, "commit-tree", tree, "-p", head, "-m", "no-mistakes: preserve the dirty managed source of run "+run.ID)
	if err != nil {
		return "", fmt.Errorf("commit the managed source: %w", err)
	}
	return commit, nil
}

// managedSourceAtRecordedHead is the one fresh read of a terminal run's own
// managed worktree that both managed-source proofs rest on: it must exist,
// share this run's gate, and sit exactly at the recorded head. It returns the
// worktree's directory; the caller decides what its cleanliness proves.
func (s *Service) managedSourceAtRecordedHead(ctx context.Context, run *db.Run) (string, error) {
	if s.Paths == nil {
		return "", fmt.Errorf("the managed worktree location is unknown")
	}
	managed := worktrees.RecordedDir(s.Paths, run.WorktreePath(), run.RepoID, run.ID)
	if _, err := os.Lstat(filepath.Join(managed, ".git")); err != nil {
		return "", fmt.Errorf("the managed worktree %s is missing or unreadable", managed)
	}
	common, err := git.Run(ctx, managed, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("the managed worktree %s cannot be read", managed)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(managed, common)
	}
	if worktrees.Canonical(common) != worktrees.Canonical(s.GateDir) {
		return "", fmt.Errorf("the managed worktree %s does not belong to this run's gate", managed)
	}
	head, err := git.HeadSHA(ctx, managed)
	if err != nil || head != run.HeadSHA {
		return "", fmt.Errorf("the managed worktree is no longer at the recorded head")
	}
	return managed, nil
}
