package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	repairScopeMarker = "Repair round scope:"
	cleanRereview     = `"summary":"s","risk_level":"low","risk_rationale":"r","risk_scope":"source-or-external"`
)

func TestRepairHunks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		diff string
		want []lineSpan
	}{
		{"a hunk spans its context lines", "diff --git a/f b/f\n@@ -5,7 +5,7 @@ func f() {\n context\n", []lineSpan{{5, 11}}},
		{"a hunk of one line has no count", "@@ -1 +1 @@\n-a\n+b\n", []lineSpan{{1, 1}}},
		{"every hunk counts", "@@ -1,3 +1,4 @@\n a\n@@ -20,7 +21,6 @@\n b\n", []lineSpan{{1, 4}, {21, 26}}},
		{"a removed line that reads like a header is not one", "@@ -1,2 +1,1 @@\n-@@ -9 +9 @@\n keep\n", []lineSpan{{1, 1}}},
		{"a deleted file has no lines left, so all of it counts", "@@ -1,3 +0,0 @@\n-a\n-b\n-c\n", nil},
		{"a binary change has no hunks, so all of it counts", "Binary files a/x and b/x differ\n", nil},
		{"a mode change has no hunks, so all of it counts", "old mode 100644\nnew mode 100755\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := repairHunks(tc.diff)

			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("repairHunks = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRepairRoundCanNarrow(t *testing.T) {
	t.Parallel()
	anchored := Finding{ID: "f-1", File: "a.go", Line: 1}
	reviewable := []string{"a.go", "b.go"}
	cases := []struct {
		name     string
		previous []string
		changed  []string
		open     []Finding
		want     bool
	}{
		{"a previous record and anchored findings", reviewable, nil, []Finding{anchored}, true},
		{"a repair with nothing selected", reviewable, []string{"a.go"}, nil, true},
		{"nothing selected, as after a coverage park", []string{"a.go"}, nil, nil, true},
		{"no previous record means nothing was judged", nil, []string{"a.go"}, []Finding{anchored}, false},
		{"a finding with no file needs a pass over every path", reviewable, nil, []Finding{anchored, {ID: "f-2"}}, false},
		{"nothing of its own to judge reviews in full", reviewable, nil, nil, false},
		{"a change only outside the reviewable files leaves nothing to judge", reviewable, []string{"ignored.lock"}, nil, false},
		{"a finding only outside the reviewable files leaves nothing to judge", reviewable, nil, []Finding{{ID: "f-3", File: "ignored.lock"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := repairRoundCanNarrow(reviewable, tc.previous, tc.changed, tc.open); got != tc.want {
				t.Fatalf("repairRoundCanNarrow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepairRoundScopeSplit(t *testing.T) {
	t.Parallel()
	const selectedDefect = "the selected  defect\nsecond line"
	outside := Finding{File: "other.go", Line: 3, Description: "a new thought", Action: types.ActionAutoFix}
	withFile := func(finding Finding, file string, line int) Finding {
		finding.File, finding.Line = file, line
		return finding
	}
	unverified := []Finding{{ID: "f-3", File: "earlier.go", Line: 7, Severity: "info", Description: "an earlier selection"}}
	cases := []struct {
		name         string
		finding      Finding
		closed       []string
		unverified   []Finding
		wantFollowUp bool
	}{
		{name: "a file the repair never touched", finding: outside, wantFollowUp: true},
		{name: "a spelling that still names a tracked file", finding: withFile(outside, "./internal/repaired.go", 30), wantFollowUp: true},
		{name: "a backslash path cannot be placed", finding: withFile(outside, `internal\repaired.go`, 15)},
		{name: "an absolute path cannot be placed", finding: withFile(outside, "/work/repo/internal/repaired.go", 15)},
		{name: "a drive path cannot be placed", finding: withFile(outside, `D:\work\repo\internal\repaired.go`, 15)},
		{name: "a diff prefix cannot be placed", finding: withFile(outside, "b/internal/repaired.go", 15)},
		{name: "another case cannot be placed", finding: withFile(outside, "Internal/Repaired.go", 15)},
		{name: "a file the repository does not have", finding: withFile(outside, "absent.go", 3)},
		{name: "an unchanged line of a repaired file", finding: withFile(outside, "repaired.go", 30), wantFollowUp: true},
		{name: "the line just past a hunk", finding: withFile(outside, "repaired.go", 21), wantFollowUp: true},
		{name: "the last line of a hunk", finding: withFile(outside, "repaired.go", 20)},
		{name: "inside the repair's change", finding: withFile(outside, "repaired.go", 15)},
		{name: "a file changed with no line to read", finding: withFile(outside, "binary.dat", 9)},
		{name: "a file no earlier round judged", finding: withFile(outside, "first-look.go", 3)},
		{name: "no file cannot be shown outside", finding: withFile(outside, "", 0)},
		{name: "no line cannot be shown outside", finding: withFile(outside, "other.go", 0)},
		{
			name:    "it names the input and the wrong result",
			finding: Finding{File: "other.go", Line: 3, Description: "d", Action: types.ActionAutoFix, FailingInput: "an empty list", WrongResult: "it panics"},
		},
		{
			name:         "an input without a wrong result",
			finding:      Finding{File: "other.go", Line: 3, Description: "d", Action: types.ActionAutoFix, FailingInput: "an empty list", WrongResult: "  "},
			wantFollowUp: true,
		},
		{
			name:         "a wrong result without an input",
			finding:      Finding{File: "other.go", Line: 3, Description: "d", Action: types.ActionAutoFix, WrongResult: "it panics"},
			wantFollowUp: true,
		},
		{name: "an ask-user finding is a person's decision", finding: Finding{File: "other.go", Line: 3, Description: "d", Action: types.ActionAskUser}},
		{name: "an unclassified finding fails closed to ask-user", finding: Finding{File: "other.go", Line: 3, Description: "d"}},
		{name: "beside a selected finding not named closed", finding: withFile(outside, "selected.go", 90), closed: []string{}},
		{name: "in another file while a selected finding has no verdict", finding: outside, closed: []string{}},
		{name: "beside a selected finding named closed", finding: withFile(outside, "selected.go", 90), wantFollowUp: true},
		{name: "a selected finding named closed and reported again", finding: Finding{File: "selected.go", Line: 90, Description: selectedDefect, Action: types.ActionAutoFix}},
		{name: "repeated as the prompt showed it", finding: Finding{File: "selected.go", Line: 90, Description: sanitizePromptText(selectedDefect), Action: types.ActionAutoFix}},
		{name: "repeated in another file", finding: Finding{File: "other.go", Line: 3, Description: selectedDefect, Action: types.ActionAutoFix}},
		{name: "beside a deferred finding, which cannot clear here", finding: withFile(outside, "deferred.go", 90), wantFollowUp: true},
		{name: "a deferred finding reported again is not new", finding: Finding{File: "deferred.go", Line: 7, Description: "left for later", Action: types.ActionAutoFix}},
		{name: "a deferred finding repeated in another file", finding: Finding{File: "other.go", Line: 3, Description: "left for later", Action: types.ActionAutoFix}},
		{name: "in another file while an earlier selection is unverified", finding: outside, unverified: unverified},
		{name: "an earlier selection reported again", finding: Finding{File: "earlier.go", Line: 7, Description: "an earlier selection", Action: types.ActionAutoFix}, unverified: unverified},
		{name: "an earlier selection named closed", finding: outside, closed: []string{"f-1", "f-3"}, unverified: unverified, wantFollowUp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scope := &repairRoundScope{
				changed:   map[string][]lineSpan{"repaired.go": {{10, 20}}, "internal/repaired.go": {{10, 20}}, "binary.dat": nil, "selected.go": {{1, 5}}},
				tracked:   map[string]bool{"other.go": true, "repaired.go": true, "internal/repaired.go": true, "binary.dat": true, "selected.go": true, "first-look.go": true, "deferred.go": true, "earlier.go": true},
				firstLook: []string{"first-look.go"},
				verifying: append([]Finding{{ID: "f-1", File: "selected.go", Line: 40, Description: selectedDefect}}, tc.unverified...),
				deferred:  []Finding{{ID: "f-2", File: "deferred.go", Line: 7, Description: "left for later"}},
			}
			closed := tc.closed
			if closed == nil {
				closed = []string{"f-1"}
			}
			findings := Findings{Items: []Finding{tc.finding}, ClosedFindings: closed}

			scope.split(func(string) {}, &findings)

			if got := len(findings.FollowUps) == 1; got != tc.wantFollowUp {
				t.Fatalf("recorded as a follow-up = %v, want %v (findings %+v)", got, tc.wantFollowUp, findings)
			}
			if got := len(findings.Items) == 1; got == tc.wantFollowUp {
				t.Fatalf("still blocking = %v, want %v (findings %+v)", got, !tc.wantFollowUp, findings)
			}
		})
	}
}

func TestRepairRoundScopeSplitRestatesTheSummaryOfWhatItMoved(t *testing.T) {
	t.Parallel()
	scope := &repairRoundScope{tracked: map[string]bool{"a.go": true}}
	moved := Finding{File: "a.go", Line: 3, Description: "d", Action: types.ActionAutoFix}
	blocked := Finding{Description: "no anchor", Action: types.ActionAutoFix}
	cases := []struct {
		name  string
		items []Finding
		want  string
	}{
		{"everything moved", []Finding{moved}, "no review findings remain"},
		{"something still blocks", []Finding{moved, blocked}, "1 review finding remains"},
		{"nothing moved keeps the reviewer's words", []Finding{blocked}, "1 error: must not merge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := Findings{Items: tc.items, Summary: "1 error: must not merge"}

			scope.split(func(string) {}, &findings)

			if findings.Summary != tc.want {
				t.Fatalf("summary = %q, want %q", findings.Summary, tc.want)
			}
		})
	}
}

func TestRepairRoundScopeRecordsOnlySelectedFindingsAsClosed(t *testing.T) {
	t.Parallel()
	scope := &repairRoundScope{
		verifying: []Finding{{ID: "f-1", File: "a.go", Line: 1}},
		deferred:  []Finding{{ID: "f-2", File: "b.go", Line: 1}},
	}
	findings := Findings{ClosedFindings: []string{"f-1", "f-2", "made-up"}}

	scope.split(func(string) {}, &findings)

	if !reflect.DeepEqual(findings.ClosedFindings, []string{"f-1"}) {
		t.Fatalf("closed findings = %v, want only the id awaiting verification", findings.ClosedFindings)
	}
}

func TestNewRepairRoundScopeSortsOpenFindingsByWhatCanClear(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	selected := Findings{Items: []Finding{
		{ID: "f-1", File: "a.go", Line: 1, Description: "sent to this repair"},
		{ID: "question-q1", File: "a.go", Line: 1, Description: "a selected question", Category: types.FindingCategoryReviewQuestion},
	}}
	deferred := []Finding{
		{ID: "f-2", File: "b.go", Line: 1, Description: "sent to an earlier repair"},
		{ID: "note", File: "c.go", Line: 1, Description: "never selected"},
		{ID: "question-q2", File: "d.go", Line: 1, Description: "a deferred question", Category: types.FindingCategoryReviewQuestion},
	}

	scope, err := newRepairRoundScope(context.Background(), dir, head, nil, nil, []string{"a.go"}, selected, deferred, []string{"f-1", "f-2", "question-q2"})
	if err != nil {
		t.Fatal(err)
	}

	section := scope.promptSection()
	for _, want := range []string{"f-1: a.go:1: sent to this repair", "f-2: b.go:1: sent to an earlier repair"} {
		if !strings.Contains(section, want) {
			t.Errorf("the checklist is missing %q:\n%s", want, section)
		}
	}
	for _, unwanted := range []string{"never selected", "a selected question", "a deferred question"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("the checklist must not list %q:\n%s", unwanted, section)
		}
	}
	if len(scope.deferred) != 1 || scope.deferred[0].ID != "note" {
		t.Fatalf("deferred = %+v, want only the finding that cannot clear", scope.deferred)
	}
}

func TestNewRepairRoundScopeReadsTheRepairFromTheWorktree(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	const bracketed = "[x].txt"
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("long.txt", numberedLines(40))
	writeFile(bracketed, numberedLines(40))
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add long files")
	startHead := gitCmd(t, dir, "rev-parse", "HEAD")
	writeFile("long.txt", strings.Replace(numberedLines(40), "line 20\n", "line twenty\n", 1))
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "the repair's commit")
	writeFile(bracketed, strings.Replace(numberedLines(40), "line 30\n", "line thirty\n", 1))

	scope, err := newRepairRoundScope(context.Background(), dir, startHead, []string{"long.txt", bracketed}, []string{"long.txt", bracketed}, []string{"long.txt", bracketed}, Findings{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		file    string
		line    int
		outside bool
	}{
		{"long.txt", 20, false},
		{"long.txt", 17, false},
		{"long.txt", 16, true},
		{"long.txt", 24, true},
		{bracketed, 30, false},
		{bracketed, 10, true},
		{"./long.txt", 30, true},
		{filepath.ToSlash(filepath.Join(dir, "long.txt")), 30, false},
		{"absent.txt", 30, false},
	}
	for _, tc := range cases {
		if got := scope.isFollowUp(Finding{File: tc.file, Line: tc.line, Description: "d", Action: types.ActionAutoFix}); got != tc.outside {
			t.Errorf("%s:%d outside the repair = %v, want %v (hunks %+v)", tc.file, tc.line, got, tc.outside, scope.changed)
		}
	}
}

func TestRepairReviewSchemaDeclaresTheRepairOutputOnlyOnARepairRound(t *testing.T) {
	t.Parallel()
	type schema struct {
		Properties struct {
			ClosedFindings json.RawMessage `json:"closed_findings"`
			Findings       struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
					Required   []string                   `json:"required"`
				} `json:"items"`
			} `json:"findings"`
		} `json:"properties"`
	}
	var repair, base schema
	if err := json.Unmarshal(repairReviewSchema, &repair); err != nil {
		t.Fatalf("repair schema is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(reviewFindingsSchema, &base); err != nil {
		t.Fatal(err)
	}

	if repair.Properties.ClosedFindings == nil {
		t.Fatal("repair schema does not declare closed_findings")
	}
	for _, field := range []string{"failing_input", "wrong_result"} {
		if repair.Properties.Findings.Items.Properties[field] == nil {
			t.Errorf("repair schema does not declare %s", field)
		}
		if base.Properties.Findings.Items.Properties[field] != nil {
			t.Errorf("the ordinary review schema declares %s", field)
		}
	}
	if base.Properties.ClosedFindings != nil {
		t.Error("the ordinary review schema declares closed_findings")
	}
	if !reflect.DeepEqual(repair.Properties.Findings.Items.Required, base.Properties.Findings.Items.Required) {
		t.Errorf("repair schema changed the required fields: %v", repair.Properties.Findings.Items.Required)
	}
}

func TestParseReviewAnalyzerOutputRefusesAFollowUpFromTheAgent(t *testing.T) {
	t.Parallel()
	output := fmt.Sprintf(`{"findings":[],"follow_ups":[{"severity":"error","description":"hidden","action":"auto-fix"}],%s}`, cleanRereview)

	findings, err := parseReviewAnalyzerOutput(&agent.Result{Output: []byte(output)})

	if err != nil {
		t.Fatal(err)
	}
	if len(findings.FollowUps) != 0 {
		t.Fatalf("follow-ups = %+v, want none: the step alone records them", findings.FollowUps)
	}
}

func TestSanitizedPreviousFindingsKeepTheRepairRecordFromTheFixer(t *testing.T) {
	t.Parallel()
	raw, err := types.MarshalFindingsJSON(Findings{
		Items: []Finding{{
			ID: "f-1", Severity: "error", Description: "d", Action: "auto-fix",
			FailingInput: "<<<<<<< HEAD an input", WrongResult: "<<<<<<< HEAD a result",
		}},
		FollowUps:      []Finding{{Severity: "warning", Description: "a thought for later", Action: "auto-fix"}},
		ClosedFindings: []string{"f-9"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := sanitizedPreviousFindingsForPrompt(raw)

	if strings.Contains(got, "a thought for later") || strings.Contains(got, "f-9") {
		t.Fatalf("the fixer was handed the record of another round: %s", got)
	}
	if strings.Contains(got, "<<<<<<<") {
		t.Fatalf("a concrete failure reached the prompt unsanitized: %s", got)
	}
	if !strings.Contains(got, "an input") || !strings.Contains(got, "a result") {
		t.Fatalf("the fixer lost the concrete failure: %s", got)
	}
}

func numberedLines(count int) string {
	var b strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// longFeatureHarness is fixRoundScopeHarness over a feature.txt long enough
// that a repair of its first line leaves most of the file outside the repair.
func longFeatureHarness(t *testing.T, mock *sessionMockAgent) (*pipeline.Executor, *db.DB, *db.Run, *db.Repo, string) {
	t.Helper()
	workDir, baseSHA, _ := twoFileRepo(t)
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte(numberedLines(40)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "-A")
	gitCmd(t, workDir, "commit", "-m", "lengthen the feature")
	return fixRoundScopeHarnessOn(t, mock, workDir, baseSHA, gitCmd(t, workDir, "rev-parse", "HEAD"))
}

func repairFirstLine(t *testing.T, opts agent.RunOpts) *agent.Result {
	t.Helper()
	repaired := strings.Replace(numberedLines(40), "line 1\n", "line one\n", 1)
	if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte(repaired), 0o644); err != nil {
		t.Errorf("write fix: %v", err)
	}
	return &agent.Result{Output: []byte(`{"summary":"fix the first line"}`)}
}

const firstRoundOutput = `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"selected defect","action":"auto-fix"}],"summary":"1","risk_level":"high","risk_rationale":"it can lose data","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`

// scriptedRepairRound answers the first review with firstRoundOutput, repairs
// the first line of feature.txt, and answers every later review with rereview.
func scriptedRepairRound(t *testing.T, rereview string) *sessionMockAgent {
	t.Helper()
	reviewTurn := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			if reviewTurn == 1 {
				return &agent.Result{Output: []byte(firstRoundOutput)}
			}
			return &agent.Result{Output: []byte(rereview)}
		case "review-fix":
			return repairFirstLine(t, opts)
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	return mock
}

// reviewParkedOn runs the executor until the review parks and returns the
// findings the gate shows. A run that completes instead fails the test.
func reviewParkedOn(t *testing.T, exec *pipeline.Executor, database *db.DB, run *db.Run, repo *db.Repo, workDir string) types.Findings {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(run.ID)
		if err == nil && len(steps) == 1 && (steps[0].Status == types.StepStatusFixReview || steps[0].Status == types.StepStatusAwaitingApproval) {
			cancel()
			<-done
			if steps[0].FindingsJSON == nil {
				t.Fatal("the review parked with no findings")
			}
			findings, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
			if err != nil {
				t.Fatalf("parse parked findings: %v", err)
			}
			return findings
		}
		select {
		case err := <-done:
			t.Fatalf("the review completed instead of parking: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the review did not park")
	return types.Findings{}
}

// lastReviewRound returns the review step's rows and its last round's record.
func lastReviewRound(t *testing.T, database *db.DB, run *db.Run) ([]*db.StepResult, []*db.StepRound, types.Findings) {
	t.Helper()
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("review step = %+v, want completed", steps)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) < 2 || rounds[len(rounds)-1].FindingsJSON == nil {
		t.Fatalf("rounds = %+v, want a recorded rereview", rounds)
	}
	record, err := types.ParseFindingsJSON(*rounds[len(rounds)-1].FindingsJSON)
	if err != nil {
		t.Fatalf("parse the rereview's record: %v", err)
	}
	return steps, rounds, record
}

func TestReviewLoop_ARepairRoundIsGivenOnlyTheRepairsChange(t *testing.T) {
	mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[],"closed_findings":["f-1"],"reviewed_paths":["feature.txt"],%s}`, cleanRereview))
	exec, _, run, repo, workDir := longFeatureHarness(t, mock)
	startHead := run.HeadSHA

	executeWithoutAPark(t, exec, run, repo, workDir)

	reviews := reviewCalls(mock.snapshot())
	if len(reviews) != 2 {
		t.Fatalf("review turns = %d, want the review and one rereview", len(reviews))
	}
	rereview := reviews[1].Prompt
	for _, want := range []string{
		"- review scope: only this repair's change since starting head " + startHead + "\n",
		repairScopeMarker,
		"f-1: feature.txt:1: selected defect",
		"closed_findings",
		"failing_input",
		"wrong_result",
		"still describe the whole branch",
		`set risk_level to "high"`,
	} {
		if !strings.Contains(rereview, want) {
			t.Errorf("the rereview prompt is missing %q:\n%s", want, rereview)
		}
	}
	if strings.Contains(rereview, "- review scope: the branch changes") || strings.Contains(rereview, "relative to base commit "+run.BaseSHA) {
		t.Errorf("the rereview was sent to the whole branch change:\n%s", rereview)
	}
	if !strings.Contains(string(reviews[1].JSONSchema), "closed_findings") {
		t.Error("the rereview's schema has no closed_findings")
	}
	if strings.Contains(reviews[0].Prompt, repairScopeMarker) || strings.Contains(string(reviews[0].JSONSchema), "closed_findings") {
		t.Errorf("the first review must judge the whole branch with the ordinary contract:\n%s", reviews[0].Prompt)
	}
}

func TestReviewLoop_AFileNoEarlierRoundJudgedIsReadInFull(t *testing.T) {
	reviewTurn := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			if reviewTurn == 1 {
				return &agent.Result{Output: []byte(strings.Replace(firstRoundOutput, `["feature.txt","feature2.txt"]`, `["feature.txt"]`, 1))}
			}
			return &agent.Result{Output: []byte(fmt.Sprintf(`{"findings":[{"id":"n-1","severity":"warning","file":"feature2.txt","line":1,"description":"first look finds a defect","action":"ask-user"}],"closed_findings":["f-1"],"reviewed_paths":["feature.txt","feature2.txt"],%s}`, cleanRereview))}
		case "review-fix":
			return repairFirstLine(t, opts)
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	exec, database, run, repo, workDir := longFeatureHarness(t, mock)

	parked := reviewParkedOn(t, exec, database, run, repo, workDir)

	rereview := reviewCalls(mock.snapshot())[1].Prompt
	if !strings.Contains(rereview, "plus the branch changes relative to base commit "+run.BaseSHA) || !strings.Contains(rereview, "  - feature2.txt\n") {
		t.Fatalf("the rereview must read the unjudged file's whole change:\n%s", rereview)
	}
	if len(parked.Items) != 1 || parked.Items[0].Description != "first look finds a defect" || len(parked.FollowUps) != 0 {
		t.Fatalf("parked findings = %+v, want the first look's finding blocking", parked)
	}
}

func TestReviewLoop_AFindingOutsideTheRepairIsRecordedWithoutParking(t *testing.T) {
	mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[{"id":"n-1","severity":"error","file":"feature2.txt","line":1,"description":"a thought for later","action":"auto-fix"}],"closed_findings":["f-1"],"reviewed_paths":["feature.txt"],%s}`, cleanRereview))
	exec, database, run, repo, workDir := longFeatureHarness(t, mock)

	executeWithoutAPark(t, exec, run, repo, workDir)

	if got := len(fixCalls(mock.snapshot())); got != 1 {
		t.Fatalf("fix turns = %d, want 1: a follow-up must not spend a round", got)
	}
	steps, rounds, record := lastReviewRound(t, database, run)
	if len(record.Items) != 0 {
		t.Fatalf("rereview items = %+v, want none outstanding", record.Items)
	}
	if len(record.FollowUps) != 1 || record.FollowUps[0].Description != "a thought for later" {
		t.Fatalf("follow-ups = %+v, want the finding on the record", record.FollowUps)
	}
	if !reflect.DeepEqual(record.ClosedFindings, []string{"f-1"}) {
		t.Fatalf("closed findings = %v, want the selected finding", record.ClosedFindings)
	}
	summary, _ := BuildPipelineSummary(steps, map[string][]*db.StepRound{steps[0].ID: rounds}, testPipelineHeadSHA)
	if !strings.Contains(summary, "a thought for later") || !strings.Contains(summary, "feature2.txt:1") {
		t.Fatalf("the PR summary does not list the follow-up:\n%s", summary)
	}
}

func TestReviewLoop_AFindingNamingAConcreteFailureOutsideTheRepairStillBlocks(t *testing.T) {
	mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[{"id":"n-1","severity":"error","file":"feature2.txt","line":1,"description":"a real defect","action":"auto-fix","failing_input":"an empty name","wrong_result":"the record is saved with no owner"}],"closed_findings":["f-1"],"reviewed_paths":["feature.txt"],%s}`, cleanRereview))
	exec, database, run, repo, workDir := longFeatureHarness(t, mock)

	parked := reviewParkedOn(t, exec, database, run, repo, workDir)

	if len(parked.Items) != 1 || parked.Items[0].Description != "a real defect" {
		t.Fatalf("parked items = %+v, want the concrete finding blocking", parked.Items)
	}
	if len(parked.FollowUps) != 0 {
		t.Fatalf("follow-ups = %+v, want none", parked.FollowUps)
	}
}

// The clearing rule refuses to clear a selected finding beside any other
// finding in its file, because that finding may be the selected defect moved
// or reworded. Only the reviewer's closed verdict settles that doubt.
func TestReviewLoop_AFindingBesideASelectedOneNeedsAClosedVerdict(t *testing.T) {
	sameFile := `{"id":"n-1","severity":"warning","file":"feature.txt","line":30,"description":"a line the repair never touched","action":"auto-fix"}`

	t.Run("named closed, the selected finding clears and the other is a follow-up", func(t *testing.T) {
		mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[%s],"closed_findings":["f-1"],"reviewed_paths":["feature.txt"],%s}`, sameFile, cleanRereview))
		exec, database, run, repo, workDir := longFeatureHarness(t, mock)

		executeWithoutAPark(t, exec, run, repo, workDir)

		_, _, record := lastReviewRound(t, database, run)
		if len(record.Items) != 0 || len(record.FollowUps) != 1 {
			t.Fatalf("record = %+v, want the selected finding cleared and one follow-up", record)
		}
	})

	t.Run("not named closed, both stay and the run parks", func(t *testing.T) {
		mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[%s],"reviewed_paths":["feature.txt"],%s}`, sameFile, cleanRereview))
		exec, database, run, repo, workDir := longFeatureHarness(t, mock)

		parked := reviewParkedOn(t, exec, database, run, repo, workDir)

		if len(parked.FollowUps) != 0 {
			t.Fatalf("follow-ups = %+v, want none: this may be the selected defect moved", parked.FollowUps)
		}
		descriptions := make([]string, 0, len(parked.Items))
		for _, item := range parked.Items {
			descriptions = append(descriptions, item.Description)
		}
		if got := strings.Join(descriptions, ","); !strings.Contains(got, "selected defect") || !strings.Contains(got, "a line the repair never touched") {
			t.Fatalf("parked items = %q, want both still outstanding", got)
		}
	})
}

// A question is resolved by its answer. The checklist tells the rereview to
// report an open finding again, and a copy of a question's row has no category,
// so the answer would leave the gate parked on it.
func TestReviewLoop_AnOpenQuestionIsLeftToItsAnswer(t *testing.T) {
	const questionRow = "  - question-q1: feature.txt:1: "
	var conversationDir string
	reviewTurn := 0
	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		switch opts.Purpose {
		case "review":
			reviewTurn++
			if reviewTurn == 1 {
				if err := appendAgentQuestionLine(conversationDir, `{"id":"q1","kind":"question","question":"Should the legacy route keep answering?","options":["Keep","Remove"],"weight":"major","file":"feature.txt","line":1}`); err != nil {
					t.Errorf("append question: %v", err)
				}
				return &agent.Result{Output: []byte(fmt.Sprintf(`{"findings":[{"id":"f-1","severity":"error","file":"feature2.txt","line":1,"description":"selected defect","action":"auto-fix"}],"reviewed_paths":["feature.txt","feature2.txt"],%s}`, cleanRereview))}
			}
			findings := "[]"
			if _, listed, isListed := strings.Cut(opts.Prompt, questionRow); isListed {
				description, _, _ := strings.Cut(listed, "\n")
				row, err := json.Marshal([]map[string]any{{"id": "question-q1", "severity": "warning", "file": "feature.txt", "line": 1, "description": description, "action": "ask-user"}})
				if err != nil {
					t.Errorf("marshal the copied row: %v", err)
				}
				findings = string(row)
			}
			return &agent.Result{Output: []byte(fmt.Sprintf(`{"findings":%s,"closed_findings":["f-1"],"reviewed_paths":["feature.txt","feature2.txt"],%s}`, findings, cleanRereview))}
		case "review-fix":
			if err := os.WriteFile(filepath.Join(opts.CWD, "feature2.txt"), []byte("fixed two\n"), 0o644); err != nil {
				t.Errorf("write fix: %v", err)
			}
			return &agent.Result{Output: []byte(`{"summary":"fix"}`)}
		default:
			t.Errorf("unexpected agent purpose %q", opts.Purpose)
			return &agent.Result{Output: []byte(`{}`)}
		}
	}
	workDir, baseSHA, headSHA := twoFileRepo(t)
	ensureHermeticOrigin(t, workDir)
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Agent: types.AgentClaude, AutoFix: config.AutoFix{Review: 3}}
	enableReviewConversation(cfg)
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, mock, []pipeline.Step{&ReviewStep{}}, nil)
	conversationDir = exec.ReviewConversationDir(run.ID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		steps, err := database.GetStepsByRun(run.ID)
		if err == nil && len(steps) == 1 && (steps[0].Status == types.StepStatusFixReview || steps[0].Status == types.StepStatusAwaitingApproval) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the review did not park on the open question")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := reviewqa.AppendAnswer(conversationDir, reviewqa.Answer{ID: "q1", Answer: "Keep", AnsweredBy: "captain", AskOrdinal: 1}); err != nil {
		t.Fatalf("append answer: %v", err)
	}
	// The gate's own resumer may take the answer first, as it does in a real run.
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil); err != nil && !strings.Contains(err.Error(), "no step awaiting approval") {
		t.Fatalf("respond with the answer: %v", err)
	}

	deadline = time.Now().Add(30 * time.Second)
	for {
		steps, err := database.GetStepsByRun(run.ID)
		if err == nil && len(steps) == 1 && steps[0].Status == types.StepStatusCompleted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer did not end the wait: steps = %+v", steps)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewLoop_ADeferredNoteHoldsNoFollowUpBack(t *testing.T) {
	const firstRoundWithANote = `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"selected defect","action":"auto-fix"},{"id":"note","severity":"info","file":"feature2.txt","line":1,"description":"worth knowing","action":"no-op"}],"summary":"bug","risk_level":"high","risk_rationale":"it can lose data","risk_scope":"source-or-external","reviewed_paths":["feature.txt","feature2.txt"]}`
	mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[{"id":"n-1","severity":"error","file":"feature.txt","line":30,"description":"a thought for later","action":"auto-fix"}],"closed_findings":["f-1"],"reviewed_paths":["feature.txt","feature2.txt"],%s}`, cleanRereview))
	answerReview := mock.respond
	isFirstReview := true
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		result := answerReview(opts)
		if opts.Purpose == "review" && isFirstReview {
			isFirstReview = false
			return &agent.Result{Output: []byte(firstRoundWithANote)}
		}
		return result
	}
	exec, database, run, repo, workDir := longFeatureHarness(t, mock)

	executeWithoutAPark(t, exec, run, repo, workDir)

	if got := len(fixCalls(mock.snapshot())); got != 1 {
		t.Fatalf("fix turns = %d, want 1: a note must not turn a follow-up into another round", got)
	}
	_, _, record := lastReviewRound(t, database, run)
	if len(record.FollowUps) != 1 || record.FollowUps[0].Description != "a thought for later" {
		t.Fatalf("follow-ups = %+v, want the finding on the record", record.FollowUps)
	}
}

