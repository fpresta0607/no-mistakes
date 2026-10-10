package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns the three operator and repository rules that bound the Test
// step's live check (its evidence turn). Each is off unless configured, and
// with all three off the step behaves exactly as it did before they existed:
//
//   - test.surface_paths (trusted repository): a change that touches none of
//     the listed paths skips the evidence turn. commands.test still runs.
//   - test_live_check_budget (global): the evidence turn is stopped at the
//     budget and the step completes with what the turn wrote down.
//   - test.environment (trusted repository): the evidence agent is told to use
//     the repository's prepared environment and to build none of its own.

// liveCheckDirName is the folder in a run's evidence directory that holds what
// a budgeted live check writes for the step itself: the scenario ledger and
// anything set aside from the worktree. It is not evidence.
const (
	liveCheckDirName      = "live-check"
	liveCheckLedgerName   = "progress.ndjson"
	liveCheckSetAsideName = "set-aside"
	// liveCheckLedgerMaxBytes and liveCheckLedgerMaxScenarios bound what the
	// step reads back from a file an agent wrote.
	liveCheckLedgerMaxBytes     = 1 << 20
	liveCheckLedgerMaxScenarios = 100
	liveCheckPlanned            = "planned"
)

// errTestLiveCheckBudget is the cancellation cause of an evidence turn that
// reached test_live_check_budget. It is deliberately not errTestAgentTimeout:
// that cause parks the run, and this one completes the step.
var errTestLiveCheckBudget = errors.New("test live check budget")

// evidencePublishExcludedDirs names the folders of a run's evidence directory
// that are never published to the evidence branch. The review conversation is
// excluded for the reason evidence_publish.go records; the live check folder
// holds a ledger and set-aside work, neither of which is evidence.
func evidencePublishExcludedDirs() []string {
	return []string{reviewqa.DirName, liveCheckDirName}
}

// liveSurfaceUntouched reports whether trusted test.surface_paths is set and
// the change touches none of it. The complete changed-file set is matched,
// never the ignore_patterns-filtered one (a pushed-branch field must not
// decide whether the live check runs), renames count on both sides, and the
// worktree is compared with the base so a fix turn's edits count too.
func liveSurfaceUntouched(sctx *pipeline.StepContext, baseSHA string) (bool, error) {
	patterns := sctx.Config.Test.SurfacePaths
	if len(patterns) == 0 {
		return false, nil
	}
	changedFiles, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "-z", "--no-renames", baseSHA)
	if err != nil {
		return false, fmt.Errorf("list changed files for test.surface_paths: %w", err)
	}
	for _, file := range changedPathList(changedFiles) {
		for _, pattern := range patterns {
			if matchIgnorePattern(file, pattern) {
				return false, nil
			}
		}
	}
	return true, nil
}

// liveSurfaceSkipFindings is the Test record of a live check the repository's
// own rule skipped. It carries no verdict and no scenario: nothing was driven
// live, so the pull request's attestation omits live_validation instead of
// claiming one.
func liveSurfaceSkipFindings(sctx *pipeline.StepContext, ranBaseline bool) Findings {
	rule := "test.surface_paths: " + strings.Join(sctx.Config.Test.SurfacePaths, ", ")
	ran := "no test command is configured, so the Test step ran nothing for this change"
	if ranBaseline {
		ran = "the configured test command still ran"
	}
	sctx.Log(fmt.Sprintf("live check skipped: this change touches none of the repository's surface paths (%s); %s", rule, ran))
	return Findings{
		TestingSummary: fmt.Sprintf("Live check skipped by the repository's rule: this change touches none of its surface paths (%s); %s.", rule, ran),
	}
}

// liveCheckBudget is the effective test_live_check_budget: zero when unset,
// and never above the Test agent's own limit.
func liveCheckBudget(sctx *pipeline.StepContext) time.Duration {
	if sctx == nil || sctx.Config == nil || sctx.Config.TestLiveCheckBudget <= 0 {
		return 0
	}
	return min(sctx.Config.TestLiveCheckBudget, testAgentTimeout(sctx))
}

func liveCheckLedgerPath(evidenceDir string) string {
	return filepath.Join(evidenceDir, liveCheckDirName, liveCheckLedgerName)
}

