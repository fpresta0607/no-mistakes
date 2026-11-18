package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// mergeRepairFixture is a CI repair fixture under rebase.strategy: merge whose
// base moved after the run pushed its reviewed head.
type mergeRepairFixture struct {
	*ciRepairFixture
	reviewedHead string
	baseTip      string
	prompts      []string
}

// newMergeRepairFixture moves the base with moveBase, run on a main checkout,
// and makes agentAction the CI fix agent.
func newMergeRepairFixture(t *testing.T, moveBase func(t *testing.T, f *mergeRepairFixture), agentAction func(dir string) error) *mergeRepairFixture {
	t.Helper()
	f := &mergeRepairFixture{ciRepairFixture: newCIRepairFixture(t, false, nil)}
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	f.reviewedHead = f.headSHA
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		f.prompts = append(f.prompts, opts.Prompt)
		if agentAction != nil {
			if err := agentAction(opts.CWD); err != nil {
				t.Errorf("agent action: %v", err)
			}
		}
		return &agent.Result{Output: []byte(`{"summary":"merge the base branch","code_change_needed":true}`)}, nil
	}}
	gitCmd(t, f.dir, "checkout", "main")
	moveBase(t, f)
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "base moves")
	f.baseTip = gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")
	return f
}

// conflictingBase edits the line the reviewed head added.
func conflictingBase(t *testing.T, f *mergeRepairFixture) {
	writeFile(t, f.dir, "feature.txt", "base version\n")
}

// resolveConflict resolves conflictingBase's conflict additively and stages it.
func resolveConflict(dir string) error {
	if _, err := runGitIn(dir, "rev-parse", "--verify", "MERGE_HEAD"); err != nil {
		return errors.New("no merge was in progress for the agent to resolve")
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\nbase version\n"), 0o644); err != nil {
		return err
	}
	_, err := runGitIn(dir, "add", "feature.txt")
	return err
}

func runGitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New(strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (f *mergeRepairFixture) repair(t *testing.T, targets ciFixTargets) (ciRepairResult, error) {
	t.Helper()
	host, skip := buildHost(f.sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(skip)
	}
	return (&CIStep{}).autoFixCI(f.sctx, host, &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL}, targets)
}

// assertPublishedMerge checks the repair published a merge commit of the
// reviewed head and the base tip as a fast-forward, keeping the approval.
func (f *mergeRepairFixture) assertPublishedMerge(t *testing.T, repair ciRepairResult) string {
	t.Helper()
	merged := f.localHead(t)
	if !repair.HeadAdvanced || repair.Revalidate {
		t.Fatalf("repair = %+v, want a published repair that keeps monitoring; log:\n%s", repair, f.log())
	}
	if parents := gitCmd(t, f.dir, "rev-list", "--parents", "-n", "1", merged); parents != merged+" "+f.reviewedHead+" "+f.baseTip {
		t.Fatalf("repaired head parents = %q, want the reviewed head %s then the base tip %s", parents, f.reviewedHead, f.baseTip)
	}
	if remote := f.remoteHead(t); remote != merged {
		t.Fatalf("remote head = %s, want the merge %s", remote, merged)
	}
	if f.sctx.Run.HeadSHA != merged {
		t.Fatalf("recorded head = %s, want the merge %s", f.sctx.Run.HeadSHA, merged)
	}
	if got := (&replayFixture{ciRepairFixture: f.ciRepairFixture}).approval(t); got != f.reviewedHead {
		t.Fatalf("review approval = %q, want it kept on the reviewed head %s", got, f.reviewedHead)
	}
	return merged
}