func TestReviewLoop_ACoverageCompletionFollowUpDoesNotPark(t *testing.T) {
	mock := scriptedRepairRound(t, fmt.Sprintf(`{"findings":[],"closed_findings":["f-1"],"reviewed_paths":[],%s}`, cleanRereview))
	answerReview := mock.respond
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		if opts.Purpose == "review-coverage" {
			return &agent.Result{Output: []byte(fmt.Sprintf(`{"findings":[{"id":"n-1","severity":"error","file":"feature2.txt","line":1,"description":"found while completing coverage","action":"auto-fix"}],"reviewed_paths":["feature.txt"],%s}`, cleanRereview))}
		}
		return answerReview(opts)
	}
	exec, database, run, repo, workDir := longFeatureHarness(t, mock)

	executeWithoutAPark(t, exec, run, repo, workDir)

	_, _, record := lastReviewRound(t, database, run)
	if len(record.Items) != 0 || len(record.FollowUps) != 1 {
		t.Fatalf("record = %+v, want the completion turn's finding recorded as a follow-up", record)
	}
	for _, call := range mock.snapshot() {
		if call.Purpose == "review-coverage" && (strings.Contains(call.Prompt, "change in the branch diff") || !strings.Contains(call.Prompt, "the repair round scope above")) {
			t.Fatalf("the completion turn must keep to the repair's scope:\n%s", call.Prompt)
		}
	}
}