// liveCheckPromptSections are appended after everything else in the evidence
// prompt, so with both settings off the prompt is byte for byte what it was
// (TestTestStep_LiveCheckRulesOffIsTodaysPrompt).
func liveCheckPromptSections(sctx *pipeline.StepContext, evidenceDir string) string {
	var b strings.Builder
	if environment := strings.TrimSpace(sctx.Config.Test.Environment); environment != "" {
		b.WriteString("\n\nPrepared environment (trusted, from the default branch; this replaces the instruction above to build a disposable environment yourself):\n")
		b.WriteString(sanitizePromptMultilineText(environment))
		b.WriteString(`
- Use this prepared environment for everything a scenario needs: the product instance, its data, its accounts, and its services.
- Build no environment of your own: no database, no service stand-in, no seed data, no second instance of the product.
- If the prepared environment does not start, or lacks what a scenario needs, report that scenario "untested" with what failed. Do not build a replacement.`)
	}
	if budget := liveCheckBudget(sctx); budget > 0 {
		b.WriteString(fmt.Sprintf(`

Live check budget (set by the operator; this turn is stopped when it runs out):
- You have %s for this whole live check, starting now. It is not extended.
- Before you drive anything, append one line per scenario you plan to drive to the ledger file, each a single line of JSON: {"name": "<scenario name>", "result": "planned"}
- As soon as a scenario is settled, append its line: {"name": "<the same name>", "result": "pass" | "fail" | "untested", "live": true | false, "evidence": "<what shows it>", "reason": "<why untested>"}. Append only and never rewrite the file; the last line for a name is the one that counts.
- Ledger file: %s
- When the budget runs out this turn is stopped and the ledger is all that remains of it: a recorded live pass counts, a recorded failure is reported, and a planned scenario with no result is listed on the pull request as untested. Anything this turn left in the working tree is set aside and not pushed.
- Drive the scenarios that matter most to the intent first, and spend the budget on driving scenarios rather than on setup that does not finish.
- If you finish inside the budget, return the full JSON result as usual.`, budget, liveCheckLedgerPath(evidenceDir)))
	}
	return b.String()
}

// liveCheckTurn is one evidence turn's budget and the worktree state it
// started from, which decides what a turn stopped at the budget may complete
// with.
type liveCheckTurn struct {
	ctx    context.Context
	cancel context.CancelFunc
	budget time.Duration
	head   string
	// clean is true when the worktree held no changes before the turn, so
	// everything in it afterwards is the stopped turn's own.
	clean bool
}

// beginLiveCheck binds the evidence turn to test_live_check_budget when one is
// set. Without one the turn runs on the step context exactly as before.
func beginLiveCheck(sctx *pipeline.StepContext, evidenceDir string) liveCheckTurn {
	budget := liveCheckBudget(sctx)
	if budget <= 0 {
		return liveCheckTurn{ctx: sctx.Ctx, cancel: func() {}}
	}
	turn := liveCheckTurn{budget: budget}
	if head, err := stepGitHeadSHA(sctx); err == nil {
		if status, err := stepGitRunRaw(sctx, "status", "--porcelain"); err == nil {
			turn.head = head
			turn.clean = strings.TrimSpace(status) == ""
		}
	}
	// Every turn starts its own ledger: an earlier round's lines describe
	// another head.
	if err := os.Remove(liveCheckLedgerPath(evidenceDir)); err != nil && !os.IsNotExist(err) {
		sctx.Log(fmt.Sprintf("warning: could not clear the live check ledger: %v", err))
	}
	turn.ctx, turn.cancel = context.WithTimeoutCause(sctx.Ctx, budget, errTestLiveCheckBudget)
	return turn
}

