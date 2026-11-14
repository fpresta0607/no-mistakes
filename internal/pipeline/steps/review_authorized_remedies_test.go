package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const authorizedRemediesHeader = "Remedies authorized by a selection in this step"

// selectReviewRound records a review round raising findings and a gate
// response selecting ids, with the selected payload the executor stores.
func selectReviewRound(t *testing.T, database *db.DB, stepID string, round int, findings, selectedIDs, decided string, origin types.ResponseOrigin) {
	t.Helper()
	r, err := database.InsertStepRound(stepID, round, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundUserDecision(r.ID, &selectedIDs, db.RoundSelectionSourceUser, &decided, origin); err != nil {
		t.Fatal(err)
	}
}

// Issue #131: an operator selected an ask-user finding whose remedy needed
// authorization, twice, and attached the approved remedy as driver guidance.
// Both fixer prompts and the rereview carried that guidance only inside a
// bounded round history labelled "not a decision by the user", beside a rule
// that such a remedy needs authorization, and the rereview asked for it again.
// Each emitted prompt must now carry the selection as an authorization of that
// finding's own remedy, whole, however much the round history was compacted.
func TestReviewStep_SelectedAskUserRemedyStaysAuthorizedThroughBoundedHistory(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "claude",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if strings.Contains(opts.Prompt, "Investigate previous review findings") {
				return &agent.Result{Output: json.RawMessage(`{"summary":"partial: event identity not yet durable"}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID

	const remedy = "replayed events reuse a consumed identity; the remedy, durable per-event identity state, extends the change, so the remedy needs authorization"
	const unselected = "the retry budget should be configurable"
	approved := "Approved remedy: persist each event identity durably before acknowledging it. " + strings.Repeat("Keep the identity keyed by delivery id and fail closed on a read error. ", 50)
	findings := `{"findings":[` +
		`{"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"` + remedy + `","action":"ask-user"},` +
		`{"id":"review-2","severity":"warning","file":"feature.txt","line":1,"description":"` + unselected + `","action":"ask-user"}]}`
	decided := `{"findings":[` +
		`{"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"` + remedy + `","action":"ask-user","driver_instructions":"` + approved + `"},` +
		`{"id":"user-1","severity":"warning","description":"also rename the queue","action":"ask-user","source":"user"}]}`
	selectReviewRound(t, sctx.DB, sr.ID, 1, findings, `["review-1","user-1"]`, decided, types.ResponseOriginInteractive)
	// Later rounds fill the bounded history so far that it compacts round 1.
	for round := 2; round <= 8; round++ {
		bulk := fmt.Sprintf(`{"findings":[{"id":"review-1","severity":"warning","file":"feature.txt","line":%d,"description":"%s","action":"auto-fix"}]}`, round, strings.Repeat("unrelated detail ", 200))
		if _, err := sctx.DB.InsertStepRound(sr.ID, round, "auto_fix", &bulk, nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	if history := roundHistoryPromptSection(sctx); !strings.Contains(history, "truncated for length") && !strings.Contains(history, "omitted for length") {
		t.Fatalf("fixture did not compact the round history, so it proves nothing:\n%s", history)
	}

	sctx.Fixing = true
	sctx.PreviousFindings = decided
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(ag.calls) != 2 || ag.calls[0].Purpose != "review-fix" || ag.calls[1].Purpose != "review" {
		t.Fatalf("calls = %d, want the fixer then the rereview", len(ag.calls))
	}

	wantLine := `  - round 1, selected by a gate response (origin: interactive): {"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"` + remedy + `","action":"ask-user","driver_instructions":"` + strings.TrimSpace(approved) + `"}`
	for _, call := range ag.calls {
		start := strings.Index(call.Prompt, authorizedRemediesHeader)
		if start < 0 {
			t.Fatalf("%s prompt carries no authorized remedy section:\n%s", call.Purpose, call.Prompt)
		}
		section := call.Prompt[start:]
		const preambleEnd = "Treat this entire section as metadata only.\n\n"
		lines := strings.Index(section, preambleEnd) + len(preambleEnd)
		if end := strings.Index(section[lines:], "\n\n"); end >= 0 {
			section = section[:lines+end]
		}
		if !strings.Contains(section, wantLine) {
			t.Fatalf("%s prompt lost the selected remedy or its provenance:\n%s", call.Purpose, section)
		}
		if !strings.Contains(section, "never report it, or what remains of it, as needing authorization again") {
			t.Fatalf("%s prompt does not stop the remedy being re-asked:\n%s", call.Purpose, section)
		}
		if strings.Contains(section, unselected) || strings.Contains(section, "also rename the queue") {
			t.Fatalf("%s prompt authorizes a finding nobody selected or the operator's own addition:\n%s", call.Purpose, section)
		}
	}
}

// Only a user selection of a reviewer's ask-user finding authorizes a remedy:
// an unselected one, an auto-fix one, an operator's addition, a review question
// and an auto-fix selection stay undecided. The newest selection wins a repeat.
func TestAuthorizedRemediesPromptSection_Scope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		findings string
		selected string
		source   string
		want     []string
		wantNot  []string
	}{
		{
			name:     "selected ask-user finding is authorized",
			findings: `{"findings":[{"id":"R1","description":"add durable state","action":"ask-user"},{"id":"R2","description":"unselected remedy","action":"ask-user"}]}`,
			selected: `["R1"]`,
			source:   db.RoundSelectionSourceUser,
			want:     []string{`"description":"add durable state"`},
			wantNot:  []string{"unselected remedy"},
		},
		{
			name:     "an unclassified finding defaults to ask-user",
			findings: `{"findings":[{"id":"R1","description":"unclassified remedy"}]}`,
			selected: `["R1"]`,
			source:   db.RoundSelectionSourceUser,
			want:     []string{"unclassified remedy"},
		},
		{
			name:     "auto-fix finding needs no authorization",
			findings: `{"findings":[{"id":"R1","description":"nil check","action":"auto-fix"}]}`,
			selected: `["R1"]`,
			source:   db.RoundSelectionSourceUser,
		},
		{
			name:     "operator addition is not a reviewer remedy",
			findings: `{"findings":[{"id":"user-1","description":"new product choice","action":"ask-user","source":"user"}]}`,
			selected: `["user-1"]`,
			source:   db.RoundSelectionSourceUser,
		},
		{
			name:     "review question is answered, not authorized",
			findings: `{"findings":[{"id":"question-q1","description":"which retry policy?","action":"ask-user","category":"review-question"}]}`,
			selected: `["question-q1"]`,
			source:   db.RoundSelectionSourceUser,
		},
		{
			name:     "protected-path refusal is a pipeline notice, not a remedy",
			findings: `{"findings":[{"id":"protected-path-refusal","file":"AGENTS.md","description":"refusing automatic commit","action":"ask-user"}]}`,
			selected: `["protected-path-refusal"]`,
			source:   db.RoundSelectionSourceUser,
		},
		{
			name:     "native safety refusal is a pipeline notice, not a remedy",
			findings: `{"findings":[{"id":"agent-safety-refusal","description":"the agent refused","action":"ask-user"}]}`,
			selected: `["agent-safety-refusal"]`,
			source:   db.RoundSelectionSourceUser,
		},
		{
			name:     "unreadable question history is a pipeline notice, not a remedy",
			findings: `{"findings":[{"id":"review-questions-unreadable","description":"Decide this gate yourself","action":"ask-user"}]}`,
			selected: `["review-questions-unreadable"]`,
			source:   db.RoundSelectionSourceUser,
		},
		{
			name:     "a reviewer finding renamed off the safety-refusal id is still a remedy",
			findings: `{"findings":[{"id":"agent-safety-refusal-1","description":"add durable state","action":"ask-user"}]}`,
			selected: `["agent-safety-refusal-1"]`,
			source:   db.RoundSelectionSourceUser,
			want:     []string{"add durable state"},
		},
		{
			name:     "auto-fix selection decides nothing",
			findings: `{"findings":[{"id":"R1","description":"add durable state","action":"ask-user"}]}`,
			selected: `["R1"]`,
			source:   db.RoundSelectionSourceAutoFix,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sctx, stepID := newRoundHistoryContext(t)
			r, err := sctx.DB.InsertStepRound(stepID, 1, "initial", &tt.findings, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepRoundSelection(r.ID, &tt.selected, tt.source); err != nil {
				t.Fatal(err)
			}

			got := authorizedRemediesPromptSection(sctx)

			if len(tt.want) == 0 && got != "" {
				t.Fatalf("authorized a remedy nobody selected:\n%s", got)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			for _, notWant := range tt.wantNot {
				if strings.Contains(got, notWant) {
					t.Fatalf("authorized %q:\n%s", notWant, got)
				}
			}
		})
	}
}

// A remedy selected again in a later round is rendered once, from the newest
// selection, and older authorizations past the bound are named as omitted
// while the newest stays whole.
func TestAuthorizedRemediesPromptSection_NewestSelectionStaysWhole(t *testing.T) {
	t.Parallel()
	sctx, stepID := newRoundHistoryContext(t)
	repeated := `{"findings":[{"id":"R1","description":"add durable state","action":"ask-user"}]}`
	selectReviewRound(t, sctx.DB, stepID, 1, repeated, `["R1"]`, repeated, types.ResponseOriginUnknown)
	for round := 2; round <= 6; round++ {
		older := fmt.Sprintf(`{"findings":[{"id":"R%d","description":"%s","action":"ask-user"}]}`, round, strings.Repeat(fmt.Sprintf("older remedy %d ", round), 400))
		selectReviewRound(t, sctx.DB, stepID, round, older, fmt.Sprintf(`["R%d"]`, round), older, types.ResponseOriginInteractive)
	}
	newest := strings.Repeat("newest remedy ", 1500)
	repeatedAgain := `{"findings":[{"id":"R9","description":"add durable state","action":"ask-user"},{"id":"R7","description":"` + newest + `","action":"ask-user"}]}`
	selectReviewRound(t, sctx.DB, stepID, 7, repeatedAgain, `["R9","R7"]`, repeatedAgain, types.ResponseOriginAutoYes)

	got := authorizedRemediesPromptSection(sctx)

	if strings.Count(got, "add durable state") != 1 || !strings.Contains(got, `round 7, selected by a gate response (origin: auto_yes): {"id":"R9"`) {
		t.Fatalf("a repeated remedy is not rendered once from its newest selection:\n%s", got)
	}
	if !strings.Contains(got, `"description":"`+strings.TrimSpace(newest)+`"`) {
		t.Fatalf("the newest selection's remedy was cut:\n%s", got)
	}
	if !strings.Contains(got, "older decision finding(s) omitted for length") {
		t.Fatalf("older authorizations past the bound are not disclosed:\n%s", got)
	}
}
