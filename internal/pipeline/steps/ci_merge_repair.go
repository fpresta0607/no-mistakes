package steps

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// A CI repair that integrates the base branch (a merge conflict, or a PR
// behind a base that requires up-to-date branches) honors rebase.strategy:
// merge the way the Rebase step does, with a merge commit whose first parent
// is the run's recorded head. The repaired head then descends from the
// reviewed head, so ciRepairContinuityGap proves its continuity by ancestry
// and it publishes as a fast-forward instead of a force push.
//
// The pipeline owns that merge, not the agent. startCIMerge runs
// `git merge --no-ff --no-commit` of the resolved base tip, the agent only
// resolves what it leaves conflicted, and commitCIMerge concludes it with the
// repository's commit settings and proves its parents are exactly the recorded
// head and that tip. A merge the agent concluded itself would carry the
// agent's own message, which conformAgentCommits cannot reshape because it
// rewrites only a linear chain.

const ciBaseMergeFixRules = `- Do not make unrelated file edits.
` + ciFixerClassRules

// ciBaseMergePromptSection tells the agent the merge is the pipeline's and
// asks for the same additive resolution as the Rebase step's merge resolver,
// because the merge commit is what makes a resolution auditable afterwards.
func ciBaseMergePromptSection(target string, conflicts []string) string {
	section := fmt.Sprintf(`

Base branch merge:
- The pipeline has started merging base tip %s into the branch with git merge --no-ff --no-commit; the merge is in progress in the worktree.`, target)
	if len(conflicts) > 0 {
		section += "\n- Conflicted files:\n  - " + strings.Join(conflicts, "\n  - ")
	} else {
		section += "\n- The merge reported no conflicts."
	}
	return section + `
- Resolve ADDITIVELY. Keep both sides' introduced content; never delete content one side introduced to make the merge apply. Where both sides changed the same lines, combine them so neither side's contribution is lost.
- Only where the two sides are genuinely mutually exclusive may one supersede the other, and then say which and why in the summary.
- Stage each resolved file with: git add <file>
- Do not commit, and do not run git merge --continue, git merge --abort, git rebase, git reset, git checkout, or git switch: the pipeline concludes the merge itself with the repository's commit settings, and refuses a merge that was concluded, abandoned, or replaced.`
}

// startCIMerge starts merging target into the run's recorded head and leaves
// the merge in progress for the agent, returning the files it left conflicted.
func startCIMerge(sctx *pipeline.StepContext, target string) ([]string, error) {
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return nil, fmt.Errorf("resolve head before merging the base branch: %w", err)
	}
	if head != sctx.Run.HeadSHA {
		return nil, fmt.Errorf("the run worktree is at %s, not the recorded head %s; refusing to merge the base branch into it", shortObjectID(head), shortObjectID(sctx.Run.HeadSHA))
	}
	if mergeInProgress(sctx.Ctx, sctx.WorkDir) || rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		return nil, errors.New("the run worktree already has a merge or rebase in progress; refusing to start the base merge")
	}
	contained, err := headDescendsFrom(sctx, target, head)
	if err != nil {
		return nil, fmt.Errorf("check whether the branch contains base tip %s: %w", shortObjectID(target), err)
	}
	if contained {
		return nil, fmt.Errorf("the branch already contains base tip %s; there is nothing to merge", shortObjectID(target))
	}
	sctx.Log(fmt.Sprintf("merging base tip %s...", shortObjectID(target)))
	// rerere.enabled=false for the reason mergeArgs gives.
	if _, mergeErr := stepGitRun(sctx, "-c", "rerere.enabled=false", "merge", "--no-ff", "--no-commit", target); mergeErr != nil {
		conflicts, err := unmergedPaths(sctx)
		if err != nil || len(conflicts) == 0 {
			_, _ = stepGitRun(sctx, "merge", "--abort")
			return nil, errors.Join(fmt.Errorf("merge base tip %s: %w", shortObjectID(target), mergeErr), err)
		}
		return conflicts, nil
	}
	return nil, nil
}

