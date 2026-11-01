package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func runTerminalContinuation(cmd *cobra.Command, reason string) error {
	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	defer env.close()
	branch, err := git.CurrentBranch(cmd.Context(), ".")
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	runs, err := env.d.GetRunsByRepo(env.repo.ID)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		snapshot, err := env.d.ReadTerminalSnapshot(run.ID)
		if err != nil {
			return emitError(cmd, 1, err.Error())
		}
		event, err := env.d.GetTerminalAdoption(run.ID)
		if err != nil {
			return emitError(cmd, 1, err.Error())
		}
		if event != nil {
			snapshot = &event.Snapshot
		}
		directory, err := filepath.Abs(".")
		if err != nil {
			return emitError(cmd, 1, err.Error())
		}
		var result ipc.RecoverTerminalResult
		request := ipc.RecoverTerminalParams{RepoID: env.repo.ID, RunID: run.ID, Branch: branch, CallerDir: directory, SnapshotDigest: snapshot.Digest(), Reason: reason}
		if err := env.client.CallWithContext(cmd.Context(), ipc.MethodRecoverTerminal, request, &result, 2*time.Minute); err != nil {
			return emitError(cmd, 1, fmt.Sprintf("terminal recovery: %v", err))
		}
		emitDoc(cmd, toon.Field{Key: "adopted_unvalidated", Value: true}, toon.Field{Key: "run", Value: result.RunID}, toon.Field{Key: "candidate", Value: result.CandidateSHA}, toon.Field{Key: "adoption", Value: result.AdoptionID}, toon.Field{Key: "started", Value: result.Started}, toon.Field{Key: "help", Value: []string{"Run `no-mistakes axi status` to inspect fresh validation and its next decision"}})
		return nil
	}
	return emitError(cmd, 1, "no run exists for the current branch")
}
