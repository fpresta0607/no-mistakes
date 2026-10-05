package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type launchTrustedControlStep struct{ configs chan *config.Config }

func (*launchTrustedControlStep) Name() types.StepName { return types.StepReview }
func (step *launchTrustedControlStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	step.configs <- sctx.Config
	return &pipeline.StepOutcome{}, nil
}

func TestLaunchAssertionTrustedConfigCannotLoseValidatedControls(t *testing.T) {
	helperDirectory := t.TempDir()
	helperPath := filepath.Join(helperDirectory, "git")
	if runtime.GOOS == "windows" {
		helperPath += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-ldflags=-s -w", "-o", helperPath, "../pipeline/fakecli")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake git: %v\n%s", err, output)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"start_fresh_run", "recovery"} {
		t.Run(path, func(t *testing.T) {
			var factoryCalls atomic.Int32
			captured := make(chan *config.Config, 1)
			manager, root, database, repository, head, marker, _ := launchAssertionFixture(t, func() []pipeline.Step {
				factoryCalls.Add(1)
				if path == "recovery" {
					return steps.AllSteps()
				}
				return []pipeline.Step{&launchTrustedControlStep{configs: captured}}
			})
			trustedYAML := "agent: codex\ndisable_project_settings: true\nprotected_paths: ['test.txt']\nreview:\n  conversation: true\n  path_instructions:\n    - path: test.txt\n      instructions: retain trusted review guidance\n"
			trustedSHA := commitDefaultBranchConfig(t, repository.WorkingPath, trustedYAML)
			gateDirectory := root.RepoDir(repository.ID)
			blob := gitOutput(t, gateDirectory, "show", trustedSHA+":.no-mistakes.yaml")
			if strings.TrimSpace(blob) != strings.TrimSpace(trustedYAML) {
				t.Fatal("trusted-read fixture did not bind the committed YAML")
			}
			expected := launchExpectation(trustedSHA)
			var active *db.Run
			var activeContext context.Context
			if path == "start_fresh_run" {
				active, err = database.InsertRun(repository.ID, "feature", head, head)
				if err != nil {
					t.Fatal(err)
				}
				var cancel context.CancelCauseFunc
				activeContext, cancel = context.WithCancelCause(context.Background())
				manager.cancels[active.ID] = cancel
				defer cancel(nil)
			} else {
				active, err = database.InsertRunWithLaunchAssertion(repository.ID, "feature", head, head, nil, "recover-read", "generation", "digest", "", false, nil, expected)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := expected.Verify(trustedSHA, expected.Profiles)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.SetLaunchAssertionProof(active.ID, expected, proof); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, gateDirectory, "worktree", "add", "--detach", root.WorktreeDir(repository.ID, active.ID), head)
				parkRunAtReviewGate(t, database, active.ID, steps.AllSteps())
			}
			if err := database.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			before, err := database.GetRun(active.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeSteps, err := database.GetStepsByRun(active.ID)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, gateDirectory, "update-ref", "refs/remotes/origin/main", trustedSHA)
			beforeRefs := gitOutput(t, gateDirectory, "for-each-ref", "--format=%(refname) %(objectname)")
			beforeWorktrees := gitOutput(t, gateDirectory, "worktree", "list", "--porcelain")
			fetchHead := []byte("healthy trusted-read fixture custody\n")
			fetchHeadPath := filepath.Join(gateDirectory, "FETCH_HEAD")
			if err := os.WriteFile(fetchHeadPath, fetchHead, 0o600); err != nil {
				t.Fatal(err)
			}
			tracePath := filepath.Join(t.TempDir(), "trusted-reads.jsonl")
			t.Setenv("FAKE_CLI_MODE", "git-trusted-config-second-read-error")
			t.Setenv("FAKE_CLI_REAL_GIT", realGit)
			t.Setenv("FAKE_CLI_TRUSTED_OBJECT", trustedSHA+":.no-mistakes.yaml")
			t.Setenv("FAKE_CLI_TRUSTED_READ_TRACE", tracePath)
			t.Setenv("PATH", helperDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

			var resolved *config.Config
			var launchError error
			var receipt ipc.LaunchReceipt
			if path == "start_fresh_run" {
				receipt, launchError = manager.HandleStartFreshRun(t.Context(), &ipc.StartFreshRunParams{
					RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "preserve validated trusted controls",
					LaunchNonce: "trusted-read", ValidationGeneration: "generation", LaunchAssertion: expected,
				})
			} else {
				plan, recoveryError := manager.prepareRecoveredRun(t.Context(), before)
				launchError = recoveryError
				if plan != nil {
					resolved = plan.cfg
					if err := plan.agent.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			trace, err := os.ReadFile(tracePath)
			if err != nil {
				t.Fatalf("trusted-read fixture never reached the pinned read: %v", err)
			}
			lines := bytes.Split(bytes.TrimSpace(trace), []byte("\n"))
			if len(lines) < 1 || len(lines) > 2 {
				t.Fatalf("unexpected pinned-read count: %d\n%s", len(lines), trace)
			}
			for index, line := range lines {
				var read struct {
					Args      []string `json:"args"`
					Directory string   `json:"directory"`
					IsRefused bool     `json:"is_refused"`
				}
				if err := json.Unmarshal(line, &read); err != nil {
					t.Fatal(err)
				}
				wantDirectory := gateDirectory
				wantArgs := []string{"--git-dir=" + gateDirectory, "show", trustedSHA + ":.no-mistakes.yaml"}
				if path == "recovery" {
					wantDirectory = root.WorktreeDir(repository.ID, active.ID)
					wantArgs = wantArgs[1:]
				}
				if !reflect.DeepEqual(read.Args, wantArgs) || !samePath(read.Directory, wantDirectory) || read.IsRefused != (index == 1) {
					t.Fatalf("trusted-read fixture argv/cwd/refusal premise differs: %+v", read)
				}
			}
			if len(lines) == 2 {
				if launchError == nil || !strings.Contains(launchError.Error(), "pinned YAML second read refused") {
					t.Errorf("failed second trusted YAML read did not refuse before selection: %v", launchError)
				}
				if receipt.RunID != "" || factoryCalls.Load() != 0 {
					t.Errorf("failed trusted read created a receipt or pipeline: receipt=%+v factories=%d", receipt, factoryCalls.Load())
				}
				if activeContext != nil && context.Cause(activeContext) != nil {
					t.Errorf("failed trusted read cancelled healthy active run: %v", context.Cause(activeContext))
				}
				runs, err := database.GetRunsByRepo(repository.ID)
				if err != nil || len(runs) != 1 || !reflect.DeepEqual(runs[0], before) {
					t.Errorf("failed trusted read changed run custody: %+v, error=%v", runs, err)
				}
				afterSteps, err := database.GetStepsByRun(active.ID)
				if err != nil || !reflect.DeepEqual(afterSteps, beforeSteps) {
					t.Errorf("failed trusted read changed recorded steps: %+v, error=%v", afterSteps, err)
				}
				if refs := gitOutput(t, gateDirectory, "for-each-ref", "--format=%(refname) %(objectname)"); refs != beforeRefs {
					t.Errorf("failed trusted read changed Git refs: %s", refs)
				}
				if worktrees := gitOutput(t, gateDirectory, "worktree", "list", "--porcelain"); worktrees != beforeWorktrees {
					t.Errorf("failed trusted read changed worktree custody: %s", worktrees)
				}
				if after, err := os.ReadFile(fetchHeadPath); err != nil || !bytes.Equal(after, fetchHead) {
					t.Errorf("failed trusted read changed FETCH_HEAD: %q, error=%v", after, err)
				}
			} else {
				if launchError != nil {
					t.Fatalf("single validated capture refused: %v", launchError)
				}
				if path == "start_fresh_run" {
					if err := receipt.LaunchAssertionProof.Check(expected); err != nil {
						t.Fatal(err)
					}
					select {
					case resolved = <-captured:
					case <-time.After(5 * time.Second):
						t.Fatal("single validated capture did not reach the local config observer")
					}
				}
				if resolved == nil || resolved.TrustedConfigSHA != trustedSHA || !resolved.DisableProjectSettings ||
					!reflect.DeepEqual(resolved.ProtectedPaths, []string{"test.txt"}) || !resolved.Review.Conversation ||
					!reflect.DeepEqual(resolved.Review.PathInstructions, []config.PathInstruction{{Path: "test.txt", Instructions: "retain trusted review guidance"}}) {
					t.Errorf("single validated capture lost trusted controls: %+v", resolved)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("trusted-config validation entered agent fixture: %v", err)
			}
		})
	}
}
