package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ErrAgentInvocationFailed marks an error an agent invocation itself
// returned while its context was still live: the provider, the transport, or
// the agent process failed. A timeout, a cancellation, and anything a step
// concluded from a successful invocation's output never carry it.
var ErrAgentInvocationFailed = errors.New("agent invocation failed")

type agentInvocationFailure struct{ err error }

func (f *agentInvocationFailure) Error() string { return f.err.Error() }

func (f *agentInvocationFailure) Unwrap() error { return f.err }

func (f *agentInvocationFailure) Is(target error) bool { return target == ErrAgentInvocationFailed }

// ErrPreparationEnvironment marks a dependency preparation that failed for its
// environment rather than for the change. Preparation marks it only when every
// part of its failure is the environment's, so a prepare command's own
// failure about the change never carries it.
var ErrPreparationEnvironment = errors.New("dependency preparation failed for its environment")

type preparationEnvironmentFailure struct{ err error }

func (f *preparationEnvironmentFailure) Error() string { return f.err.Error() }

func (f *preparationEnvironmentFailure) Unwrap() error { return f.err }

func (f *preparationEnvironmentFailure) Is(target error) bool {
	return target == ErrPreparationEnvironment
}

// PreparationEnvironmentFailure marks err as ErrPreparationEnvironment and
// keeps its text unchanged.
func PreparationEnvironmentFailure(err error) error {
	return &preparationEnvironmentFailure{err: err}
}

// invocationFailureAgent is the innermost step-agent decorator, so it sees the
// adapter's own error under the invocation's own context: a deadline or a
// cancellation has already ended that context by the time its error returns,
// which is how both stay unmarked.
type invocationFailureAgent struct {
	inner agent.Agent
}

func (a *invocationFailureAgent) Name() string { return a.inner.Name() }

func (a *invocationFailureAgent) Close() error { return a.inner.Close() }

func (a *invocationFailureAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	result, err := a.inner.Run(ctx, opts)
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	return result, &agentInvocationFailure{err: err}
}