// completeAtBudget turns a live check stopped at its budget into the step's
// findings, or returns false when the stop must park as a Test agent cut
// always has. It parks when completing could push or clear something nobody
// validated:
//
//   - a fix round whose gate held a failed or inconclusive live check, or any
//     blocking finding: the turn was stopped before it could show that gone;
//   - a turn that committed, or left a rebase or merge unfinished: that cannot
//     be put back without rewriting the run's branch;
//   - a worktree that was already changed before the turn, or leftovers that
//     could not be set aside: the step cannot tell what is the turn's own.
func (turn liveCheckTurn) completeAtBudget(sctx *pipeline.StepContext, evidenceDir string) (Findings, bool) {
	if carried := answeredTestGate(sctx); hasBlockingFindings(carried.Items) || carried.Verdict == types.TestVerdictNoGo || carried.Verdict == types.TestVerdictInconclusive {
		return Findings{}, false
	}
	if !turn.clean || turn.head == "" {
		return Findings{}, false
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil || head != turn.head || rebaseInProgress(sctx.Ctx, sctx.WorkDir) || mergeInProgress(sctx.Ctx, sctx.WorkDir) {
		return Findings{}, false
	}
	setAsideDir, setAside, err := setAsideLiveCheckWork(sctx, evidenceDir)
	if err != nil {
		sctx.Log(fmt.Sprintf("the live check reached its %s budget and what it left in the worktree could not be set aside, so the step parks: %v", turn.budget, err))
		return Findings{}, false
	}

	scenarios := readLiveCheckLedger(liveCheckLedgerPath(evidenceDir), turn.budget)
	var passed, failed, untested int
	for _, scenario := range scenarios {
		switch scenario.Result {
		case types.ScenarioResultPass:
			passed++
		case types.ScenarioResultFail:
			failed++
		default:
			untested++
		}
	}
	counts := fmt.Sprintf("%d passed live, %d failed, %d untested", passed, failed, untested)
	note := fmt.Sprintf("The live check was stopped at its %s budget (test_live_check_budget) and the Test step completed with what it had written down: %s. Untested scenarios are listed on the pull request.", turn.budget, counts)
	if len(setAside) > 0 {
		const maxNamed = 10
		named := strings.Join(setAside[:min(len(setAside), maxNamed)], ", ")
		if len(setAside) > maxNamed {
			named += fmt.Sprintf(" and %d more", len(setAside)-maxNamed)
		}
		note += fmt.Sprintf(" What the stopped turn left in the worktree was set aside under %s and is not part of this change: %s.", setAsideDir, named)
	}
	sctx.Log(note)

	findings := Findings{
		Scenarios:      scenarios,
		TestingSummary: fmt.Sprintf("The live check was stopped at its %s budget: %s.", turn.budget, counts),
		Items: []Finding{{
			ID:          types.FindingIDTestLiveCheckBudget,
			Severity:    types.FindingSeverityInfo,
			Action:      types.ActionNoOp,
			Description: note,
		}},
	}
	// A recorded failure is the same no-go a finished live check reports, so
	// verdictFindings parks it as an auto-fixable error. Anything else earns
	// no verdict: the turn did not finish, so it cannot say "go".
	if failed > 0 {
		findings.Verdict = types.TestVerdictNoGo
	}
	return findings, true
}

// readLiveCheckLedger reads the scenarios a stopped live check wrote down, in
// the order it first named them, the last line for a name winning. The ledger
// is an agent's own account, so it is held to the live-validation contract: a
// pass or fail counts only when it is live with evidence, and everything else
// is untested with a reason. Unreadable lines are skipped.
func readLiveCheckLedger(path string, budget time.Duration) []types.TestScenario {
	type entry struct {
		Name     string `json:"name"`
		Result   string `json:"result"`
		Live     bool   `json:"live"`
		Evidence string `json:"evidence"`
		Reason   string `json:"reason"`
	}
	var order []string
	latest := map[string]entry{}
	if f, err := os.Open(path); err == nil {
		data, _ := io.ReadAll(io.LimitReader(f, liveCheckLedgerMaxBytes))
		f.Close()
		for _, line := range strings.Split(string(data), "\n") {
			var e entry
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) != nil {
				continue
			}
			e.Name = strings.TrimSpace(e.Name)
			if e.Name == "" {
				continue
			}
			if _, seen := latest[e.Name]; !seen {
				if len(order) == liveCheckLedgerMaxScenarios {
					continue
				}
				order = append(order, e.Name)
			}
			latest[e.Name] = e
		}
	}
	if len(order) == 0 {
		return []types.TestScenario{{
			Name:   "live check",
			Result: types.ScenarioResultUntested,
			Reason: fmt.Sprintf("the live check recorded no scenario before its %s budget ran out", budget),
		}}
	}
	scenarios := make([]types.TestScenario, 0, len(order))
	for _, name := range order {
		e := latest[name]
		settled := e.Result == types.ScenarioResultPass || e.Result == types.ScenarioResultFail
		switch {
		case settled && e.Live && strings.TrimSpace(e.Evidence) != "":
			scenarios = append(scenarios, types.TestScenario{Name: name, Result: e.Result, Live: true, Evidence: strings.TrimSpace(e.Evidence)})
			continue
		case settled:
			e.Reason = fmt.Sprintf("recorded as %q without live evidence before the live check's %s budget ran out", e.Result, budget)
		case e.Result == types.ScenarioResultUntested && strings.TrimSpace(e.Reason) != "":
			e.Reason = strings.TrimSpace(e.Reason)
		default:
			e.Reason = fmt.Sprintf("not reached before the live check's %s budget ran out", budget)
		}
		scenarios = append(scenarios, types.TestScenario{Name: name, Result: types.ScenarioResultUntested, Reason: e.Reason})
	}
	return scenarios
}

