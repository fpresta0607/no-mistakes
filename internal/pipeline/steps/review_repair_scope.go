package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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
	`"closed_findings": {"type": "array", "items": {"type": "string"}, "description": "Repair rounds only: the id of each selected finding this repair closed"},
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
// own change, the files no earlier round judged, and the findings the repair
// was sent to close. Everything else on the branch was judged by the rounds
// before it.
type repairRoundScope struct {
	startHead string
	// changed maps each file the repair touched to its hunks, context lines
	// included, so a finding anchored a few lines off still counts as inside. A
	// file with no hunks, such as a binary or deleted one, is inside as a whole.
	changed map[string][]lineSpan
	// tracked is every path in the repository. A finding's file is the
	// reviewer's own spelling, so one that names none of these cannot be placed.
	tracked      map[string]bool
	firstLook    []string
	selected     []Finding
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

func newRepairRoundScope(ctx context.Context, workDir, startHead string, changedSinceStart, reviewable, previouslyReviewed []string, selected Findings, deferred []Finding) (*repairRoundScope, error) {
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
		changed:      make(map[string][]lineSpan, len(changedSinceStart)),
		tracked:      tracked,
		firstLook:    uncoveredReviewablePaths(previouslyReviewed, reviewable),
		selected:     selected.Items,
		deferred:     deferred,
		previousRisk: selected.RiskLevel,
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
	if len(r.selected) > 0 {
		b.WriteString("- The repair was sent to close these findings. Trace each one's failing sequence through the current code:\n")
		writeRepairChecklist(&b, r.selected)
		b.WriteString("- Name each finding the repair closed in closed_findings, by id. Report a finding the repair did not close again in findings. Until every one of them is named closed, the pipeline blocks on every finding you report.\n")
	}
	if len(r.deferred) > 0 {
		b.WriteString("- These findings stay open from earlier rounds and this repair was not sent to close them. Check each one against the current code, and report it again if it still holds:\n")
		writeRepairChecklist(&b, r.deferred)
	}
	b.WriteString("- A defect outside the repair's change is a follow-up: report it, and the pipeline records it without blocking this run. It blocks only when you set failing_input to the concrete input or state that reaches it and wrong_result to the wrong result that input produces. Set them only for a failure you traced, and state both in the description too. An ask-user finding parks the run wherever it sits, so outside the repair's change keep it for a decision only a person can make.\n")
	b.WriteString("- risk_level, risk_rationale and risk_scope still describe the whole branch, not this repair.")
	if r.previousRisk != "" {
		fmt.Fprintf(&b, " The previous round set risk_level to %q, so change it only for what this repair changed.", sanitizePromptText(r.previousRisk))
	}
	b.WriteString("\n")
	return b.String()
}

func writeRepairChecklist(b *strings.Builder, findings []Finding) {
	for _, finding := range findings {
		fmt.Fprintf(b, "  - %s: %s:%d: %s\n", sanitizePromptText(finding.ID), sanitizePromptText(finding.File), finding.Line, sanitizePromptText(finding.Description))
	}
}

// split moves each finding outside the repair from Items to FollowUps, where it
// is on the record and blocks nothing, and keeps only selected ids in
// ClosedFindings.
func (r *repairRoundScope) split(log func(string), findings *Findings) {
	if r.closed == nil {
		r.closed = make(map[string]bool, len(r.selected))
	}
	var closed []string
	for _, id := range findings.ClosedFindings {
		for _, selected := range r.selected {
			if selected.ID == id {
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
// names no concrete failure. Any doubt keeps it blocking: a finding with no
// line, or a file that names no tracked path, cannot be placed, and a file no
// earlier round judged is being reviewed for the first time. An ask-user
// finding is a person's decision, and the intent and access rules that raise
// one are not the pipeline's to waive.
//
// Nothing leaves Items until the rereview names every selected finding closed.
// Any finding may be a selected defect moved, reworded or reported where it
// still reaches, and taking it out would clear that selected finding on
// silence (pipeline.resolveVerifiedFindingsJSON). A finding repeating a selected
// description still contradicts the verdict. A finding beside a deferred one
// stays for the same reason, since a deferred finding cannot be named closed.
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
	for _, open := range r.selected {
		if !r.closed[open.ID] || sanitizePromptText(open.Description) == sanitizePromptText(finding.Description) {
			return false
		}
	}
	for _, open := range r.deferred {
		if normalizeReviewedPath(open.File) == file {
			return false
		}
	}
	return true
}