// Issue #206: under rebase.strategy: merge, the CI step's merge-conflict
// repair merges the base in instead of rebasing, the pipeline concludes that
// merge with the configured commit settings, and the merge publishes as a
// fast-forward of the reviewed head without revalidating.
func TestCIStep_MergeStrategyConflictRepairPublishesAMergeOfTheReviewedHead(t *testing.T) {
	t.Parallel()
	f := newMergeRepairFixture(t, conflictingBase, resolveConflict)

	repair, err := f.repair(t, ciTargetsFor(nil, true))
	if err != nil {
		t.Fatalf("merge repair: %v\nlog:\n%s", err, f.log())
	}

	merged := f.assertPublishedMerge(t, repair)
	if got := gitCmd(t, f.dir, "show", merged+":feature.txt"); got != "feature\nbase version" {
		t.Errorf("merged feature.txt = %q, want the agent's additive resolution", got)
	}
	want, err := f.sctx.Config.Commit.RenderFixMessageForBranch(types.StepCI, "merge the base branch", f.sctx.Run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if subject := gitCmd(t, f.dir, "log", "-1", "--format=%s", merged); subject != firstLine(want) {
		t.Errorf("merge commit subject = %q, want the configured fix message %q", subject, firstLine(want))
	}
	if len(f.prompts) != 1 {
		t.Fatalf("agent ran %d times, want once", len(f.prompts))
	}
	prompt := f.prompts[0]
	for _, wantText := range []string{"the pipeline has started merging the base branch", "Conflicted files:\n  - feature.txt", "Resolve ADDITIVELY", "Do not commit"} {
		if !strings.Contains(prompt, wantText) {
			t.Errorf("prompt is missing %q:\n%s", wantText, prompt)
		}
	}
	if strings.Contains(prompt, "Rebase onto") || strings.Contains(prompt, "rebase target commit") {
		t.Errorf("the merge repair prompt still asks for a rebase:\n%s", prompt)
	}
}

// A behind PR with no conflict takes the same merge, which the pipeline
// concludes even though the agent had nothing to resolve.
func TestCIStep_MergeStrategyBehindBaseRepairMergesWithoutConflicts(t *testing.T) {
	t.Parallel()
	f := newMergeRepairFixture(t, func(t *testing.T, f *mergeRepairFixture) {
		writeFile(t, f.dir, "base.txt", "another PR landed\n")
	}, nil)

	repair, err := f.repair(t, ciFixTargets{BehindBase: true})
	if err != nil {
		t.Fatalf("merge repair: %v\nlog:\n%s", err, f.log())
	}

	f.assertPublishedMerge(t, repair)
	if !strings.Contains(f.prompts[0], "The merge reported no conflicts.") {
		t.Errorf("prompt does not say the merge was clean:\n%s", f.prompts[0])
	}
}

// The pipeline concludes the merge only as it started it. Every other end the
// agent can give it fails the round and puts the recorded head back, so the
// next round starts a fresh merge and nothing reaches the remote.
func TestCIStep_MergeStrategyRepairRejectsAMergeThePipelineDidNotConclude(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		action  func(dir string) error
		wantErr string
	}{
		{
			name: "agent_concluded_the_merge_itself",
			action: func(dir string) error {
				if err := resolveConflict(dir); err != nil {
					return err
				}
				_, err := runGitIn(dir, "-c", "user.name=agent", "-c", "user.email=agent@test.com", "commit", "--no-edit")
				return err
			},
			wantErr: "was concluded or abandoned",
		},
		{
			name: "agent_aborted_the_merge",
			action: func(dir string) error {
				_, err := runGitIn(dir, "merge", "--abort")
				return err
			},
			wantErr: "was concluded or abandoned",
		},
		{
			name:    "agent_left_the_conflict_unresolved",
			action:  func(dir string) error { return nil },
			wantErr: "unresolved conflicts in: feature.txt",
		},
		{
			name: "agent_rebased_instead",
			action: func(dir string) error {
				if _, err := runGitIn(dir, "merge", "--abort"); err != nil {
					return err
				}
				// The rebase stops on the same conflict and is left in progress.
				_, _ = runGitIn(dir, "rebase", "origin/main")
				return nil
			},
			wantErr: "a rebase was left in progress",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMergeRepairFixture(t, conflictingBase, tc.action)

			_, err := f.repair(t, ciTargetsFor(nil, true))

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("merge repair error = %v, want one containing %q", err, tc.wantErr)
			}
			if head := f.localHead(t); head != f.reviewedHead {
				t.Errorf("worktree head = %s, want the recorded head %s restored", head, f.reviewedHead)
			}
			if mergeInProgress(f.sctx.Ctx, f.dir) || rebaseInProgress(f.sctx.Ctx, f.dir) {
				t.Error("a merge or rebase is still in progress after the rejected repair")
			}
			if f.sctx.Run.HeadSHA != f.reviewedHead || f.remoteHead(t) != f.reviewedHead {
				t.Errorf("recorded head %s, remote %s; want both still on the reviewed head %s", f.sctx.Run.HeadSHA, f.remoteHead(t), f.reviewedHead)
			}
		})
	}
}

