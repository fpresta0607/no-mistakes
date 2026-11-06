package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func newAxiTerminalPreflightCmd() *cobra.Command {
	var predecessorID string
	command := &cobra.Command{
		Use: "terminal-preflight", Short: "Inspect terminal candidate and recovery bindings without changing Git, the database or a run",
		Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(command *cobra.Command, _ []string) error {
			if predecessorID == "" {
				return emitError(command, 2, "--predecessor is required")
			}
			root, err := paths.New()
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			database, err := db.OpenReadOnly(root.DB())
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			defer database.Close()
			snapshot, err := database.ReadTerminalSnapshot(predecessorID)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			repository, err := findRepo(database)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			parent, err := database.GetRun(predecessorID)
			if err != nil || parent == nil || repository.ID != snapshot.Run.RepoID {
				return emitError(command, 1, "predecessor does not belong to the invoking repository")
			}
			directory, err := os.Getwd()
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			directory, err = filepath.EvalSymlinks(directory)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			ctx, cancel := context.WithTimeout(command.Context(), time.Minute)
			defer cancel()
			head, err := git.HeadSHA(ctx, directory)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			tree, err := git.Run(ctx, directory, "rev-parse", "HEAD^{tree}")
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			branch, err := git.CurrentBranch(ctx, directory)
			if err != nil || branch != parent.Branch {
				return emitError(command, 1, "predecessor belongs to another branch")
			}
			status, err := git.Run(ctx, directory, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			anchors, err := branchsync.TerminalValidationAnchors(ctx, root.RepoDir(repository.ID), directory, parent)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			runs, err := database.GetRunsByRepo(repository.ID)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			hasCompetingRun := false
			for _, run := range runs {
				if run.Branch == parent.Branch && run.ID != parent.ID && (!run.Status.Terminal() || run.ID > parent.ID) {
					hasCompetingRun = true
				}
			}
			assertionJSON, err := json.Marshal(parent.LaunchAssertion)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			emitDoc(command,
				toon.Field{Key: "predecessor_id", Value: parent.ID}, toon.Field{Key: "repo_id", Value: repository.ID}, toon.Field{Key: "branch", Value: parent.Branch},
				toon.Field{Key: "predecessor_status", Value: string(parent.Status)}, toon.Field{Key: "has_competing_run", Value: hasCompetingRun},
				toon.Field{Key: "snapshot_digest", Value: snapshot.Digest()}, toon.Field{Key: "caller_dir", Value: directory},
				toon.Field{Key: "candidate_sha", Value: head}, toon.Field{Key: "candidate_tree", Value: tree}, toon.Field{Key: "candidate_is_terminal_head", Value: head == parent.HeadSHA},
				toon.Field{Key: "caller_is_clean", Value: status == ""}, toon.Field{Key: "custody_returned_at", Value: parent.CustodyReturnedAt},
				toon.Field{Key: "predecessor_launch_nonce", Value: parent.LaunchNonce}, toon.Field{Key: "predecessor_validation_generation", Value: parent.LaunchValidationGeneration},
				toon.Field{Key: "intent_digest", Value: parent.LaunchIntentDigest}, toon.Field{Key: "predecessor_launch_assertion_json", Value: string(assertionJSON)},
				toon.Field{Key: "published_head_sha", Value: parent.LastPushedSHA}, toon.Field{Key: "push_generation", Value: parent.PushGeneration}, toon.Field{Key: "push_target_fingerprint", Value: parent.PushTargetFingerprint},
				toon.Field{Key: "source_anchors", Value: anchors}, toon.Field{Key: "historical_source_completeness", Value: "unknown"},
				toon.Field{Key: "note", Value: "Read-only observations. Admission must separately prove the explicit successor selection and current trusted source; this preflight grants no execution or completion authority."})
			return nil
		},
	}
	command.Flags().StringVar(&predecessorID, "predecessor", "", "terminal predecessor to inspect in the current repository and branch")
	return command
}