func (a *invocationFailureAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *invocationFailureAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *invocationFailureAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *invocationFailureAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

// RetryCheckpoint is the boundary a failed run continues from: its failed
// step restarts from the beginning, and the steps before it are carried.
type RetryCheckpoint struct {
	// Index is the failed step's position in the pipeline.
	Index int
	// Steps holds every step record of the failed run, in pipeline order.
	Steps []*db.StepResult
	// Round is the failed step's round that was in flight when it failed.
	Round int
}

// Failed is the failed step's record.
func (c *RetryCheckpoint) Failed() *db.StepResult { return c.Steps[c.Index] }

// Retry proofs PlanRetryCheckpoint can fail.
const (
	RetryProofCheckpoint = "durable checkpoint"
	RetryProofCause      = "eligible failure"
	RetryProofEvidence   = "reusable evidence"
)

// RetryRefusal is a failed retry proof: which one, and why.
type RetryRefusal struct {
	Proof  string
	Reason string
}

func (r *RetryRefusal) Error() string { return r.Reason }

func refuseCheckpoint(proof, format string, args ...any) error {
	return &RetryRefusal{Proof: proof, Reason: fmt.Sprintf(format, args...)}
}

// PlanRetryCheckpoint proves that run's record names exactly one safe
// boundary to continue from, against the step sequence steps, and refuses
// with a *RetryRefusal naming the first proof that fails. The record must hold one row per step in
// that order: completed or skipped rows, then exactly one failed row whose
// recorded cause is eligible, then rows that never ran. An agent invocation
// failure is eligible at any step; a preparation that failed for its
// environment only at Test, after a carried Review approved exactly the head
// the continuation starts from. No carried row may be Test or a later step,
// because Test's evidence and every later step's work are not kept after a
// run ends.
func PlanRetryCheckpoint(database *db.DB, run *db.Run, steps []Step) (*RetryCheckpoint, error) {
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("read the run's step records: %w", err)
	}
	if len(rows) != len(steps) {
		return nil, refuseCheckpoint(RetryProofCheckpoint, "the run recorded %d steps but its pipeline has %d", len(rows), len(steps))
	}
	failed := -1
	for index, row := range rows {
		if row.StepName != steps[index].Name() {
			return nil, refuseCheckpoint(RetryProofCheckpoint, "the run's step %d is %q, but the pipeline's is %q", index, row.StepName, steps[index].Name())
		}
		switch {
		case row.Status == types.StepStatusFailed:
			if failed >= 0 {
				return nil, refuseCheckpoint(RetryProofCheckpoint, "steps %s and %s both failed", rows[failed].StepName, row.StepName)
			}
			failed = index
		case failed < 0 && row.Status != types.StepStatusCompleted && row.Status != types.StepStatusSkipped:
			return nil, refuseCheckpoint(RetryProofCheckpoint, "step %s is %s before the failed step", row.StepName, row.Status)
		case failed >= 0 && row.Status != types.StepStatusPending && row.Status != types.StepStatusSkipped:
			return nil, refuseCheckpoint(RetryProofCheckpoint, "step %s is %s after the failed step", row.StepName, row.Status)
		}
	}
	if failed < 0 {
		return nil, refuseCheckpoint(RetryProofCheckpoint, "no step failed")
	}
	row := rows[failed]
	if err := proveRetryCause(run, rows[:failed], row); err != nil {
		return nil, err
	}
	for _, carried := range rows[:failed] {
		if carried.StepName.Order() >= types.StepTest.Order() {
			return nil, refuseCheckpoint(RetryProofEvidence, "step %s failed after %s, and a continuation cannot carry %s or any later step because their evidence is not kept after a run ends", row.StepName, carried.StepName, types.StepTest)
		}
	}
	rounds, err := database.GetRoundsByStep(row.ID)
	if err != nil {
		return nil, fmt.Errorf("read step %s rounds: %w", row.StepName, err)
	}
	return &RetryCheckpoint{Index: failed, Steps: rows, Round: len(rounds) + 1}, nil
}

// proveRetryCause requires the failed step's recorded cause to place its
// failure outside the change, and names that cause when it refuses. A Test
// preparation that failed for its environment continues only at exactly the
// head a carried Review approved, so no route lets an unreviewed head through.
func proveRetryCause(run *db.Run, carried []*db.StepResult, failed *db.StepResult) error {
	recorded := "no failure cause"
	cause := ""
	if failed.FailureCause != nil {
		cause = *failed.FailureCause
		recorded = "failure cause " + cause
	}
	switch cause {
	case db.StepFailureCauseAgentInvocation:
		return nil
	case db.StepFailureCausePreparationEnvironment:
		if failed.StepName != types.StepTest {
			return refuseCheckpoint(RetryProofCause, "step %s recorded %s, which only a failed Test continues from", failed.StepName, recorded)
		}
		reviewed := false
		for _, row := range carried {
			reviewed = reviewed || (row.StepName == types.StepReview && row.Status == types.StepStatusCompleted)
		}
		if !reviewed || run.ReviewApprovedHeadSHA == nil {
			return refuseCheckpoint(RetryProofCause, "step %s recorded %s, but no carried Review approved the head it would test", failed.StepName, recorded)
		}
		if *run.ReviewApprovedHeadSHA != run.HeadSHA {
			return refuseCheckpoint(RetryProofCause, "step %s recorded %s, but the run's head %s is not the head %s that Review approved", failed.StepName, recorded, run.HeadSHA, *run.ReviewApprovedHeadSHA)
		}
		return nil
	}
	return refuseCheckpoint(RetryProofCause, "step %s recorded %s, so the failure may be about the change itself; only an agent invocation failure (provider, transport, or agent process) or a Test preparation that failed for its environment continues", failed.StepName, recorded)
}