// A rejected merge round restores the recorded head with a hard reset, so the
// pipeline never starts a merge over uncommitted tracked changes, such as a
// timed-out repair's leftovers it reported to the operator: those would be
// carried into the merge and discarded with it.
func TestCIStep_MergeStrategyRepairRefusesToMergeOverUncommittedChanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stage bool
	}{
		{name: "unstaged_edit"},
		{name: "staged_edit", stage: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMergeRepairFixture(t, conflictingBase, func(dir string) error { return nil })
			writeFile(t, f.dir, "init.txt", "leftover repair work\n")
			if tc.stage {
				gitCmd(t, f.dir, "add", "init.txt")
			}

			_, err := f.repair(t, ciTargetsFor(nil, true))

			if err == nil || !strings.Contains(err.Error(), "uncommitted changes to tracked files") {
				t.Fatalf("merge repair error = %v, want a refusal naming the uncommitted changes\nlog:\n%s", err, f.log())
			}
			if len(f.prompts) != 0 {
				t.Errorf("agent ran %d times, want no repair turn over the uncommitted changes", len(f.prompts))
			}
			if got, readErr := os.ReadFile(filepath.Join(f.dir, "init.txt")); readErr != nil || string(got) != "leftover repair work\n" {
				t.Errorf("init.txt = %q (%v), want the uncommitted change kept", got, readErr)
			}
			if mergeInProgress(f.sctx.Ctx, f.dir) {
				t.Error("a merge was started over the uncommitted changes")
			}
			if head := f.localHead(t); head != f.reviewedHead || f.sctx.Run.HeadSHA != f.reviewedHead || f.remoteHead(t) != f.reviewedHead {
				t.Errorf("worktree head %s, recorded head %s, remote %s; want all on the reviewed head %s", head, f.sctx.Run.HeadSHA, f.remoteHead(t), f.reviewedHead)
			}
		})
	}
}

// A merge moves a gitlink in the index but not the submodule's checkout, so
// staging the checkout would silently revert the base's submodule bump.
func TestCIStep_MergeStrategyRepairKeepsTheBasesSubmoduleBump(t *testing.T) {
	t.Parallel()
	sub := t.TempDir()
	gitCmd(t, sub, "init", "-b", "main")
	writeFile(t, sub, "a.txt", "a\n")
	gitCmd(t, sub, "add", "-A")
	gitCmd(t, sub, "commit", "-m", "a")
	oldPointer := gitCmd(t, sub, "rev-parse", "HEAD")
	writeFile(t, sub, "b.txt", "b\n")
	gitCmd(t, sub, "add", "-A")
	gitCmd(t, sub, "commit", "-m", "b")
	newPointer := gitCmd(t, sub, "rev-parse", "HEAD")

	f := newMergeRepairFixture(t, func(t *testing.T, f *mergeRepairFixture) {
		dir := f.dir
		// The reviewed head already has the submodule at oldPointer, through
		// a merge of the base that added it.
		gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
		gitCmd(t, filepath.Join(dir, "sub"), "checkout", "-q", oldPointer)
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "add the submodule")
		gitCmd(t, dir, "push", "origin", "main")
		gitCmd(t, dir, "checkout", "feature")
		gitCmd(t, dir, "merge", "--no-ff", "--no-edit", "main")
		gitCmd(t, dir, "push", "origin", "feature")
		f.reviewedHead = gitCmd(t, dir, "rev-parse", "HEAD")
		rerecordReviewedHead(t, f.ciRepairFixture, f.reviewedHead)
		// Then the base bumps it.
		gitCmd(t, dir, "checkout", "main")
		gitCmd(t, filepath.Join(dir, "sub"), "checkout", "-q", newPointer)
	}, nil)
	// The run worktree's submodule checkout is still the reviewed one.
	gitCmd(t, filepath.Join(f.dir, "sub"), "checkout", "-q", oldPointer)

	repair, err := f.repair(t, ciFixTargets{BehindBase: true})
	if err != nil {
		t.Fatalf("merge repair: %v\nlog:\n%s", err, f.log())
	}

	merged := f.assertPublishedMerge(t, repair)
	if got := gitCmd(t, f.dir, "rev-parse", merged+":sub"); got != newPointer {
		t.Fatalf("merged submodule pointer = %s, want the base's bump %s (reviewed pointer %s)", got, newPointer, oldPointer)
	}
}

