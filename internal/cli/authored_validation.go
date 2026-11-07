package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func newAxiValidateAuthoredCmd() *cobra.Command {
	return newValidationAdmissionCmd(false)
}

func newAxiValidateTerminalCmd() *cobra.Command {
	return newValidationAdmissionCmd(true)
}

func newValidationAdmissionCmd(isTerminal bool) *cobra.Command {
	var admissionFile string
	var wait time.Duration
	command := &cobra.Command{
		Use:   "validate-authored",
		Short: "Validate one declared authored candidate with an immutable terminal predecessor and zero repairs",
		Long:  "Admit an explicit clean authored commit/tree for entirely fresh validation.\nThe predecessor stays terminal, all automatic/manual repair capacity is zero,\nand historical dirty/untracked source completeness remains unknown. The supplied\nold policy is recorded as a controller assertion, never as a native historical fact.\nAn exact repeated request observes the same child; only an untouched pending\nacceptance may be explicitly started again after revalidation.",
		Args:  cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := validateAxiWait(wait); err != nil {
				return emitError(command, 2, err.Error())
			}
			if admissionFile == "" {
				return emitError(command, 2, "--admission-file is required")
			}
			request, err := authoredvalidation.Read(admissionFile)
			if err != nil {
				return emitError(command, 2, err.Error())
			}
			if (request.TerminalGeneration != "") != isTerminal {
				return emitError(command, 2, "use validate-terminal for a terminal_generation and validate-authored for a new authored candidate")
			}
			directory, err := os.Getwd()
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			directory, err = filepath.EvalSymlinks(directory)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			environment, err := openAxiDaemonEnv()
			if err != nil {
				return emitError(command, 1, err.Error(), repoInitHelp(err)...)
			}
			defer environment.close()
			if environment.repo.ID != request.RepoID {
				return emitError(command, 1, "admission file repository differs from the invoking repository")
			}
			var result ipc.ValidateAuthoredResult
			if err := environment.client.CallWithContext(command.Context(), ipc.MethodValidateAuthored, &ipc.ValidateAuthoredParams{Request: *request, CallerDir: directory}, &result, 2*time.Minute); err != nil {
				return emitError(command, 1, fmt.Sprintf("authored admission refused: %v", err), "Preserve the declared source and predecessor; inspect axi status before changing the admission request")
			}
			if result.Family == nil || result.Family.PredecessorID != request.PredecessorID || result.Family.ControllerPolicy != request.ControllerPolicy || result.Family.PolicyOrigin != "controller_assertion" || result.Family.HistoricalSourceCompleteness != "unknown" || result.Family.HasChildRepairs || result.Receipt.RunID == "" || result.Receipt.RunID == request.PredecessorID || result.Receipt.Branch != request.Branch || result.Receipt.SubmittedHeadSHA != request.CandidateSHA || result.Receipt.HeadSHA != request.CandidateSHA || result.Receipt.ValidationGeneration != request.SuccessorGeneration() || result.Receipt.IntentDigest != request.IntentDigest || result.Receipt.LaunchNonce == request.LaunchNonce {
				return emitError(command, 1, "daemon returned a conflicting authored admission proof")
			}
			if err := result.Receipt.LaunchAssertionProof.Check(request.LaunchAssertion); err != nil {
				return emitError(command, 1, err.Error())
			}
			emitLaunchReceipt(command, result.Receipt)
			emitDoc(command, validationFamilyField(result.Family), toon.Field{Key: "started", Value: result.Started})
			ctx := command.Context()
			driveCtx, cancel, err := boundAxiWait(ctx, wait)
			if err != nil {
				return emitError(command, 2, err.Error())
			}
			defer cancel()
			run, ready, err := driveRun(driveCtx, command.ErrOrStderr(), environment.client, environment.p.Socket(), result.Receipt.RunID, false)
			if err != nil {
				if isAxiWaitElapsed(ctx, driveCtx, err) {
					return emitAxiWaitElapsed(command, wait, "no-mistakes axi status")
				}
				return emitError(command, 1, fmt.Sprintf("drive authored validation: %v", err))
			}
			return renderDriveResult(command, run, ready)
		},
	}
	if isTerminal {
		command.Use = "validate-terminal"
		command.Short = "Validate an exact terminal candidate with an explicit fresh harness selection and zero repairs"
		command.Long = "Admit the exact committed terminal head for fresh validation in a distinct generation.\nExplicitly accept unknown historical dirty/untracked source and select the successor\nharness in the admission file. The predecessor and every known recovery anchor\nremain intact; no gate certification or repair allowance is inherited."
	}
	command.Flags().StringVar(&admissionFile, "admission-file", "", "operator-owned JSON binding the predecessor, canonical clean candidate, launch, publication and controller policy assertion")
	bindAxiWaitFlag(command, &wait)
	return command
}

func validationFamilyField(family *authoredvalidation.Family) toon.Field {
	type phase struct {
		Name      string `toon:"step"`
		Automatic int    `toon:"automatic_spent"`
		Manual    int    `toon:"manual_spent"`
		Remaining int    `toon:"remaining"`
	}
	var phases []phase
	for name, spending := range family.Spending {
		phases = append(phases, phase{Name: string(name), Automatic: spending.Automatic, Manual: spending.Manual, Remaining: 0})
	}
	sort.Slice(phases, func(left, right int) bool { return phases[left].Name < phases[right].Name })
	return toon.Field{Key: "authored_validation", Value: toon.NewObject(
		toon.Field{Key: "admission_id", Value: family.AdmissionID}, toon.Field{Key: "predecessor_id", Value: family.PredecessorID},
		toon.Field{Key: "automatic_repairs_remaining", Value: 0}, toon.Field{Key: "manual_repairs_remaining", Value: 0},
		toon.Field{Key: "historical_source_completeness", Value: family.HistoricalSourceCompleteness},
		toon.Field{Key: "policy_origin", Value: family.PolicyOrigin}, toon.Field{Key: "controller_policy_sha256", Value: family.ControllerPolicy.SHA256},
		toon.Field{Key: "spending", Value: phases},
	)}
}
