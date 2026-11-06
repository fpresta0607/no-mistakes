package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func newAxiRefreshPRCmd() *cobra.Command {
	var runID string
	command := &cobra.Command{
		Use:           "refresh-pr --run <id>",
		Short:         "Publish a settled run's recorded Test exception in its existing PR",
		Long:          "Refresh only the recorded Test approval metadata and human text for an exact run.\nRequires a settled latest run and an unchanged open PR at its published head.\nRuns no agents or validation, changes no source or budgets, and never starts a daemon.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			runID = strings.TrimSpace(runID)
			if runID == "" {
				return emitError(command, 2, "refresh-pr requires an explicit --run <id>")
			}
			env, err := openAxiEnvWithOptions(axiEnvOptions{explicitRunID: runID, readOnlyDB: true})
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			defer env.close()
			run, err := env.d.GetRun(runID)
			if err != nil || run == nil || env.repo == nil {
				return emitError(command, 1, fmt.Sprintf("refresh-pr run %q was not found: %v", runID, err))
			}
			if run.PRURL == nil || run.LastPushedSHA == nil || *run.LastPushedSHA != run.HeadSHA || run.PushGeneration == nil {
				return emitError(command, 1, "refresh-pr requires an exact published run and recorded PR")
			}
			client, err := ipc.Dial(env.p.Socket())
			if err != nil {
				return emitError(command, 1, "refresh-pr requires a running compatible daemon: "+err.Error())
			}
			defer client.Close()
			request := ipc.RefreshPRParams{RunID: run.ID, RepoID: run.RepoID, Branch: run.Branch, HeadSHA: run.HeadSHA, PRURL: *run.PRURL, PushGeneration: *run.PushGeneration}
			var result ipc.RefreshPRResult
			if err := client.CallWithContext(command.Context(), ipc.MethodRefreshPR, request, &result, 35*time.Second); err != nil {
				return emitError(command, 1, "refresh-pr refused: "+err.Error(), "Use a reviewed daemon build supporting refresh-pr; inspect the result before any retry.")
			}
			if result.RunID != request.RunID || result.HeadSHA != request.HeadSHA || result.PRURL != request.PRURL {
				return emitError(command, 1, "refresh-pr returned a different publication identity; inspect without retrying")
			}
			emitDoc(command, toon.Field{Key: "run", Value: result.RunID}, toon.Field{Key: "head_sha", Value: result.HeadSHA}, toon.Field{Key: "pr", Value: result.PRURL}, toon.Field{Key: "updated", Value: result.Updated})
			return nil
		},
	}
	command.Flags().StringVar(&runID, "run", "", "exact run ID; required, with no implicit branch selection")
	return command
}