// rerecordReviewedHead makes head the run's reviewed, published head, as the
// fixture does for the head it starts with.
func rerecordReviewedHead(t *testing.T, f *ciRepairFixture, head string) {
	t.Helper()
	if err := f.sctx.DB.UpdateRunHeadSHA(f.sctx.Run.ID, head); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.HeadSHA = head
	recordReviewApproval(t, f.sctx, head)
	if err := f.sctx.DB.UpdateRunPushBinding(f.sctx.Run.ID, db.PushBinding{
		HeadSHA: head, TargetKind: "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(f.upstream), Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}
	f.headSHA = head
}

// protected_paths covers what the resolution changed, not what the base brought
// in, and a refused merge is retained for the explicit fix that retries it.
func TestCIStep_MergeStrategyProtectedPathsCoverOnlyTheResolution(t *testing.T) {
	t.Parallel()
	f := newMergeRepairFixture(t, func(t *testing.T, f *mergeRepairFixture) {
		conflictingBase(t, f)
		if err := os.MkdirAll(filepath.Join(f.dir, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, f.dir, ".github/base.yml", "from the base\n")
	}, func(dir string) error {
		if err := resolveConflict(dir); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".github", "agent.yml"), []byte("from the agent\n"), 0o644)
	})
	f.sctx.Config.ProtectedPaths = []string{".github/**"}

	_, err := f.repair(t, ciTargetsFor(nil, true))

	var refusal *pipeline.ProtectedPathError
	if !errors.As(err, &refusal) || refusal.Path != ".github/agent.yml" {
		t.Fatalf("merge repair error = %v, want a protected-path refusal of .github/agent.yml alone", err)
	}
	if !mergeInProgress(f.sctx.Ctx, f.dir) || f.localHead(t) != f.reviewedHead || f.remoteHead(t) != f.reviewedHead {
		t.Fatalf("the refused merge was not retained in progress on the unpublished reviewed head")
	}

	// The operator removes the protected edit and asks for the retry.
	gitCmd(t, f.dir, "rm", "-q", "-f", ".github/agent.yml")
	repair, err := (&CIStep{}).retryProtectedPathRepair(f.sctx)
	if err != nil {
		t.Fatalf("retry the retained merge: %v\nlog:\n%s", err, f.log())
	}

	merged := f.assertPublishedMerge(t, repair)
	if got := gitCmd(t, f.dir, "show", merged+":.github/base.yml"); got != "from the base" {
		t.Errorf("merged .github/base.yml = %q, want the base's file", got)
	}
}

// A conflicted protected path differs from git's automatic merge, which holds
// the conflict markers, in every real resolution, so the pipeline refuses to
// conclude it. The operator concludes that merge in the run worktree instead,
// and the retry records it only when its first parent is the recorded head and
// the strategy is still merge; under rebase it stays refused as non-linear.
func TestCIStep_MergeStrategyRetryRecordsAnOperatorConcludedMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		conclude      func(t *testing.T, f *mergeRepairFixture)
		wantPublished bool
	}{
		{
			name: "operator_commits_the_retained_merge",
			conclude: func(t *testing.T, f *mergeRepairFixture) {
				gitCmd(t, f.dir, "commit", "-q", "--no-edit")
			},
			wantPublished: true,
		},
		{
			name: "operator_merges_the_reviewed_head_into_the_base",
			conclude: func(t *testing.T, f *mergeRepairFixture) {
				gitCmd(t, f.dir, "commit", "-q", "--no-edit")
				reversed := gitCmd(t, f.dir, "commit-tree", "HEAD^{tree}", "-p", f.baseTip, "-p", f.reviewedHead, "-m", "reversed merge")
				gitCmd(t, f.dir, "reset", "-q", "--hard", reversed)
			},
		},
		{
			name: "operator_merge_under_the_rebase_strategy",
			conclude: func(t *testing.T, f *mergeRepairFixture) {
				gitCmd(t, f.dir, "commit", "-q", "--no-edit")
				f.sctx.Config.Rebase.Strategy = config.RebaseStrategyRebase
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMergeRepairFixture(t, conflictingBase, resolveConflict)
			f.sctx.Config.ProtectedPaths = []string{"feature.txt"}
			_, err := f.repair(t, ciTargetsFor(nil, true))
			var refusal *pipeline.ProtectedPathError
			if !errors.As(err, &refusal) || refusal.Path != "feature.txt" {
				t.Fatalf("merge repair error = %v, want a protected-path refusal of the conflicted feature.txt", err)
			}
			tc.conclude(t, f)

			repair, err := (&CIStep{}).retryProtectedPathRepair(f.sctx)

			if tc.wantPublished {
				if err != nil {
					t.Fatalf("retry the operator's merge: %v\nlog:\n%s", err, f.log())
				}
				merged := f.assertPublishedMerge(t, repair)
				if got := gitCmd(t, f.dir, "show", merged+":feature.txt"); got != "feature\nbase version" {
					t.Errorf("merged feature.txt = %q, want the operator's resolution", got)
				}
				return
			}
			if !errors.Is(err, errAgentCommitsNotLinear) {
				t.Fatalf("retry error = %v, want the non-linear refusal", err)
			}
			if f.sctx.Run.HeadSHA != f.reviewedHead || f.remoteHead(t) != f.reviewedHead {
				t.Errorf("recorded head %s, remote %s; want both still on the reviewed head %s", f.sctx.Run.HeadSHA, f.remoteHead(t), f.reviewedHead)
			}
		})
	}
}

