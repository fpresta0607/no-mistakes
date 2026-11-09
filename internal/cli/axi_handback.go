package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newAxiHandbackCmd() *cobra.Command {
	var shouldResume bool
	var shouldRevalidatePublication bool
	var wait time.Duration
	var expectationPath, receiptID string
	command := &cobra.Command{
		Use:   "handback",
		Short: "Return failed published CI to its author for revalidation",
		Long: "Release ordinary failed checks or a published CI merge conflict with automatic CI repair disabled.\n" +
			"The same run, PR, findings and repair budgets remain unresolved. Synchronize\n" +
			"behind caller with axi sync. Ordinary divergence is anchored without moving\n" +
			"the author branch; follow branch_sync.next_action for additive integration.\n\n" +
			"Preserve the released head and original author history. Conflicts also\n" +
			"require an additive merge of the current PR base. Commit the correction,\n" +
			"then use --resume --receipt <id> from the same\n" +
			"worktree to submit that head to the same run for fresh validation from Review.\n" +
			"No fixer runs during release. No approval, skip, rebase or force-push is used.\n" +
			"GitHub head verification is required. --resume follows the next decision\n" +
			"point with the usual bounded --wait.\n" +
			"Use --revalidate-publication to release an ordinary published descendant\n" +
			"of the recorded Review and Test heads without certifying that descendant.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateAxiWait(wait); err != nil {
				return emitError(command, 2, err.Error())
			}
			var expected *ipc.CIHandbackBinding
			if expectationPath != "" {
				var err error
				expected, err = readCIHandbackExpectation(expectationPath)
				if err != nil {
					return emitError(command, 2, "invalid handback expectation: "+err.Error())
				}
			}
			if shouldResume && strings.TrimSpace(receiptID) == "" {
				return emitError(command, 2, "--resume requires --receipt <native handback receipt id>")
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
			caller, err = git.FindGitRoot(caller)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			caller, err = realpath.Resolve(caller)
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
			request := ipc.CIHandbackParams{RepoID: env.repo.ID, RunID: active.Run.ID, Branch: branch, CallerDir: caller, HeadSHA: head, Resume: shouldResume, RevalidatePublication: shouldRevalidatePublication, ReceiptID: receiptID, Expect: expected}
			stored, err := env.d.GetRun(active.Run.ID)
			if err != nil || stored == nil || stored.LastPushedSHA == nil || stored.PushGeneration == nil {
				return emitError(command, 1, "cannot bind handback to the published run")
			}
			request.PublishedHead, request.PushGeneration = *stored.LastPushedSHA, *stored.PushGeneration
			binding := ipc.CIHandbackBinding{RepoID: env.repo.ID, RunID: stored.ID, Branch: branch, CallerDir: caller, CallerHead: head, CurrentHead: stored.HeadSHA, PublishedHead: *stored.LastPushedSHA, PushGeneration: *stored.PushGeneration}
			steps, err := env.d.GetStepsByRun(stored.ID)
			if err != nil {
				return emitError(command, 1, err.Error())
			}
			for _, step := range steps {
				if step.StepName == types.StepCI {
					rounds, err := env.d.GetRoundsByStep(step.ID)
					if err != nil || len(rounds) == 0 {
						return emitError(command, 1, "cannot bind handback to its unresolved CI round")
					}
					request.RoundID = rounds[len(rounds)-1].ID
				}
			}
			isAcceptedReplay := false
			if shouldResume {
				receipt, err := env.d.LatestCIHandback(stored.ID)
				if err != nil || receipt == nil || receipt.ID != receiptID {
					return emitError(command, 1, "handback receipt does not identify this branch's current run")
				}
				request.PublishedHead, request.PushGeneration, request.RoundID = receipt.ReleasedHead, receipt.PushGeneration, receipt.RoundID
				isAcceptedReplay = receipt.AcceptedHead != nil
				if receipt.AcceptedHead != nil && *receipt.AcceptedHead == head && stored.HeadSHA == head {
					binding.CurrentHead, binding.PublishedHead, binding.PushGeneration = receipt.ReleasedHead, receipt.ReleasedHead, receipt.PushGeneration
				}
			}
			if expected != nil && *expected != binding {
				return emitError(command, 1, "handback expectation changed; no run was adopted or modified")
			}
			var result ipc.CIHandbackResult
			if err := env.client.CallWithContext(ctx, ipc.MethodCIHandback, request, &result, 95*time.Second); err != nil {
				return emitError(command, 1, fmt.Sprintf("CI handback refused: %v", err))
			}
			state := "released"
			if result.AcceptedHead != "" {
				state = "accepted"
			}
			receiptFields := []toon.Field{{Key: "id", Value: result.ReceiptID}, {Key: "run_id", Value: result.Binding.RunID}, {Key: "repo_id", Value: result.Binding.RepoID}, {Key: "branch", Value: result.Binding.Branch}, {Key: "caller_dir", Value: result.Binding.CallerDir}, {Key: "caller_head", Value: result.Binding.CallerHead}, {Key: "current_head", Value: result.Binding.CurrentHead}, {Key: "published_head", Value: result.Binding.PublishedHead}, {Key: "push_generation", Value: result.Binding.PushGeneration}, {Key: "round_id", Value: result.RoundID}, {Key: "state", Value: state}}
			if result.AcceptedHead != "" {
				receiptFields = append(receiptFields, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
			}
			receiptField := toon.Field{Key: "receipt", Value: toon.NewObject(receiptFields...)}
			fields := []toon.Field{{Key: "run", Value: result.RunID}, {Key: "pr", Value: result.PRURL}, {Key: "released_head", Value: result.ReleasedHead}, receiptField}
			if !shouldResume {
				nextCode, nextCommand := "resume_ci_handback", "no-mistakes axi handback --resume --receipt "+result.ReceiptID
				if result.Binding.CallerHead != result.ReleasedHead {
					nextCode = "integrate_ci_handback"
					nextCommand = fmt.Sprintf("git merge --no-ff --no-edit refs/no-mistakes/ci-handback/%s/%d/published", result.RunID, result.Binding.PushGeneration)
				}
				fields = append(fields, toon.Field{Key: "handback", Value: state}, toon.Field{Key: "next_action", Value: toon.NewObject(toon.Field{Key: "code", Value: nextCode}, toon.Field{Key: "command", Value: nextCommand})}, toon.Field{Key: "help", Value: []string{"Preserve both released and original author histories, commit the correction, then resume from this worktree with this receipt.", "Failed CI remains unresolved; readiness requires fresh validation of the submitted head."}})
				emitDoc(command, fields...)
				return nil
			}
			fields = append(fields, toon.Field{Key: "handback", Value: state}, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
			emitFollowError := func(err error) error {
				emitDoc(command, append(fields, toon.Field{Key: "error", Value: err.Error()})...)
				return &exitError{code: 1}
			}
			driveContext, cancel, err := boundAxiWait(ctx, wait)
			if err != nil {
				return emitError(command, 2, err.Error())
			}
			defer cancel()
			if !isAcceptedReplay {
				if err := waitStepLeavesGate(driveContext, env.p.Socket(), result.RunID, "ci", gateIdentityFor(runViewFromIPC(active.Run), "ci")); err != nil {
					if isAxiWaitElapsed(ctx, driveContext, err) {
						return emitAxiWaitElapsed(command, wait, "no-mistakes axi run", fields...)
					}
					return emitFollowError(err)
				}
			}
			run, isReady, err := driveRun(driveContext, command.ErrOrStderr(), env.client, env.p.Socket(), result.RunID, false)
			if err != nil {
				if isAxiWaitElapsed(ctx, driveContext, err) {
					return emitAxiWaitElapsed(command, wait, "no-mistakes axi run", fields...)
				}
				return emitFollowError(err)
			}
			return renderDriveResult(command, run, isReady, toon.Field{Key: "handback", Value: state}, receiptField, toon.Field{Key: "accepted_head", Value: result.AcceptedHead})
		},
	}
	command.Flags().BoolVar(&shouldResume, "resume", false, "submit the additive follow-up for fresh validation in the same run")
	command.Flags().BoolVar(&shouldRevalidatePublication, "revalidate-publication", false, "release an ordinary post-validation publication with its historical evidence pinned")
	command.Flags().StringVar(&expectationPath, "expect", "", "compare all identities with this exact binding JSON file before changing custody")
	command.Flags().StringVar(&receiptID, "receipt", "", "exact native handback receipt id (required for --resume)")
	bindAxiWaitFlag(command, &wait)
	return command
}

func readCIHandbackExpectation(path string) (*ipc.CIHandbackBinding, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return nil, fmt.Errorf("binding must be at most 16 KiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	keys := []string{"repo_id", "run_id", "branch", "caller_dir", "caller_head", "current_head", "published_head", "push_generation"}
	if len(fields) != len(keys) {
		return nil, fmt.Errorf("binding requires exactly repo_id, run_id, branch, caller_dir, caller_head, current_head, published_head and push_generation")
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("missing field %s", key)
		}
	}
	var binding ipc.CIHandbackBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return nil, err
	}
	if strings.TrimSpace(binding.RepoID) == "" || strings.TrimSpace(binding.RunID) == "" || strings.TrimSpace(binding.Branch) == "" || !filepath.IsAbs(binding.CallerDir) || binding.PushGeneration <= 0 {
		return nil, fmt.Errorf("binding requires nonempty identity, absolute caller directory and positive publication generation")
	}
	for _, head := range []string{binding.CallerHead, binding.CurrentHead, binding.PublishedHead} {
		if _, err := hex.DecodeString(head); err != nil || (len(head) != 40 && len(head) != 64) || head != strings.ToLower(head) {
			return nil, fmt.Errorf("binding requires full lowercase commit ids")
		}
	}
	return &binding, nil
}
