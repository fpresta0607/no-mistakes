package pipeline

import (
	"context"
	"fmt"

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
