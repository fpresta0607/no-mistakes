package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// isStepHandback reports whether axi handback addresses a parked Test or Lint
// failure rather than a published CI failure.
func isStepHandback(database *db.DB, runID string, isResume bool, receiptID string) (bool, error) {
	if isResume {
		receipt, err := database.GetStepHandback(receiptID)
		if err != nil || receipt == nil {
			return false, err
		}
		if receipt.RunID != runID {
			return false, fmt.Errorf("handback receipt does not identify this branch's current run")
		}
		return true, nil
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return false, err
	}
	for _, step := range steps {
		if (step.StepName == types.StepTest || step.StepName == types.StepLint) && step.Status == types.StepStatusAwaitingApproval {
			return true, nil
		}
	}
	return false, nil
}

func runAxiStepHandback(command *cobra.Command, env *axiEnv, active *ipc.RunInfo, request ipc.StepHandbackParams, wait time.Duration) error {
	ctx := command.Context()
	var result ipc.StepHandbackResult
	if err := env.client.CallWithContext(ctx, ipc.MethodStepHandback, request, &result, 95*time.Second); err != nil {
		return emitError(command, 1, fmt.Sprintf("handback refused: %v", err))
	}
	state := "released"
	if result.AcceptedHead != "" {
		state = "accepted"
	}
	receiptFields := []toon.Field{{Key: "id", Value: result.ReceiptID}, {Key: "run_id", Value: result.RunID}, {Key: "step", Value: string(result.Step)}, {Key: "round_id", Value: result.RoundID}, {Key: "caller_dir", Value: result.CallerDir}, {Key: "released_head", Value: result.ReleasedHead}, {Key: "released_ref", Value: result.ReleasedRef}, {Key: "state", Value: state}}
	if result.AcceptedHead != "" {
		receiptFields = append(receiptFields, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
	}
	receiptField := toon.Field{Key: "receipt", Value: toon.NewObject(receiptFields...)}
	fields := []toon.Field{{Key: "run", Value: result.RunID}, {Key: "step", Value: string(result.Step)}, {Key: "released_head", Value: result.ReleasedHead}, receiptField, {Key: "handback", Value: state}}
	if !request.Resume {
		nextCode, nextCommand := "resume_step_handback", "no-mistakes axi handback --resume --receipt "+result.ReceiptID
		if request.HeadSHA != result.ReleasedHead {
			nextCode, nextCommand = "integrate_step_handback", "git merge --ff-only "+result.ReleasedRef
		}
		fields = append(fields, toon.Field{Key: "next_action", Value: toon.NewObject(toon.Field{Key: "code", Value: nextCode}, toon.Field{Key: "command", Value: nextCommand})}, toon.Field{Key: "help", Value: []string{
			"Commit exactly one correction on the released head, then resume from this worktree with this receipt.",
			fmt.Sprintf("The %s failure stays unresolved until this run validates that commit from %s.", result.Step, result.Step),
		}})
		emitDoc(command, fields...)
		return nil
	}
	fields = append(fields, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
	driveContext, cancel, err := boundAxiWait(ctx, wait)
	if err != nil {
		return emitError(command, 2, err.Error())
	}
	defer cancel()
	emitFollowError := func(err error) error {
		if isAxiWaitElapsed(ctx, driveContext, err) {
			return emitAxiWaitElapsed(command, wait, "no-mistakes axi run", fields...)
		}
		emitDoc(command, append(fields, toon.Field{Key: "error", Value: err.Error()})...)
		return &exitError{code: 1}
	}
	if err := waitStepLeavesGate(driveContext, env.p.Socket(), result.RunID, string(result.Step), gateIdentityFor(runViewFromIPC(active), string(result.Step))); err != nil {
		return emitFollowError(err)
	}
	run, isReady, err := driveRun(driveContext, command.ErrOrStderr(), env.client, env.p.Socket(), result.RunID, false)
	if err != nil {
		return emitFollowError(err)
	}
	return renderDriveResult(command, run, isReady, toon.Field{Key: "handback", Value: state}, receiptField, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
}
