package branchsync

import (
	"context"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// recordedHeadIsTheRunsObservedCleanHead is the proof for issue #364: a run
// whose terminalization could not observe its managed worktree at all (on a
// loaded host its bounded read of the worktree head timed out after Test failed
// on infrastructure). It anchored nothing, verified nothing and recorded no
// source state, while the daemon retained the managed worktree. Neither other
// proof can apply when the head was rebased: it is a rewrite of the gate branch,
// not a descendant, and its tree differs from the gate's.
//
// Recovery makes the observation terminalization could not: the run's own
// managed worktree, sharing this gate, sits exactly at the recorded head and is
// clean, so the recorded head is exactly what the run reached with nothing
// beyond it. A recorded observation may only agree: absent, unreadable, or
// clean at this same head. A recorded dirty source belongs to the anchored
// dirty proof, and a source recorded anywhere else contradicts the head. Like
// that proof, this proves only which head the run reached and is never
// containment evidence: the operator's local work is still proven present by
// ancestry or the merge-tree proof before the branch moves.
func (s *Service) recordedHeadIsTheRunsObservedCleanHead(ctx context.Context, state State, run *db.Run) bool {
	recorded := strings.TrimSpace(run.HeadSHA)
	if !state.Local.Clean || recorded == "" || !objectExists(ctx, s.GateDir, recorded) {
		return false
	}
	if observed := run.TerminalSourceState; observed != nil && *observed != "unreadable" && *observed != "clean:"+recorded {
		return false
	}
	if compatible, err := recoveryAnchorCompatible(ctx, s.GateDir, run.ID, recorded); err != nil || !compatible {
		return false
	}
	managed, err := s.managedSourceAtRecordedHead(ctx, run)
	if err != nil {
		return false
	}
	clean, _ := terminalSourceClean(ctx, managed)
	return clean
}

// verifyObservedCleanTerminalHead records the proof from
// recordedHeadIsTheRunsObservedCleanHead: it anchors the recorded head at the
// run's create-only recovery ref, then stamps the clean observation and the
// verified head with a compare-and-swap on the source observation the proof
// read. It returns the refusal to report when any of that fails.
func (s *Service) verifyObservedCleanTerminalHead(ctx context.Context, state State, run *db.Run, gateHead string) (State, bool) {
	if blocked, ok := s.anchorUnverifiedTerminalHead(ctx, state, run, gateHead, run.HeadSHA, state.Local.Head); !ok {
		return blocked, false
	}
	updated, err := s.DB.VerifyTerminalRunObservedCleanHead(run.ID, run.Status, run.HeadSHA, run.TerminalSourceState)
	if err != nil || !updated {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the terminal run or its source observation changed while the recorded head was being verified; the preserved recovery ref remains available and custody was not returned"), false
	}
	return State{}, true
}
