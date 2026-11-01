package pipeline

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const actionCIFollowup types.ApprovalAction = "ci-followup"

type CIHandbackStep interface {
	HandbackCI(*StepContext, ipc.CIHandbackParams) (*ipc.CIHandbackResult, error)
}

type ciHandbackRequest struct {
	params ipc.CIHandbackParams
	ctx    context.Context
	reply  chan ciHandbackReply
}

type ciHandbackReply struct {
	result *ipc.CIHandbackResult
	err    error
}

// HandbackCI is serialized with the gate's own reconciliation and responses.
// The CI step alone verifies and changes custody, while its executor has no writer.
func (e *Executor) HandbackCI(ctx context.Context, params ipc.CIHandbackParams) (*ipc.CIHandbackResult, error) {
	e.mu.Lock()
	if !e.waiting || e.waitingStep != types.StepCI || e.ciHandbackPending {
		e.mu.Unlock()
		return nil, fmt.Errorf("CI handback requires an idle parked CI gate")
	}
	e.ciHandbackPending = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.ciHandbackPending = false; e.mu.Unlock() }()
	request := ciHandbackRequest{params: params, ctx: ctx, reply: make(chan ciHandbackReply, 1)}
	select {
	case e.ciHandbackCh <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	reply := <-request.reply
	return reply.result, reply.err
}

func (e *Executor) ciHandbackOpen(runID string) (bool, error) {
	receipt, err := e.db.LatestCIHandback(runID)
	return receipt != nil && receipt.AcceptedHead == nil, err
}

func CIHandbackRecoveryPending(database *db.DB, runID string) (bool, error) {
	receipt, err := database.LatestCIHandback(runID)
	if err != nil || receipt == nil {
		return err != nil, err
	}
	if receipt.AcceptedHead == nil || receipt.Candidate == nil {
		return true, nil
	}
	reviewRoundID, err := database.LatestReviewRoundID(runID)
	if err != nil || reviewRoundID == receipt.Candidate.ReviewRoundID {
		return true, err
	}
	results, err := database.GetStepsByRun(runID)
	if err != nil {
		return true, err
	}
	for _, result := range results {
		if result.StepName == types.StepReview {
			return result.Status == types.StepStatusPending || result.Status == types.StepStatusRunning, nil
		}
	}
	return true, nil
}

func CIHandbackReviewRestart(database *db.DB, run *db.Run, sequence []Step) (int, error) {
	receipt, err := database.LatestCIHandback(run.ID)
	if err != nil {
		return -1, err
	}
	if receipt == nil || receipt.AcceptedHead == nil {
		return -1, nil
	}
	isPending, err := CIHandbackRecoveryPending(database, run.ID)
	if err != nil || !isPending {
		return -1, err
	}
	if receipt.Candidate == nil || receipt.Candidate.Head != *receipt.AcceptedHead || run.HeadSHA != *receipt.AcceptedHead || run.ReviewApprovedHeadSHA != nil || run.CIReadyAt != nil || run.CIReadyNoCI || run.PushActive {
		return -1, fmt.Errorf("accepted CI handback checkpoint does not match the run")
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil || len(results) != len(sequence) {
		return -1, fmt.Errorf("accepted CI handback step sequence changed")
	}
	reviewIndex, ciIndex := -1, -1
	for index, result := range results {
		if result.StepName != sequence[index].Name() || result.AgentPID != nil {
			return -1, fmt.Errorf("accepted CI handback has changed steps or active writers")
		}
		if result.StepName == types.StepReview {
			reviewIndex = index
		}
		if result.StepName == types.StepCI {
			ciIndex = index
		}
	}
	if reviewIndex < 0 || ciIndex <= reviewIndex || ciIndex != len(results)-1 {
		return -1, fmt.Errorf("accepted CI handback has no full validation sequence")
	}
	ci := results[ciIndex]
	rounds, err := database.GetRoundsByStep(ci.ID)
	if err != nil || len(rounds) == 0 || rounds[len(rounds)-1].ID != receipt.RoundID {
		return -1, fmt.Errorf("accepted CI handback conflict checkpoint changed")
	}
	if ci.Status == types.StepStatusAwaitingApproval {
		latest := rounds[len(rounds)-1]
		if ci.FindingsJSON == nil || latest.FindingsJSON == nil || *ci.FindingsJSON != *latest.FindingsJSON || ci.AutoFixLimit != nil {
			return -1, fmt.Errorf("accepted CI handback conflict gate changed")
		}
		findings, err := types.ParseFindingsJSON(*ci.FindingsJSON)
		if err != nil {
			return -1, err
		}
		hasConflict := false
		for _, finding := range findings.Items {
			hasConflict = hasConflict || finding.Category == types.FindingCategoryCIMergeConflict
		}
		if !hasConflict {
			return -1, fmt.Errorf("accepted CI handback has no unresolved conflict")
		}
		for index, result := range results {
			if index != ciIndex && result.Status != types.StepStatusCompleted {
				return -1, fmt.Errorf("accepted CI handback validation checkpoint changed")
			}
		}
	} else {
		for index, result := range results {
			expected := types.StepStatusPending
			if index < reviewIndex {
				expected = types.StepStatusCompleted
			}
			if result.Status != expected && !(index == reviewIndex && result.Status == types.StepStatusRunning) {
				return -1, fmt.Errorf("accepted CI handback restart checkpoint changed")
			}
		}
	}
	return reviewIndex, nil
}
