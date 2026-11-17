package pipeline

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// actionStepHandback re-enters a parked Test or Lint step on its contributor's
// accepted correction. It is never an operator response.
const actionStepHandback types.ApprovalAction = "step-handback"

// StepHandbackStep is a Test or Lint step whose parked failure, with its
// automatic repair disabled, can go back to its contributor instead of to the
// gate's repair agent.
type StepHandbackStep interface {
	HandbackStep(*StepContext, ipc.StepHandbackParams) (*ipc.StepHandbackResult, error)
}

type stepHandbackRequest struct {
	params ipc.StepHandbackParams
	ctx    context.Context
	reply  chan stepHandbackReply
}

type stepHandbackReply struct {
	result *ipc.StepHandbackResult
	err    error
}

func isStepHandbackGate(name types.StepName) bool {
	return name == types.StepTest || name == types.StepLint
}

// HandbackStep is serialized with the gate's responses: the parked step alone
// verifies and moves the run, while no response can be delivered to it.
func (e *Executor) HandbackStep(ctx context.Context, params ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	e.mu.Lock()
	if !e.waiting || !isStepHandbackGate(e.waitingStep) || e.stepHandbackPending {
		e.mu.Unlock()
		return nil, fmt.Errorf("handback requires an idle parked Test or Lint gate")
	}
	e.stepHandbackPending = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.stepHandbackPending = false; e.mu.Unlock() }()
	request := stepHandbackRequest{params: params, ctx: ctx, reply: make(chan stepHandbackReply, 1)}
	select {
	case e.stepHandbackCh <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	reply := <-request.reply
	return reply.result, reply.err
}

// serveStepHandback runs inside the parked gate's wait and reports whether an
// accepted correction now re-enters the step.
func (e *Executor) serveStepHandback(ctx context.Context, step Step, sctx *StepContext, request stepHandbackRequest) bool {
	handler, ok := step.(StepHandbackStep)
	if !ok || !isStepHandbackGate(step.Name()) || e.autoFixLimit(step.Name()) != 0 {
		request.reply <- stepHandbackReply{err: fmt.Errorf("handback requires a parked Test or Lint gate with automatic repair disabled")}
		return false
	}
	if err := e.refuseValidationRepair(actionStepHandback); err != nil {
		request.reply <- stepHandbackReply{err: err}
		return false
	}
	copyContext := *sctx
	operationContext, cancel := context.WithCancel(request.ctx)
	stop := context.AfterFunc(ctx, cancel)
	copyContext.Ctx = operationContext
	result, err := handler.HandbackStep(&copyContext, request.params)
	stop()
	cancel()
	isResumed := err == nil && request.params.Resume
	if isResumed {
		e.mu.Lock()
		e.waiting = false
		e.mu.Unlock()
	}
	request.reply <- stepHandbackReply{result: result, err: err}
	return isResumed
}