// Under the default rebase strategy the pipeline never concludes a merge, so
// an unfinished merge the operator left in the run worktree is retried as an
// ordinary repair, whose protected_paths check reads the whole change against
// the recorded head and refuses the protected file the base brought in.
func TestCIStep_RebaseStrategyRetryNeverConcludesAnOperatorsUnfinishedMerge(t *testing.T) {
	t.Parallel()
	f := newMergeRepairFixture(t, func(t *testing.T, f *mergeRepairFixture) {
		conflictingBase(t, f)
		if err := os.MkdirAll(filepath.Join(f.dir, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, f.dir, ".github/base.yml", "from the base\n")
	}, nil)
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyRebase
	f.sctx.Config.ProtectedPaths = []string{".github/base.yml"}
	if _, err := runGitIn(f.dir, "merge", "--no-ff", "--no-commit", f.baseTip); err == nil {
		t.Fatal("operator merge of the conflicting base succeeded, want a conflict")
	}
	if err := resolveConflict(f.dir); err != nil {
		t.Fatal(err)
	}

	_, err := (&CIStep{}).retryProtectedPathRepair(f.sctx)

	var refusal *pipeline.ProtectedPathError
	if !errors.As(err, &refusal) || refusal.Path != ".github/base.yml" {
		t.Fatalf("retry error = %v, want a protected-path refusal of .github/base.yml\nlog:\n%s", err, f.log())
	}
	if head := f.localHead(t); head != f.reviewedHead {
		t.Errorf("local head = %s, want the operator's merge left unconcluded on the reviewed head %s", head, f.reviewedHead)
	}
	if f.sctx.Run.HeadSHA != f.reviewedHead || f.remoteHead(t) != f.reviewedHead {
		t.Errorf("recorded head %s, remote %s; want both still on the reviewed head %s", f.sctx.Run.HeadSHA, f.remoteHead(t), f.reviewedHead)
	}
}