// setAsideLiveCheckWork moves what a stopped live check left in a worktree
// that was clean before it into the run's evidence directory, and puts the
// worktree back at HEAD. Untracked paths are moved whole; changes to tracked
// files are kept as one patch. Nothing is deleted, and nothing the turn left
// can ride into a later pipeline commit. It returns the folder and the paths
// set aside, or an error when the worktree could not be made clean, in which
// case the caller parks.
func setAsideLiveCheckWork(sctx *pipeline.StepContext, evidenceDir string) (string, []string, error) {
	status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--no-renames")
	if err != nil {
		return "", nil, fmt.Errorf("read worktree status: %w", err)
	}
	var paths, untracked []string
	tracked := false
	for _, item := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
		if item == "" {
			continue
		}
		if len(item) < 4 || item[2] != ' ' {
			return "", nil, fmt.Errorf("invalid git status entry %q", item)
		}
		path := item[3:]
		paths = append(paths, path)
		if item[:2] == "??" {
			untracked = append(untracked, path)
		} else {
			tracked = true
		}
	}
	if len(paths) == 0 {
		return "", nil, nil
	}

	dir := filepath.Join(evidenceDir, liveCheckDirName, liveCheckSetAsideName)
	for n := 2; ; n++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(evidenceDir, liveCheckDirName, fmt.Sprintf("%s-%d", liveCheckSetAsideName, n))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create set-aside folder: %w", err)
	}
	if tracked {
		patch, err := stepGitRunRaw(sctx, "diff", "--binary", "HEAD")
		if err != nil {
			return "", nil, fmt.Errorf("read tracked changes: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tracked-changes.patch"), []byte(patch), 0o644); err != nil {
			return "", nil, fmt.Errorf("keep tracked changes: %w", err)
		}
	}
	for _, path := range untracked {
		rel := filepath.FromSlash(strings.TrimSuffix(path, "/"))
		if !filepath.IsLocal(rel) {
			return "", nil, fmt.Errorf("untracked path %q is outside the worktree", path)
		}
		target := filepath.Join(dir, "files", rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", nil, fmt.Errorf("create set-aside folder for %s: %w", path, err)
		}
		if err := os.Rename(filepath.Join(sctx.WorkDir, rel), target); err != nil {
			return "", nil, fmt.Errorf("move %s: %w", path, err)
		}
	}
	if tracked {
		if _, err := stepGitRun(sctx, "reset", "--hard", "HEAD"); err != nil {
			return "", nil, fmt.Errorf("restore tracked files: %w", err)
		}
	}
	after, err := stepGitRunRaw(sctx, "status", "--porcelain")
	if err != nil {
		return "", nil, fmt.Errorf("re-read worktree status: %w", err)
	}
	if strings.TrimSpace(after) != "" {
		return "", nil, fmt.Errorf("the worktree still holds changes after the set-aside: %s", strings.Join(porcelainPaths(after), ", "))
	}
	return dir, paths, nil
}