// concludeCIMerge concludes and records the base merge an automatic CI repair
// round started. A rejected merge is restored to the recorded head, as the
// Rebase step restores one (restorePreMergeHead), so the next round starts a
// fresh merge. A protected-path refusal is retained instead, like every refused
// repair, for the explicit fix that retries it (retryProtectedPathRepair).
func (s *CIStep) concludeCIMerge(sctx *pipeline.StepContext, target, summary string, producer *agent.Result, before []string) (ciRepairResult, error) {
	merged, err := commitCIMerge(sctx, target, summary, producer, before)
	var refusal *pipeline.ProtectedPathError
	if errors.As(err, &refusal) {
		return ciRepairResult{}, err
	}
	if err != nil {
		return ciRepairResult{}, restorePreMergeHead(sctx.Ctx, sctx, sctx.Run.HeadSHA, err)
	}
	return s.recordRepair(sctx, merged)
}

// commitCIMerge commits the base merge in progress on the recorded head and
// returns the merge commit. target is the tip the merge must take in; an empty
// target accepts the merge's own MERGE_HEAD, for the retry of a retained merge.
func commitCIMerge(sctx *pipeline.StepContext, target, summary string, producer *agent.Result, before []string) (string, error) {
	if err := sctx.AssertValidationSource(); err != nil {
		return "", err
	}
	if rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		_, _ = stepGitRun(sctx, "rebase", "--abort")
		return "", errors.New("a rebase was left in progress instead of the base merge the pipeline started")
	}
	if !mergeInProgress(sctx.Ctx, sctx.WorkDir) {
		return "", errors.New("the base merge the pipeline started was concluded or abandoned before the pipeline could conclude it")
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return "", fmt.Errorf("resolve the base merge's head: %w", err)
	}
	if head != sctx.Run.HeadSHA {
		return "", fmt.Errorf("the base merge is on %s instead of the recorded head %s", shortObjectID(head), shortObjectID(sctx.Run.HeadSHA))
	}
	mergeHead, err := stepGitRun(sctx, "rev-parse", "--verify", "MERGE_HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve the base merge's MERGE_HEAD: %w", err)
	}
	if target == "" {
		target = mergeHead
	}
	if mergeHead != target {
		return "", fmt.Errorf("the merge in progress takes in %s instead of base tip %s", shortObjectID(mergeHead), shortObjectID(target))
	}
	conflicts, err := unmergedPaths(sctx)
	if err != nil {
		return "", err
	}
	if len(conflicts) > 0 {
		return "", fmt.Errorf("the base merge still has unresolved conflicts in: %s", namedPaths(conflicts))
	}
	excluded, err := mergeStagingExclusions(sctx)
	if err != nil {
		return "", err
	}
	if err := stageWorktree(sctx, before, nil, excluded); err != nil {
		return "", fmt.Errorf("stage the base merge: %w", err)
	}
	if err := refuseProtectedMergeEdits(sctx, head, target); err != nil {
		return "", err
	}
	message, err := ciRepairMessage(sctx, summary, producer)
	if err != nil {
		return "", err
	}
	if _, err := stepGitRun(sctx, "commit", "-m", message); err != nil {
		return "", fmt.Errorf("conclude the base merge: %w", err)
	}
	merged, err := stepGitHeadSHA(sctx)
	if err != nil {
		return "", fmt.Errorf("resolve the concluded base merge: %w", err)
	}
	parents, err := stepGitRun(sctx, "rev-list", "--parents", "-n", "1", merged)
	if err != nil {
		return "", fmt.Errorf("read the concluded base merge's parents: %w", err)
	}
	if strings.Join(strings.Fields(parents), " ") != strings.Join([]string{merged, head, target}, " ") {
		return "", fmt.Errorf("the concluded base merge %s does not join the recorded head %s and base tip %s", shortObjectID(merged), shortObjectID(head), shortObjectID(target))
	}
	sctx.Log(fmt.Sprintf("concluded the merge of base tip %s into %s as %s", shortObjectID(target), shortObjectID(head), shortObjectID(merged)))
	return merged, nil
}

