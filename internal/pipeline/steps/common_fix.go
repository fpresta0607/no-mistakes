package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type fixExecutionOptions struct {
	RequirePreviousFindings bool
	MissingFindingsError    string
	LogMessage              string
	Prompt                  string
	ErrorPrefix             string
	FallbackSummary         string
	AfterAgentRun           func(*agent.Result) error
	// RunAgent overrides the agent-call seam while leaving preparation and
	// post-agent commit work on the step context. Review uses it to apply a
	// fresh review_agent_timeout stall budget at the instant each fixer starts.
	RunAgent func(agent.RunOpts) (*agent.Result, error)
	// SessionRole, when set, runs the fix turn in that durable review-loop
	// session (the review step's fixer role). Steps outside the review loop
	// leave it empty and stay session-isolated.
	SessionRole pipeline.SessionRole
	// Purpose labels the invocation for local performance telemetry.
	Purpose string
	// Workload records the bounded size of the change under fix for local
	// telemetry. Optional; nil leaves the invocation's workload unknown.
	Workload *agent.InvocationWorkload
	// IsReviewRepair marks the Review fix turn. It asks the fixer for the
	// optional verification_blocked result field and logs it, since this is
	// the one fix prompt that orders edits before verification. It also names
	// an owned scratch directory in the prompt for disposable output, commits
	// only the new files the fixer names in new_files, and moves every other
	// new file out of the checkout: a Review repair once pointed pytest's
	// basetemp into the checkout and its commit carried 20 test artifacts
	// (issue #114).
	IsReviewRepair bool
}

type commitSummary struct {
	Summary string `json:"summary"`
}

var errRejectedCommitSummary = errors.New("rejected commit summary")

const (
	// NoChangesAppliedSummary is the fix result of a round that changed
	// nothing; it is not a fix the pipeline applied.
	NoChangesAppliedSummary = "no changes applied"
	changesAppliedSummary   = "changes applied"
)

const fixerRemovalRule = `

Removal-first rule:
- When a problem can be solved by removing a code path that is not strictly required to satisfy the intent - an extra acceptance or matching branch, a fallback, an alias, a second definition of something the code already defines once, or handling for an input nobody intends - fix it by removing that path, not by validating, hardening, or documenting it. Judge what the intent strictly requires against the User intent section when present, otherwise against the change's own stated purpose. Later recorded human fix decisions supersede conflicting original intent. Removal is the smallest fix for such a path: hardening it leaves the unrequired path in place for the next review to find another hole in.`

// fixerPrompt wraps every shared fix-turn prompt with the two rules that apply
// to all of them: the removal-first rule and the limit on independently
// initiated memory-file edits. Review, Test, Lint, and custom-gate fix turns
// route through executeFixMode, and the Lint agent pass and the CI repair wrap
// their prompts the same way, so this is the insertion point for fix-turn rules.
func fixerPrompt(prompt string) string {
	return prompt + fixerRemovalRule + agent.MemoryFilesRule
}

const (
	commitSubjectLimit      = 72
	minFixSummaryBudget     = 20
	fixSummaryBudgetProbe   = "x"
	fixSummaryRuleTemplate  = "- The summary must be one concise sentence fragment suitable for a git commit subject.\n- Keep the summary under 10 words and at most %d characters: the commit subject adds a prefix and must stay within %d characters."
	fixSummaryRuleUnbounded = "- The summary must be one concise sentence fragment suitable for a git commit subject.\n- Keep the summary under 10 words."
)

// fixSummaryRules renders the summary rules of a fix prompt, including the
// character budget the configured commit.fix_message leaves for the summary.
func fixSummaryRules(sctx *pipeline.StepContext, stepName types.StepName) string {
	rendered, err := sctx.Config.Commit.RenderFixMessageForBranch(stepName, fixSummaryBudgetProbe, sctx.Run.Branch)
	if err != nil {
		return fixSummaryRuleUnbounded
	}
	doubled, err := sctx.Config.Commit.RenderFixMessageForBranch(stepName, fixSummaryBudgetProbe+fixSummaryBudgetProbe, sctx.Run.Branch)
	if err != nil {
		return fixSummaryRuleUnbounded
	}
	occurrences := utf8.RuneCountInString(doubled) - utf8.RuneCountInString(rendered)
	if occurrences < 1 {
		return fixSummaryRuleUnbounded
	}
	fixed := utf8.RuneCountInString(rendered) - occurrences*len(fixSummaryBudgetProbe)
	budget := (commitSubjectLimit - fixed) / occurrences
	return fmt.Sprintf(fixSummaryRuleTemplate, max(budget, minFixSummaryBudget), commitSubjectLimit)
}

var commitSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes))

// maxVerificationBlockedBytes bounds the optional verification_blocked field
// of a fix result. The field only reaches the step log.
const maxVerificationBlockedBytes = 300

// fixSummarySchema is the Review repair's result: commitSummarySchema plus the
// optional verification_blocked field a fixer fills when the tool that would
// verify its edits is missing, and the new_files list, the only new files its
// commit may carry. Both stay optional: a fixer with nothing to report omits
// verification_blocked, and an omitted new_files names no new file, which
// fails safe, where a required one would fail every turn that edits only
// tracked files and leaves it out.
var fixSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d},
		"verification_blocked": {"type": "string", "maxLength": %d},
		"new_files": {"type": "array", "items": {"type": "string"}}
	},
	"required": ["summary"]
}`, config.MaxFixMessageSummaryBytes, maxVerificationBlockedBytes))

// reviewRepairFilesRule is the Review repair's rule for the files it creates.
const reviewRepairFilesRule = `

New files rule:
- List in "new_files" every file this fix adds, by its path from the repository root. Every other new file in the checkout, staged or not, stays out of the commit and is moved out of the checkout after it.
- Keep disposable output, such as a test base temp directory, caches, build output and logs, in %s, never in the checkout. Before you finish, delete every file you created in the checkout that is not part of the fix.`

// unnamedNewFiles reads the new files a Review repair named and returns every
// other file it created since before. A new file the repair staged without
// committing counts too: it is unstaged first, so `git add` cannot carry an
// unnamed file past the rule. Output that does not parse names nothing, which
// fails safe.
func unnamedNewFiles(sctx *pipeline.StepContext, result *agent.Result, before []string) ([]string, error) {
	var parsed struct {
		NewFiles []string `json:"new_files"`
	}
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &parsed); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not read the repair's new_files, so it names no new file: %v", err))
		}
	}
	named := make(map[string]bool, len(parsed.NewFiles))
	for _, file := range parsed.NewFiles {
		if filepath.IsAbs(file) {
			if relative, err := filepath.Rel(sctx.WorkDir, file); err == nil {
				file = relative
			}
		}
		named[path.Clean(filepath.ToSlash(file))] = true
	}
	staged, err := stepGitRunRaw(sctx, "diff", "--cached", "--name-only", "--diff-filter=A", "--no-renames", "-z", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("list staged new files: %w", err)
	}
	var unstage []string
	for _, file := range strings.Split(strings.TrimSuffix(staged, "\x00"), "\x00") {
		if file != "" && !named[file] {
			unstage = append(unstage, ":(literal)"+file)
		}
	}
	if len(unstage) > 0 {
		if _, err := stepGitRunInput(sctx, strings.Join(unstage, "\x00"), "reset", "-q", "HEAD", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return nil, fmt.Errorf("unstage the new files the repair did not name: %w", err)
		}
	}
	wasBefore := make(map[string]bool, len(before))
	for _, file := range before {
		wasBefore[file] = true
	}
	untracked, err := untrackedFiles(sctx)
	if err != nil {
		return nil, err
	}
	var unnamed []string
	for _, file := range untracked {
		if !wasBefore[file] && !named[file] {
			unnamed = append(unnamed, file)
		}
	}
	return unnamed, nil
}

// reviewRepairDirName is the folder in a run's evidence directory that holds
// the new files a Review repair did not name. It is not evidence.
const reviewRepairDirName = "review-repair"

// setAsideUnnamedNewFiles moves the new files a Review repair did not name,
// already left out of its commit, out of the checkout into the run's evidence
// directory, under a folder publication never reads, so every later step
// checks exactly the committed tree. Nothing is deleted.
func setAsideUnnamedNewFiles(sctx *pipeline.StepContext, unnamed []string) error {
	if len(unnamed) == 0 {
		return nil
	}
	if sctx.EvidenceDir == "" {
		return errors.New("set aside the new files the repair did not name: the run has no evidence directory")
	}
	parent := filepath.Join(sctx.EvidenceDir, reviewRepairDirName)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create review repair set-aside folder: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "set-aside-")
	if err != nil {
		return fmt.Errorf("create review repair set-aside folder: %w", err)
	}
	for _, file := range unnamed {
		rel := filepath.FromSlash(file)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("untracked path %q is outside the worktree", file)
		}
		source := filepath.Join(sctx.WorkDir, rel)
		target := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create set-aside folder for %s: %w", file, err)
		}
		if err := os.Rename(source, target); err != nil {
			if err := copyPath(source, target); err != nil {
				return fmt.Errorf("move %s to %s: %w", file, dir, err)
			}
			if err := os.Remove(source); err != nil {
				return fmt.Errorf("move %s to %s: %w", file, dir, err)
			}
		}
	}
	sctx.Log(fmt.Sprintf("moved the new files the repair did not name in new_files out of the checkout to %s (%d): %s", dir, len(unnamed), namedPaths(unnamed)))
	return nil
}

// blockedVerification returns the bounded, single-line verification_blocked
// text of a fix result, or "" when the agent reported none or its output does
// not parse. A parse failure is not worth failing a committed fix over: the
// summary extraction already reports unparsable output.
func blockedVerification(result *agent.Result) string {
	if result == nil || result.Output == nil {
		return ""
	}
	var parsed struct {
		VerificationBlocked string `json:"verification_blocked"`
	}
	if err := json.Unmarshal(result.Output, &parsed); err != nil {
		return ""
	}
	blocked := strings.Join(strings.Fields(parsed.VerificationBlocked), " ")
	if len(blocked) > maxVerificationBlockedBytes {
		blocked = strings.ToValidUTF8(blocked[:maxVerificationBlockedBytes], "")
	}
	return blocked
}

// logBlockedVerification makes a fix round whose verification could not run
// visible in the step log. A round that produced a commit, whether the
// pipeline committed the fixer's edits or the fixer ran `git commit` itself, is
// labelled unverified; a round that changed nothing is labelled distinctly from
// the ordinary "no agent changes to commit", which reads as "nothing needed
// fixing".
func logBlockedVerification(sctx *pipeline.StepContext, blocked string, produced bool) {
	switch {
	case blocked == "":
	case produced:
		sctx.Log(fmt.Sprintf("fixes committed unverified, verification blocked: %s", blocked))
	default:
		sctx.Log(fmt.Sprintf("agent made no edits and reported verification blocked: %s; the findings were not fixed", blocked))
	}
}

// hasBlockingFindings returns true if any finding has error or warning severity.
func hasBlockingFindings(items []Finding) bool {
	for _, f := range items {
		if f.Severity == "error" || f.Severity == "warning" {
			return true
		}
	}
	return false
}

// reviewedPathsCoverReviewable reports whether reviewedPaths (a review turn's
// self-reported coverage) exactly covers reviewablePaths. Comparison is by
// cleaned path so "./x" and "x" match.
func reviewedPathsCoverReviewable(reviewedPaths, reviewablePaths []string) bool {
	allowed := make(map[string]bool, len(reviewablePaths))
	for _, candidate := range reviewablePaths {
		normalized := normalizeReviewedPath(candidate)
		if normalized == "" {
			return false
		}
		allowed[normalized] = true
	}
	covered := make(map[string]bool, len(reviewedPaths))
	for _, reviewed := range reviewedPaths {
		normalized := normalizeReviewedPath(reviewed)
		if normalized == "" || !allowed[normalized] {
			return false
		}
		covered[normalized] = true
	}
	for candidate := range allowed {
		if !covered[candidate] {
			return false
		}
	}
	return true
}

// uncoveredReviewablePaths returns the reviewable paths that no reviewed_paths
// entry covers, in reviewable order. Out-of-scope entries cannot cover
// anything, so they are ignored here; the strict
// reviewedPathsCoverReviewable check still fails the round for them.
func uncoveredReviewablePaths(reviewedPaths, reviewablePaths []string) []string {
	covered := make(map[string]bool, len(reviewedPaths))
	for _, reviewed := range reviewedPaths {
		covered[normalizeReviewedPath(reviewed)] = true
	}
	var missing []string
	for _, candidate := range reviewablePaths {
		if !covered[normalizeReviewedPath(candidate)] {
			missing = append(missing, candidate)
		}
	}
	return missing
}

// hasInvalidReviewedPath reports whether a coverage record contains an entry
// that does not normalize to a path at all (empty, whitespace, or "."). Such
// an entry is invalid coverage evidence: reviewedPathsCoverReviewable fails on
// it, so the round can only park, and no further review can cure it.
func hasInvalidReviewedPath(reviewedPaths []string) bool {
	for _, reviewed := range reviewedPaths {
		if normalizeReviewedPath(reviewed) == "" {
			return true
		}
	}
	return false
}

// mergeReviewedPaths unions two coverage records, keeping each path's first
// spelling and order. It is the deterministic merge behind the focused
// coverage pass and carried verdicts: the record combines what the turns
// examined with eligible prior verdicts, and the strict coverage check re-runs
// on the union.
//
// An entry that does not normalize to a path is kept, not folded away: it is
// invalid coverage evidence, and dropping it would let the completion turn
// launder the invalidity out of the union and certify a record that never had
// positive coverage. Keeping one such entry makes reviewedPathsCoverReviewable
// keep failing on the union, so the round parks with it named.
func mergeReviewedPaths(first, second []string) []string {
	seen := make(map[string]bool, len(first)+len(second))
	var merged []string
	invalidKept := false
	for _, path := range append(append([]string(nil), first...), second...) {
		key := normalizeReviewedPath(path)
		if key == "" {
			if invalidKept {
				continue
			}
			invalidKept = true
		} else {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		merged = append(merged, path)
	}
	return merged
}

// uncoveredReviewMessage names why a clean review round is parked instead of
// certifying the head: the reviewable files its reviewed_paths did not cover
// (or the whole set when the field was omitted), and any path it claimed that
// is not a reviewable changed file.
func uncoveredReviewMessage(reviewedPaths, reviewablePaths []string) string {
	if reviewedPaths == nil {
		return fmt.Sprintf("review reported no reviewed_paths; parking for approval with %d reviewable file(s) unverified: %s", len(reviewablePaths), strings.Join(reviewablePaths, ", "))
	}
	allowed := make(map[string]bool, len(reviewablePaths))
	for _, candidate := range reviewablePaths {
		allowed[normalizeReviewedPath(candidate)] = true
	}
	covered := make(map[string]bool, len(reviewedPaths))
	var outOfScope []string
	for _, reviewed := range reviewedPaths {
		normalized := normalizeReviewedPath(reviewed)
		if normalized == "" {
			outOfScope = append(outOfScope, `""`)
			continue
		}
		if !allowed[normalized] {
			outOfScope = append(outOfScope, reviewed)
			continue
		}
		covered[normalized] = true
	}
	var missing []string
	for _, candidate := range reviewablePaths {
		if !covered[normalizeReviewedPath(candidate)] {
			missing = append(missing, candidate)
		}
	}
	msg := "review coverage is incomplete; parking for approval"
	if len(missing) > 0 {
		msg += fmt.Sprintf(" with %d reviewable file(s) unverified: %s", len(missing), strings.Join(missing, ", "))
	}
	if len(outOfScope) > 0 {
		msg += fmt.Sprintf("; %d reviewed_paths entry(ies) outside the reviewable set: %s", len(outOfScope), strings.Join(outOfScope, ", "))
	}
	return msg
}

func normalizeReviewedPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// assertPipelineHeadContinuity fails closed when the worktree HEAD is no longer
// equal to or a descendant of the head the pipeline itself last recorded
// (sctx.Run.HeadSHA). Every repository gate and every post-review core step
// calls this guard at entry, and commitAgentFixes calls it around commits that
// advance the recorded head.
//
// The pipeline advances HEAD only through its own commits, each of which updates
// sctx.Run.HeadSHA in lockstep. If HEAD has diverged from that recorded head -
// e.g. a concurrent process reset the shared worktree to a different commit -
// then the pipeline's recorded history is no longer in HEAD, and continuing
// could validate or ship a substituted tree. The whole job of this tool is to
// not lose people's code, so we refuse rather than proceed.
//
// Anchor integrity: sctx.Run.HeadSHA is the correct, un-clobberable anchor. It
// is the *recorded* head the pipeline itself produced at its last commit - held
// in the single daemon process's in-memory Run struct (one shared pointer per
// run, never re-read from the DB mid-pipeline) and written only by no-mistakes
// commit code (commit_fix / rebase / ci_fix / push). An out-of-band `git reset`
// mutates the worktree HEAD on disk but cannot touch this field, so at the check
// point the anchor still holds the reviewed head even after a clobber. The guard
// deliberately compares the *recorded* head against the *live* worktree HEAD
// (git.HeadSHA); it never derives the anchor from the mutable worktree, which
// would be circular and defeatable. Because the guard runs at every repository
// gate and post-review core-step entry, and at the very top of commitAgentFixes
// before any commit that would advance sctx.Run.HeadSHA, the next pipeline
// boundary after a clobber is caught while the anchor is still the pre-clobber
// pipeline head. The anchor can never advance into a clobbered lineage without
// first passing this check.
//
// This is what happened in run 01KXC3SD5NZYMERGDS68Z1C8ER: the review step
// committed a correct fix, a sibling worktree sharing the bare repo reset HEAD
// to a divergent commit that lacked it, and the document step committed on the
// clobber and shipped it. A forward-only agent commit (git rebase --continue,
// etc.) keeps the recorded head as an ancestor and is allowed; a divergent
// (sibling) reset or a backward reset both trip this guard. On any failure the
// step and the whole run abort (executor.failRun) before doing more work -
// nothing is committed or shipped.
//
// A check git could not complete (HEAD unreadable, or an ancestry check that
// failed without git answering "not an ancestor") still refuses, but carries
// errHeadContinuityUnverified: it is no evidence of a rewrite, so a parked
// gate's reconciliation keeps the gate instead of failing the run (issue #88).
func assertPipelineHeadContinuity(sctx *pipeline.StepContext, stepName types.StepName) error {
	if err := sctx.AssertValidationSource(); err != nil {
		return err
	}
	recorded := strings.TrimSpace(sctx.Run.HeadSHA)
	if recorded == "" {
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("%w: resolve head before %s step: %w", errHeadContinuityUnverified, stepName, err)
	}
	if currentHead == recorded {
		return nil
	}
	// Fail closed: refuse unless the recorded head is genuinely an ancestor of the
	// live HEAD (a legitimate forward move). A non-ancestor result OR any git error
	// (e.g. an unknown recorded object) aborts rather than proceeds. Only exit
	// status 1 is git's answer "no"; git.Run drops the exit status from the chain
	// when the context ended, so a killed check never reads as that answer.
	_, err = git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", recorded, currentHead)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return fmt.Errorf("refusing to run %s step: worktree HEAD %s is not a descendant of the pipeline's recorded head %s; "+
			"the reviewed change was rewritten out-of-band and would be lost - aborting to protect it",
			stepName, currentHead, recorded)
	}
	return fmt.Errorf("%w: refusing to run %s step: cannot verify that worktree HEAD %s descends from the pipeline's recorded head %s: %w",
		errHeadContinuityUnverified, stepName, currentHead, recorded, err)
}

// errHeadContinuityUnverified marks an assertPipelineHeadContinuity refusal
// that comes from git failing, not from git proving the head diverged.
var errHeadContinuityUnverified = errors.New("head continuity unverified")

// commitPipelineCorrection creates a pipeline-authored correction commit with
// hook verification bypassed, and is the single owner of that bypass.
//
// A correction commit is machine-authored: the pipeline records the change its
// own agents or its own formatter produced, inside the throwaway run
// worktree. That worktree is freshly carved from the bare gate repo, so tracked
// hooks that depend on generated untracked runtime files cannot run there - the
// canonical case is a repository whose shared config sets core.hooksPath=.husky
// while a tracked .husky hook sources the generated .husky/_/husky.sh that no
// install step ever created in this worktree. The hook exits nonzero, the
// correction commit fails, and the whole run dies on setup state that says
// nothing about the change under review.
//
// --no-verify alone is not enough, because Git gates only pre-commit and
// commit-msg on it and always runs prepare-commit-msg (builtin/commit.c
// prepare_to_commit), so a repository carrying a legacy .husky
// prepare-commit-msg hook - commitizen and ticket-prefix setups are the common
// ones - still fails the exact commit this helper exists to complete. Pointing
// core.hooksPath at a freshly created empty directory for this one invocation
// covers the whole commit hook family; --no-verify is kept so the intent stays
// explicit at the call. The override lives only in this process argument list
// and the directory is removed afterwards, so nothing persists in the
// repository, the user's configuration, or the daemon's environment.
//
// Reach is deliberately narrow. Only commitAgentFixes (Review, Test, Document,
// Lint, and an operator-authorized repository gate repair) and the Push step's
// leftover-worktree commit route here. These are the two routes that commit the
// pipeline's own agent and formatter output.
// CI repair commits, the generic git runner, and every user-authored commit keep
// hook verification; the core pipeline and repository gates remain the
// authoritative quality checks for what these commits contain.
func commitPipelineCorrection(ctx context.Context, workDir, message string, logf func(string)) error {
	return commitPipelineCorrectionWithCleanup(ctx, workDir, message, logf, os.RemoveAll)
}

func commitPipelineCorrectionWithCleanup(
	ctx context.Context,
	workDir, message string,
	logf func(string),
	cleanup func(string) error,
) error {
	gitRun := func(args ...string) (string, error) { return git.Run(ctx, workDir, args...) }
	staged, err := stagedChangesPresent(gitRun)
	if err != nil {
		return fmt.Errorf("inspect staged correction: %w", err)
	}
	if !staged {
		return nil
	}

	emptyHooksDir, err := os.MkdirTemp("", "no-mistakes-correction-hooks-")
	if err != nil {
		return fmt.Errorf("prepare hook-free commit environment: %w", err)
	}
	_, commitErr := git.Run(ctx, workDir, "-c", "core.hooksPath="+emptyHooksDir, "commit", "--no-verify", "-m", message)
	if cleanupErr := cleanup(emptyHooksDir); cleanupErr != nil {
		if logf != nil {
			logf(fmt.Sprintf("warning: failed to remove temporary hook-free commit directory %s: %v", emptyHooksDir, cleanupErr))
		} else {
			slog.Warn("failed to remove temporary hook-free commit directory", "path", emptyHooksDir, "error", cleanupErr)
		}
	}
	return commitErr
}

// withCommitTrailers appends commit.trailers rendered for producer. Changes not
// attributable to a single invocation get none rather than a guessed agent.
func withCommitTrailers(sctx *pipeline.StepContext, message string, producer *agent.Result) (string, error) {
	if producer == nil || len(sctx.Config.Commit.Trailers) == 0 {
		return message, nil
	}
	// A fallback chain reports the agent that actually ran in Provider; a bare
	// adapter leaves it empty, and then the step agent is the one that ran.
	agentName := producer.Provider
	if agentName == "" && sctx.Agent != nil {
		agentName = sctx.Agent.Name()
	}
	trailers, err := sctx.Config.Commit.RenderTrailers(config.TrailerData{Agent: agentName, Model: producer.Model})
	if err != nil {
		return "", err
	}
	return message + "\n\n" + strings.Join(trailers, "\n"), nil
}

// stagedChangesPresent is the handoff between catch-all staging and commit.
// Worktree status can become stale when an agent completes a rebase itself, or
// can report dirt that `git add -A` cannot put in the superproject index. Only
// the staged index answers whether a correction commit is actually required.
func stagedChangesPresent(gitRun gitRunner) (bool, error) {
	staged, err := gitRun("diff", "--cached", "--name-only", "-z")
	if err != nil {
		return false, err
	}
	return staged != "", nil
}

func commitAgentFixes(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string) error {
	_, err := commitAgentFixesWithResult(sctx, stepName, summary, fallbackSummary, nil, nil, nil)
	return err
}

// commitAgentFixesWithResult commits the worktree changes left by producer, the
// agent invocation that made them. A nil producer commits without trailers.
// before lists the untracked files the worktree held before producer started,
// and unnamed the new files a Review repair did not name; both stay out of the
// commit (see stagePipelineChanges).
func commitAgentFixesWithResult(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string, producer *agent.Result, before, unnamed []string) (bool, error) {
	ctx := sctx.Ctx
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return false, err
	}
	render := func() (string, error) {
		return renderFixCommitMessage(sctx, stepName, summary, fallbackSummary, producer)
	}
	if err := conformAgentCommits(sctx, stepName, render); err != nil {
		return false, err
	}
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check %s changes: %w", stepName, err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no agent changes to commit")
		headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return false, fmt.Errorf("resolve agent head: %w", err)
		}
		return false, recordAgentFixHead(sctx, stepName, headSHA)
	}
	commitMessage, err := render()
	if err != nil {
		return false, err
	}
	if err := stagePipelineChanges(sctx, before, unnamed); err != nil {
		return false, fmt.Errorf("stage %s changes: %w", stepName, err)
	}
	headBeforeCommit, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head before %s commit: %w", stepName, err)
	}
	if err := commitPipelineCorrection(ctx, sctx.WorkDir, commitMessage, sctx.Log); err != nil {
		return false, fmt.Errorf("commit %s changes: %w", stepName, err)
	}
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head after %s commit: %w", stepName, err)
	}
	// An empty staged index is a successful no-op, not a commit. Reporting it
	// as one would claim a head advance that never happened.
	if headSHA == headBeforeCommit {
		sctx.Log("no staged agent changes to commit")
	} else {
		sctx.Log(fmt.Sprintf("committed agent fixes: %s", commitMessage))
	}
	if err := recordAgentFixHead(sctx, stepName, headSHA); err != nil {
		return false, err
	}
	return headSHA != headBeforeCommit, nil
}

// renderFixCommitMessage renders the commit.fix_message subject and the
// commit.trailers for producer, the single message every pipeline-published fix
// commit carries whether the pipeline or the agent created it.
func renderFixCommitMessage(sctx *pipeline.StepContext, stepName types.StepName, summary, fallbackSummary string, producer *agent.Result) (string, error) {
	if summary == "" {
		summary = fallbackSummary
	}
	if summary == "" {
		summary = "apply fixes"
	}
	message, err := sctx.Config.Commit.RenderFixMessageForBranch(stepName, summary, sctx.Run.Branch)
	if err != nil {
		return "", fmt.Errorf("render %s fix commit message: %w", stepName, err)
	}
	message, err = withCommitTrailers(sctx, message, producer)
	if err != nil {
		return "", fmt.Errorf("render %s fix commit trailers: %w", stepName, err)
	}
	return message, nil
}

func recordAgentFixHead(sctx *pipeline.StepContext, stepName types.StepName, headSHA string) error {
	if headSHA == sctx.Run.HeadSHA {
		return nil
	}
	// A side lane works on a copy of the run record in a clone of its own.
	// Its commit becomes the run's only if the owning step takes it, which
	// then records it here in the run's own context.
	if sctx.SideLane {
		sctx.Run.HeadSHA = headSHA
		return nil
	}
	if err := assertPipelineHeadContinuity(sctx, stepName); err != nil {
		return err
	}
	if err := updateNonSharedBranchRef(sctx, headSHA); err != nil {
		return err
	}
	startingHead := strings.TrimSpace(sctx.ReviewStartingHeadSHA)
	if startingHead == "" {
		startingHead = sctx.Run.HeadSHA
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
		return err
	}
	sctx.Run.HeadSHA = headSHA
	if stepName == types.StepReview {
		pipeline.PersistUncertifiedPipelineRange(sctx, startingHead, headSHA)
	}
	stopStaleSideLanes(sctx, headSHA)
	return nil
}

func fixResultSummary(committed bool) string {
	if committed {
		return changesAppliedSummary
	}
	return NoChangesAppliedSummary
}

func extractCommitSummary(result *agent.Result) (string, error) {
	var summary commitSummary
	if result.Output == nil {
		return "", fmt.Errorf("agent returned no structured summary")
	}
	if !utf8.Valid(result.Output) {
		return "", fmt.Errorf("%w: agent output must contain valid UTF-8", errRejectedCommitSummary)
	}
	if err := json.Unmarshal(result.Output, &summary); err != nil {
		return "", fmt.Errorf("parse commit summary: %w", err)
	}
	if len(summary.Summary) > config.MaxFixMessageSummaryBytes {
		return "", fmt.Errorf("%w: commit summary must not exceed %d bytes", errRejectedCommitSummary, config.MaxFixMessageSummaryBytes)
	}
	cleaned := strings.Join(strings.Fields(summary.Summary), " ")
	cleaned = strings.Trim(cleaned, " \t\r\n\"'.;:,-")
	if utf8.RuneCountInString(cleaned) > commitSubjectLimit {
		// Validate the entire summary so shortening cannot hide an unsafe suffix.
		if _, err := (config.Commit{FixMessage: "{{.Summary}}"}).RenderFixMessage("", cleaned); err != nil {
			return "", fmt.Errorf("%w: %v", errRejectedCommitSummary, err)
		}
		cleaned = string([]rune(cleaned)[:commitSubjectLimit])
	}
	return cleaned, nil
}

// keptRepairCutSummary is the commit subject of work a repair turn left at its
// budget, committed on the operator's fix with no repair turn to describe it.
const keptRepairCutSummary = "keep repair work cut at its agent budget"

func executeFixMode(sctx *pipeline.StepContext, stepName types.StepName, opts fixExecutionOptions) (string, error) {
	if !sctx.Fixing {
		return "", nil
	}
	if opts.RequirePreviousFindings && sctx.PreviousFindings == "" {
		return "", errors.New(opts.MissingFindingsError)
	}
	before, err := untrackedFiles(sctx)
	if err != nil {
		return "", err
	}
	if pipeline.OnlyRepairBudgetCut(sctx.PreviousFindings) {
		sctx.Log("fix selection holds only the repair budget cut; keeping what the cut turn left and checking it without a repair turn...")
		committed, err := commitAgentFixesWithResult(sctx, stepName, keptRepairCutSummary, opts.FallbackSummary, nil, before, nil)
		if err != nil {
			return "", err
		}
		return fixResultSummary(committed), nil
	}
	if opts.LogMessage != "" {
		sctx.Log(opts.LogMessage)
	}
	purpose := opts.Purpose
	if purpose == "" {
		purpose = string(stepName) + "-fix"
	}
	runOpts := agent.RunOpts{
		Prompt:     fixerPrompt(opts.Prompt),
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    purpose,
		Workload:   opts.Workload,
	}
	if opts.IsReviewRepair {
		scratch, err := newTestScratch("", runtime.GOOS)
		if err != nil {
			return "", err
		}
		defer func() {
			if err := os.RemoveAll(scratch); err != nil {
				sctx.Log(fmt.Sprintf("warning: failed to remove review repair scratch dir %s: %v", scratch, err))
			}
		}()
		runOpts.Prompt = fixerPrompt(opts.Prompt + fmt.Sprintf(reviewRepairFilesRule, scratch))
		runOpts.JSONSchema = fixSummarySchema
	}
	// The fixer may commit its own work and leave a clean worktree, which
	// commitAgentFixesWithResult reports as "not committed". HEAD movement is
	// what tells that round apart from one that made no edits.
	headBeforeAgent := ""
	if opts.IsReviewRepair {
		headBeforeAgent, err = git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return "", fmt.Errorf("resolve head before %s fix: %w", stepName, err)
		}
	}
	var result *agent.Result
	if opts.RunAgent != nil {
		result, err = opts.RunAgent(runOpts)
	} else {
		result, err = sctx.RunAgentSessionContext(sctx.Ctx, opts.SessionRole, runOpts)
	}
	if err != nil {
		if opts.ErrorPrefix != "" {
			err = fmt.Errorf("%s: %w", opts.ErrorPrefix, err)
		}
		if errors.Is(err, pipeline.ErrAgentTimeout) || errors.Is(err, errReviewAgentTimeout) {
			return "", repairBudgetCut(sctx, stepName, before, err)
		}
		return "", err
	}
	logRepairResult(sctx, result)
	if opts.AfterAgentRun != nil {
		if err := opts.AfterAgentRun(result); err != nil {
			return "", err
		}
	}
	summary, err := extractCommitSummary(result)
	if err != nil {
		if errors.Is(err, errRejectedCommitSummary) {
			return "", fmt.Errorf("validate %s fix summary: %w", stepName, err)
		}
		sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
	}
	var unnamed []string
	if opts.IsReviewRepair {
		if unnamed, err = unnamedNewFiles(sctx, result, before); err != nil {
			return "", err
		}
	}
	committed, err := commitAgentFixesWithResult(sctx, stepName, summary, opts.FallbackSummary, result, before, unnamed)
	if err != nil {
		return "", err
	}
	if err := setAsideUnnamedNewFiles(sctx, unnamed); err != nil {
		return "", err
	}
	if opts.IsReviewRepair {
		headAfterAgent, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return "", fmt.Errorf("resolve head after %s fix: %w", stepName, err)
		}
		logBlockedVerification(sctx, blockedVerification(result), committed || headAfterAgent != headBeforeAgent)
	}
	return fixResultSummary(committed), nil
}

func logRepairResult(sctx *pipeline.StepContext, result *agent.Result) {
	if result.Text != "" && result.Text != string(result.Output) {
		sctx.LogFile(result.Text)
	}
	sctx.LogFile(string(result.Output))
}

// repairBudgetCut keeps what a repair turn left when its budget stopped it. A
// turn past its budget is no trusted result, so nothing is committed here: that
// waits for the operator's fix, and a round then checks the work. The turn's new
// files are staged, because the next turn would otherwise count them among the
// files it found and its commit would leave them out (see stagePipelineChanges).
// An unfinished rebase or merge stays as it is, since staging would mark its
// conflicts resolved. Commits the turn made itself get the commit settings
// before they are recorded as the run head, the message the operator's fix
// gives the rest of the work; a chain conformAgentCommits refuses stays in the
// worktree unrecorded.
func repairBudgetCut(sctx *pipeline.StepContext, stepName types.StepName, before []string, cut error) error {
	startHead := sctx.Run.HeadSHA
	if !rebaseInProgress(sctx.Ctx, sctx.WorkDir) && !mergeInProgress(sctx.Ctx, sctx.WorkDir) {
		if err := stagePipelineChanges(sctx, before, nil); err != nil {
			return fmt.Errorf("%v; keep what it left: %w", cut, err)
		}
	}
	unshaped := conformAgentCommits(sctx, stepName, func() (string, error) {
		return renderFixCommitMessage(sctx, stepName, keptRepairCutSummary, "", nil)
	})
	return &pipeline.RepairBudgetCutError{Cut: cut, Left: unvalidatedWork(sctx, stepName, startHead, before, unshaped)}
}

func updateNonSharedBranchRef(sctx *pipeline.StepContext, headSHA string) error {
	shared, err := worktreeSharesGateRefs(sctx)
	if err != nil || shared {
		return err
	}
	if _, err := stepGitRun(sctx, "update-ref", normalizedBranchRef(sctx.Run.Branch), headSHA); err != nil {
		return fmt.Errorf("update local branch ref: %w", err)
	}
	return nil
}

func worktreeSharesGateRefs(sctx *pipeline.StepContext) (bool, error) {
	if strings.TrimSpace(sctx.GateDir) == "" {
		return false, nil
	}
	gateInfo, err := os.Stat(sctx.GateDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect gate ref storage: %w", err)
	}
	commonDir, err := stepGitRun(sctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("resolve worktree ref storage: %w", err)
	}
	commonInfo, err := os.Stat(commonDir)
	if err != nil {
		return false, fmt.Errorf("inspect worktree ref storage: %w", err)
	}
	return os.SameFile(gateInfo, commonInfo), nil
}
