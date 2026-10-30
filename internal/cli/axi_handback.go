package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func newAxiHandbackCmd() *cobra.Command {
	var shouldResume bool
	var wait time.Duration
	command := &cobra.Command{
		Use:   "handback",
		Short: "Return a published CI conflict to its exact clean caller",
		Long: "Release a parked published CI merge conflict with automatic CI repair disabled.\n" +
			"The same run, PR, findings and repair budgets remain unresolved. Synchronize\n" +
			"the clean caller to the exact published head first with axi sync.\n\n" +
			"Resolve the conflict through an additive merge that preserves the released\n" +
			"head and the current PR base, commit it, then use --resume from the same\n" +
			"worktree to submit that head to the same run for fresh validation from Review.\n" +
			"No fixer runs during release. No approval, skip, rebase or force-push is used.\n" +
			"GitHub head verification is required. --resume follows the next decision\n" +
			"point with the usual bounded --wait.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateAxiWait(wait); err != nil {
				return emitError(command, 2, err.Error())
			}
			env, err := openAxiDaemonEnv()
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			defer env.close()
			ctx := command.Context()
			caller, err := os.Getwd()
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			branch, err := git.CurrentBranch(ctx, caller)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			head, err := git.HeadSHA(ctx, caller)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			var active ipc.GetActiveRunResult
			if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
				return emitError(command, 1, err.Error())
			}
			if active.Run == nil {
				return emitError(command, 1, "no active run on the current branch to hand back")
			}
			var result ipc.CIHandbackResult
			if err := env.client.CallWithContext(ctx, ipc.MethodCIHandback, ipc.CIHandbackParams{RepoID: env.repo.ID, RunID: active.Run.ID, Branch: branch, CallerDir: caller, HeadSHA: head, Resume: shouldResume}, &result, 95*time.Second); err != nil {
				return emitError(command, 1, fmt.Sprintf("CI handback refused: %v", err))
			}
			fields := []toon.Field{{Key: "run", Value: result.RunID}, {Key: "pr", Value: result.PRURL}, {Key: "released_head", Value: result.ReleasedHead}}
			if !shouldResume {
				fields = append(fields, toon.Field{Key: "handback", Value: "released"}, toon.Field{Key: "help", Value: []string{"Resolve the conflict with an additive merge, then run no-mistakes axi handback --resume from this worktree.", "The CI conflict remains unresolved; readiness requires fresh validation of the submitted head."}})
				emitDoc(command, fields...)
				return nil
			}
			driveContext, cancel, err := boundAxiWait(ctx, wait)
			if err != nil {
				return emitError(command, 2, err.Error())
			}
			defer cancel()
			if err := waitStepLeavesGate(driveContext, env.p.Socket(), result.RunID, "ci", gateIdentityFor(runViewFromIPC(active.Run), "ci")); err != nil {
				if isAxiWaitElapsed(ctx, driveContext, err) {
					return emitAxiWaitElapsed(command, wait, "no-mistakes axi run")
				}
				return emitError(command, 1, err.Error())
			}
			run, isReady, err := driveRun(driveContext, command.ErrOrStderr(), env.client, env.p.Socket(), result.RunID, false)
			if err != nil {
				if isAxiWaitElapsed(ctx, driveContext, err) {
					return emitAxiWaitElapsed(command, wait, "no-mistakes axi run")
				}
				return emitError(command, 1, err.Error())
			}
			return renderDriveResult(command, run, isReady, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
		},
	}
	command.Flags().BoolVar(&shouldResume, "resume", false, "submit the additive follow-up for fresh validation in the same run")
	bindAxiWaitFlag(command, &wait)
	return command
}
