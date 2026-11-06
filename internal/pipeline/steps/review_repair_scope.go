package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// repairReviewSchema is the review schema plus what only a repair round's
// rereview answers: which selected findings the repair closed, and the
// concrete failure that lets a finding outside the repair keep blocking.
var repairReviewSchema = json.RawMessage(strings.NewReplacer(
	`"review_scope":`,
	`"failing_input": {"type": "string", "description": "Repair rounds only: the concrete input or state that reaches this defect"},
					"wrong_result": {"type": "string", "description": "Repair rounds only: the wrong result that input produces"},
					"review_scope":`,
	`"tested":`,
	`"closed_findings": {"type": "array", "items": {"type": "string"}, "description": "Repair rounds only: the id of each finding awaiting verification that is closed"},
		"tested":`,
).Replace(string(reviewFindingsSchema)))

// repairHunkHeader matches the current-file range of a unified diff hunk. A
// diff body line always starts with a space, "+", "-" or "\", so only a real
// header starts with "@@".
var repairHunkHeader = regexp.MustCompile(`(?m)^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// lineSpan is an inclusive range of line numbers in the current file.
type lineSpan struct {
	first, last int
}

// repairRoundScope is what a repair round's rereview is held to: the repair's
// own change, the files no earlier round judged, and the findings awaiting
// verification. Everything else on the branch was judged by the rounds before
// it.
type repairRoundScope struct {
	startHead string
	workDir   string
	// changed maps each file the repair touched to its hunks, context lines
	// included, so a finding anchored a few lines off still counts as inside. A
	// file with no hunks, such as a binary or deleted one, is inside as a whole.
	changed map[string][]lineSpan
	// tracked is every path in the repository. A finding's file is the
	// reviewer's own spelling, so one that names none of these cannot be placed.
	tracked   map[string]bool
	firstLook []string
	// verifying is what can clear on this round's coverage: the findings the
	// repair was sent to close and any earlier selection still unverified.
	verifying []Finding
	// deferred is every other open finding. None of them can clear here.
	deferred     []Finding
	previousRisk string
	closed       map[string]bool
}

// repairRoundCanNarrow reports whether a repair round can hold its rereview to
// the repair. With no previous record nothing is known to be judged, a finding
// with no file can be verified only by a pass over every path, and a round
// left with nothing of its own to judge reviews in full, as it does for
// fixRoundCarriedPaths.
func repairRoundCanNarrow(reviewable, previouslyReviewed, changedSinceStart []string, open []Finding) bool {
	if len(previouslyReviewed) == 0 {
		return false
	}
	judgedAgain := append([]string(nil), changedSinceStart...)
	for _, finding := range open {
		if normalizeReviewedPath(finding.File) == "" {
			return false
		}
		judgedAgain = append(judgedAgain, finding.File)
	}
	return len(uncoveredReviewablePaths(previouslyReviewed, reviewable)) > 0 ||
		len(uncoveredReviewablePaths(judgedAgain, reviewable)) < len(reviewable)
}

func newRepairRoundScope(ctx context.Context, workDir, startHead string, changedSinceStart, reviewable, previouslyReviewed []string, selected Findings, deferred []Finding, pendingIDs []string) (*repairRoundScope, error) {
	listed, err := git.Run(ctx, workDir, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("list tracked files: %w", err)
	}
	trackedPaths := changedPathList(listed)
	tracked := make(map[string]bool, len(trackedPaths))
	for _, path := range trackedPaths {
		tracked[path] = true
	}
	scope := &repairRoundScope{
		startHead:    startHead,
		workDir:      workDir,
		changed:      make(map[string][]lineSpan, len(changedSinceStart)),
		tracked:      tracked,
		firstLook:    uncoveredReviewablePaths(previouslyReviewed, reviewable),
		verifying:    withoutReviewQuestions(selected.Items),
		previousRisk: selected.RiskLevel,
	}
	for _, finding := range withoutReviewQuestions(deferred) {
		if slices.Contains(pendingIDs, finding.ID) {
			scope.verifying = append(scope.verifying, finding)
		} else {
			scope.deferred = append(scope.deferred, finding)
		}
	}
	for _, changed := range changedSinceStart {
		diff, err := git.Run(ctx, workDir, "diff", "--no-ext-diff", "--no-color", "--no-renames", "-U3", startHead, "--", ":(literal)"+changed)
		if err != nil {
			return nil, fmt.Errorf("get repair hunks for %s: %w", changed, err)
		}
		scope.changed[normalizeReviewedPath(changed)] = repairHunks(diff)
	}
	return scope, nil
}

// withoutReviewQuestions leaves out what only an answer resolves. Every review
// turn emits the open questions again, and a reviewer told to report one back
// would add a row with no category, which no answer can clear.
func withoutReviewQuestions(findings []Finding) []Finding {
	var kept []Finding
	for _, finding := range findings {
		if finding.Category != types.FindingCategoryReviewQuestion {
			kept = append(kept, finding)
		}
	}
	return kept
}

// repairHunks returns the current-file line spans of a diff's hunks, or nil
// when the file has no such lines to anchor to: a binary or mode change has no
// hunks, and a hunk of zero current lines means the file is now empty or gone.
func repairHunks(diff string) []lineSpan {
	var spans []lineSpan
	for _, header := range repairHunkHeader.FindAllStringSubmatch(diff, -1) {
		first, err := strconv.Atoi(header[1])
		if err != nil {
			return nil
		}
		count := 1
		if header[2] != "" {
			if count, err = strconv.Atoi(header[2]); err != nil {
				return nil
			}
		}
		if count == 0 {
			return nil
		}
		spans = append(spans, lineSpan{first: first, last: first + count - 1})
	}
	return spans
}

// reviewScope states the round's scope for the prompt's context block.
func (r *repairRoundScope) reviewScope(baseSHA string) string {
	scope := fmt.Sprintf("only this repair's change since starting head %s", r.startHead)
	if len(r.firstLook) > 0 {
		scope += fmt.Sprintf(", plus the branch changes relative to base commit %s in the files no earlier round judged", baseSHA)
	}
	return scope
}

// promptSection gives the rereview its checklist and its follow-up rule.
func (r *repairRoundScope) promptSection() string {
	var b strings.Builder
	b.WriteString("\n" + "Repair round scope:\n")
	fmt.Fprintf(&b, "- This round reads the repair's own change, the diff from starting head %s to the current worktree. Earlier rounds judged the rest of the branch, so do not review it again. Wherever this prompt says the change or the changed code, read this repair's change, and read surrounding code only to judge the repair.\n", r.startHead)
	if len(r.firstLook) > 0 {
		b.WriteString("- No earlier round judged these files, so read their whole branch change:\n")
		for _, path := range r.firstLook {
			fmt.Fprintf(&b, "  - %s\n", coveragePathLine(path))
		}
	}
	if len(r.verifying) > 0 {
		b.WriteString("- These findings await verification, because this repair or an earlier one was sent to close them. Trace each one's failing sequence through the current code:\n")
		for _, finding := range r.verifying {
			fmt.Fprintf(&b, "  - %s: %s:%d: %s\n", sanitizePromptText(finding.ID), sanitizePromptText(finding.File), finding.Line, sanitizePromptText(finding.Description))
		}
		b.WriteString("- Name each one that is closed in closed_findings, by id. Report one that is not closed again in findings. Until every one of them is named closed, the pipeline blocks on every finding you report.\n")
	}
	b.WriteString("- A defect outside the repair's change is a follow-up: report it, and the pipeline records it without blocking this run. It blocks only when you set failing_input to the concrete input or state that reaches it and wrong_result to the wrong result that input produces. Set them only for a failure you traced, and state both in the description too. An ask-user finding parks the run wherever it sits, so outside the repair's change keep it for a decision only a person can make.\n")
	b.WriteString("- risk_level, risk_rationale and risk_scope still describe the whole branch, not this repair.")
	if r.previousRisk != "" {
		fmt.Fprintf(&b, " The previous round set risk_level to %q, so change it only for what this repair changed.", sanitizePromptText(r.previousRisk))
	}
	b.WriteString("\n")
	return b.String()
}

// split moves each finding outside the repair from Items to FollowUps, where it
// is on the record and blocks nothing, and keeps only the ids awaiting
// verification in ClosedFindings.
func (r *repairRoundScope) split(log func(string), findings *Findings) {
	if r.closed == nil {
		r.closed = make(map[string]bool, len(r.verifying))
	}
	var closed []string
	for _, id := range findings.ClosedFindings {
		for _, open := range r.verifying {
			if open.ID == id {
				r.closed[id] = true
				closed = append(closed, id)
				break
			}
		}
	}
	findings.ClosedFindings = closed
	if len(closed) > 0 {
		log(fmt.Sprintf("rereview confirmed closed: %s", strings.Join(closed, ", ")))
	}

	var blocking []Finding
	var locations []string
	for _, finding := range findings.Items {
		if !r.isFollowUp(finding) {
			blocking = append(blocking, finding)
			continue
		}
		findings.FollowUps = append(findings.FollowUps, finding)
		locations = append(locations, fmt.Sprintf("%s:%d", sanitizePromptText(finding.File), finding.Line))
	}
	findings.Items = blocking
	if len(locations) > 0 {
		findings.Summary = filteredReviewSummary(blocking)
		log(fmt.Sprintf("recorded %d review follow-up(s) outside the repair's change, not blocking: %s", len(locations), strings.Join(locations, ", ")))
	}
}

// isFollowUp reports whether a finding is shown to lie outside the repair and
// names no concrete failure. Any doubt keeps it blocking: an anchor must name
// an existing line in a readable tracked file, and a file no earlier round
// judged is being reviewed for the first time. An ask-user finding is a
// person's decision, and the intent and access rules that raise one are not
// the pipeline's to waive.
//
// Nothing leaves Items until the rereview names closed every finding awaiting
// verification. Any finding may be one of those moved, reworded or reported
// where it still reaches, and taking it out would clear that one on silence
// (pipeline.resolveVerifiedFindingsJSON). Any other deferred finding cannot
// clear here, so it holds nothing back. A finding repeating an open finding is
// not new: it contradicts a closed verdict, and a copy of a carried finding
// belongs in Items, where it merges.
func (r *repairRoundScope) isFollowUp(finding Finding) bool {
	file := normalizeReviewedPath(finding.File)
	if !r.tracked[file] || finding.Line <= 0 || finding.ActionOrDefault() == types.ActionAskUser {
		return false
	}
	if strings.TrimSpace(finding.FailingInput) != "" && strings.TrimSpace(finding.WrongResult) != "" {
		return false
	}
	for _, path := range r.firstLook {
		if normalizeReviewedPath(path) == file {
			return false
		}
	}
	if spans, touched := r.changed[file]; touched {
		if len(spans) == 0 {
			return false
		}
		for _, span := range spans {
			if finding.Line >= span.first && finding.Line <= span.last {
				return false
			}
		}
	}
	for _, open := range r.verifying {
		if !r.closed[open.ID] || repeatsFinding(open, finding) {
			return false
		}
	}
	for _, open := range r.deferred {
		if repeatsFinding(open, finding) {
			return false
		}
	}
	content, err := os.ReadFile(filepath.Join(r.workDir, file))
	if err != nil {
		return false
	}
	lineCount := bytes.Count(content, []byte{'\n'})
	if len(content) > 0 && content[len(content)-1] != '\n' {
		lineCount++
	}
	return finding.Line <= lineCount
}

// repeatsFinding compares descriptions as the prompt showed them.
func repeatsFinding(open, reported Finding) bool {
	return sanitizePromptText(open.Description) == sanitizePromptText(reported.Description)
}