func unmergedPaths(sctx *pipeline.StepContext) ([]string, error) {
	out, err := stepGitRunRaw(sctx, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("list unresolved merge conflicts: %w", err)
	}
	var paths []string
	for _, path := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// mergeStagingExclusions lists what the catch-all staging of a base merge
// must leave as git merged it: every gitlink, and every untracked nested
// repository. The merge updates a gitlink in the index but not the submodule's
// checkout, so staging the checkout would silently revert the base's
// submodule bump, and the checkout of a submodule the base removed would come
// back as a new gitlink. unstageSubmodulePointerMoves cannot stand in here:
// it restores gitlinks to HEAD, which is the pre-merge head.
func mergeStagingExclusions(sctx *pipeline.StepContext) ([]string, error) {
	index, err := stepGitRunRaw(sctx, "ls-files", "-s", "-z")
	if err != nil {
		return nil, fmt.Errorf("list the base merge's gitlinks: %w", err)
	}
	var excluded []string
	seen := map[string]bool{}
	for _, entry := range strings.Split(strings.TrimSuffix(index, "\x00"), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if ok && strings.HasPrefix(meta, "160000 ") && !seen[path] {
			seen[path] = true
			excluded = append(excluded, path)
		}
	}
	untracked, err := untrackedFiles(sctx)
	if err != nil {
		return nil, err
	}
	for _, path := range untracked {
		if strings.HasSuffix(path, "/") {
			excluded = append(excluded, path)
		}
	}
	return excluded, nil
}

// refuseProtectedMergeEdits applies protected_paths to what the merge's
// resolution changed: the difference between git's own automatic merge of
// head and target and the staged result. stagePipelineChanges's dirty-path
// check cannot be used, because before the merge commit exists everything the
// base brought in is a staged change too, and that is not an edit the
// pipeline made.
func refuseProtectedMergeEdits(sctx *pipeline.StepContext, head, target string) error {
	if len(sctx.Config.ProtectedPaths) == 0 {
		return nil
	}
	resolved, err := stepGitRun(sctx, "write-tree")
	if err != nil {
		return fmt.Errorf("check protected_paths: %w", err)
	}
	automatic, err := automaticMergeTree(sctx, head, target)
	if err != nil {
		return fmt.Errorf("check protected_paths: %w", err)
	}
	changed, err := stepGitRunRaw(sctx, "diff", "--name-only", "-z", "--no-renames", automatic, resolved)
	if err != nil {
		return fmt.Errorf("check protected_paths: %w", err)
	}
	for _, file := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
		if file == "" {
			continue
		}
		for _, pattern := range sctx.Config.ProtectedPaths {
			if matchIgnorePattern(file, pattern) {
				return &pipeline.ProtectedPathError{Path: file, Rule: pattern}
			}
		}
	}
	return nil
}

// automaticMergeTree returns the tree git's own merge of ours and theirs
// writes, with conflict markers in the files it cannot merge. git merge-tree
// exits 1 for a conflicted merge and still prints that tree, so exit 1 is not
// a failure here.
func automaticMergeTree(sctx *pipeline.StepContext, ours, theirs string) (string, error) {
	cmd := stepCmd(sctx, "git", "-c", "rerere.enabled=false", "merge-tree", "--write-tree", "--no-messages", ours, theirs)
	cmd.Env = git.NonInteractiveEnvFrom(cmd.Env, sctx.WorkDir)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", fmt.Errorf("git merge-tree %s %s: %w", shortObjectID(ours), shortObjectID(theirs), err)
	}
	tree := firstLine(string(out))
	if tree == "" {
		return "", fmt.Errorf("git merge-tree %s %s wrote no tree", shortObjectID(ours), shortObjectID(theirs))
	}
	return tree, nil
}
