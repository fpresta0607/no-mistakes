package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func runAxiCIRevalidation(cmd *cobra.Command, env *axiEnv, ctx context.Context, runID string, view runView, args respondArgs) error {
	if args.autoYes || args.findings != "" || args.instructions != "" || args.addFinding != "" || (args.step != "" && args.step != string(types.StepCI)) {
		return emitError(cmd, 2, "CI revalidation cannot be combined with automatic decisions or finding actions")
	}
	if _, err := hex.DecodeString(args.candidate); err != nil || (len(args.candidate) != 40 && len(args.candidate) != 64) || args.candidate != strings.ToLower(args.candidate) || strings.TrimSpace(args.reason) == "" {
		return emitError(cmd, 2, "CI revalidation requires --candidate <full commit SHA> and --reason <operator adoption reason>")
	}
	if !filepath.IsAbs(args.ghConfigDir) || strings.TrimSpace(args.expectedLogin) == "" {
		return emitError(cmd, 2, "select existing publication credentials with --publication-gh-config-dir <absolute directory> and --publication-login <login>")
	}
	snapshot, err := env.d.ReadCIRepairSnapshot(runID)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("read original CI binding: %v", err))
	}
	isReplay := false
	if !args.reselectPublication {
		accepted, err := env.d.GetCIRepairAdoption(runID)
		if err != nil {
			return emitError(cmd, 1, fmt.Sprintf("read adoption history: %v", err))
		}
		if accepted != nil {
			snapshot = accepted
			isReplay = true
		}
	}
	digest, err := pipeline.CIRepairSnapshotDigest(snapshot)
	if err != nil {
		return emitError(cmd, 1, "cannot bind CI revalidation snapshot")
	}
	caller, err := os.Getwd()
	if err != nil {
		return emitError(cmd, 1, "cannot resolve caller directory")
	}
	head, err := git.HeadSHA(ctx, caller)
	if err != nil {
		return emitError(cmd, 1, "cannot resolve caller head")
	}
	request := &ipc.RespondParams{RunID: runID, Step: types.StepCI, Action: types.ActionRevalidate,
		CIRevalidation: &ipc.CIRevalidationParams{CandidateSHA: args.candidate, SnapshotDigest: digest, Reason: args.reason, CallerDir: caller, CallerHeadSHA: head, GHConfigDir: args.ghConfigDir, ExpectedLogin: args.expectedLogin, ReselectPublication: args.reselectPublication}}
	if err := env.client.CallWithContext(ctx, ipc.MethodRespond, request, nil, 0); err != nil {
		if isAxiWaitElapsed(cmd.Context(), ctx, err) {
			return emitAxiWaitElapsed(cmd, args.wait, "no-mistakes axi status")
		}
		return emitError(cmd, 1, fmt.Sprintf("revalidate retained CI contents: %v", err))
	}
	if gate, ok := view.awaitingStep(); ok && !isReplay && (!args.reselectPublication || gate.Name == string(types.StepPush)) {
		if err := waitStepLeavesGate(ctx, env.p.Socket(), runID, gate.Name, gateIdentityFor(view, gate.Name)); err != nil {
			if isAxiWaitElapsed(cmd.Context(), ctx, err) {
				return emitAxiWaitElapsed(cmd, args.wait, "no-mistakes axi status")
			}
			return emitError(cmd, 1, fmt.Sprintf("wait for revalidation: %v", err))
		}
	}
	final, ciReady, err := driveRun(ctx, cmd.ErrOrStderr(), env.client, env.p.Socket(), runID, false)
	if err != nil {
		if isAxiWaitElapsed(cmd.Context(), ctx, err) {
			return emitAxiWaitElapsed(cmd, args.wait, "no-mistakes axi status")
		}
		return emitError(cmd, 1, fmt.Sprintf("follow fresh CI validation: %v", err))
	}
	return renderDriveResult(cmd, final, ciReady)
}
