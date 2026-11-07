package db

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func seedScopeDecision(t *testing.T, database *DB) types.ReviewScopeDecision {
	t.Helper()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw := declinedFindings
	round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &raw, nil, "head", "head", "trusted", nil, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return types.ReviewScopeDecision{Version: 1, DecisionID: "decision-1", Authority: "controller_assertion", RunID: run.ID, StepResultID: step.ID, RoundID: round.ID, SourceHeadSHA: "head", SourceBaseSHA: "base", TrustedConfigSHA: "trusted", Finding: findings.Items[0], AcceptedScope: "Keep A; do not expand to B."}
}

func TestReviewScopeDecisionRejectsChangedBindings(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*types.ReviewScopeDecision)
	}{
		{"run", func(d *types.ReviewScopeDecision) { d.RunID = "other" }},
		{"step", func(d *types.ReviewScopeDecision) { d.StepResultID = "other" }},
		{"round", func(d *types.ReviewScopeDecision) { d.RoundID = "other" }},
		{"source", func(d *types.ReviewScopeDecision) { d.SourceHeadSHA = "stale" }},
		{"base", func(d *types.ReviewScopeDecision) { d.SourceBaseSHA = "stale" }},
		{"trusted source", func(d *types.ReviewScopeDecision) { d.TrustedConfigSHA = "stale" }},
		{"unrelated finding", func(d *types.ReviewScopeDecision) { d.Finding.ID = "other" }},
		{"reused finding id", func(d *types.ReviewScopeDecision) { d.Finding.Description = "different concern" }},
		{"wrong asserted authority", func(d *types.ReviewScopeDecision) { d.Authority = "interactive" }},
		{"missing asserted authority", func(d *types.ReviewScopeDecision) { d.Authority = "" }},
		{"ordinary guidance", func(d *types.ReviewScopeDecision) { d.Finding.DriverInstructions = "keep A" }},
		{"version", func(d *types.ReviewScopeDecision) { d.Version = 2 }},
		{"missing scope", func(d *types.ReviewScopeDecision) { d.AcceptedScope = " " }},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDB(t)
			decision := seedScopeDecision(t, database)
			runID := decision.RunID
			test.change(&decision)
			if err := database.SetReviewScopeDecision(decision, `["keep-a"]`, declinedFindings, types.ResponseOriginInteractive); err == nil {
				t.Fatal("accepted a scope decision with a changed binding")
			}
			got, err := database.GetReviewScopeDecisions(runID)
			if err != nil || len(got) != 0 {
				t.Fatalf("refused decision changed authority: decisions=%v err=%v", got, err)
			}
		})
	}
}

func TestReviewScopeDecisionIsImmutableAndRunBound(t *testing.T) {
	database := openTestDB(t)
	decision := seedScopeDecision(t, database)
	for range 2 {
		if err := database.SetReviewScopeDecision(decision, `["keep-a"]`, declinedFindings, types.ResponseOriginUnknown); err != nil {
			t.Fatal(err)
		}
	}
	changed := decision
	changed.AcceptedScope = "Expand to B."
	if err := database.SetReviewScopeDecision(changed, `["keep-a"]`, declinedFindings, types.ResponseOriginUnknown); err == nil {
		t.Fatal("changed an immutable accepted scope")
	}
	if _, err := database.sql.Exec(`UPDATE review_scope_decisions SET decision_json = '{}'`); err == nil {
		t.Fatal("database allowed scope history to be rewritten")
	}
	if _, err := database.sql.Exec(`DELETE FROM review_scope_decisions`); err == nil {
		t.Fatal("database allowed scope history to be erased")
	}
	if err := database.UpdateRunHeadSHA(decision.RunID, "repaired-head"); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetReviewScopeDecisions(decision.RunID)
	if err != nil || len(got) != 1 || got[0] != decision {
		t.Fatalf("later source lost the original accepted binding: got=%v err=%v", got, err)
	}
	other := seedScopeDecision(t, database)
	got, err = database.GetReviewScopeDecisions(other.RunID)
	if err != nil || len(got) != 0 {
		t.Fatalf("another run inherited a scope decision: got=%v err=%v", got, err)
	}
	if err := database.SetReviewScopeDecision(other, `["keep-a"]`, declinedFindings, types.ResponseOriginUnknown); err == nil {
		t.Fatal("replayed a stable decision ID against another run")
	}
	rounds, err := database.GetRoundsByStep(decision.StepResultID)
	if err != nil || len(rounds) != 1 || rounds[0].DurationMS != 7 || *rounds[0].FindingsJSON != declinedFindings {
		t.Fatalf("acceptance changed original findings, rounds or cost: rounds=%v err=%v", rounds, err)
	}
}

func TestReviewScopeDecisionRollsBackWhenSelectionCannotPersist(t *testing.T) {
	database := openTestDB(t)
	decision := seedScopeDecision(t, database)
	if _, err := database.sql.Exec(`CREATE TRIGGER deny_scope_selection BEFORE UPDATE OF selected_finding_ids ON step_rounds BEGIN SELECT RAISE(FAIL, 'selection unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.SetReviewScopeDecision(decision, `["keep-a"]`, declinedFindings, types.ResponseOriginUnknown); err == nil {
		t.Fatal("accepted scope without persisting its response selection")
	}
	got, err := database.GetReviewScopeDecisions(decision.RunID)
	if err != nil || len(got) != 0 {
		t.Fatalf("failed response left accepted scope: got=%v err=%v", got, err)
	}
}

func TestReviewScopeDecisionRequiresSelectedFindingAndLatestRound(t *testing.T) {
	database := openTestDB(t)
	decision := seedScopeDecision(t, database)
	for _, selected := range []string{`[]`, `["other"]`, `null`, `not json`} {
		if err := database.SetReviewScopeDecision(decision, selected, declinedFindings, types.ResponseOriginUnknown); err == nil {
			t.Fatalf("accepted scope for unselected finding: %s", selected)
		}
	}
	clean := `{"findings":[]}`
	if _, err := database.InsertReviewStepRound(decision.StepResultID, 2, "auto_fix", &clean, nil, "head", 3); err != nil {
		t.Fatal(err)
	}
	if err := database.SetReviewScopeDecision(decision, `["keep-a"]`, declinedFindings, types.ResponseOriginUnknown); err == nil {
		t.Fatal("accepted a response against a superseded review round")
	}
}

func TestReviewScopeDecisionDoesNotPromoteOldFixGuidance(t *testing.T) {
	database := openTestDB(t)
	decision := seedScopeDecision(t, database)
	selected := `["keep-a"]`
	payload := declinedFindings
	if err := database.SetStepRoundUserDecision(decision.RoundID, &selected, RoundSelectionSourceUser, &payload, types.ResponseOriginInteractive); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetReviewScopeDecisions(decision.RunID)
	if err != nil || len(got) != 0 {
		t.Fatalf("advisory guidance became explicit scope: got=%v err=%v", got, err)
	}
}

func TestReviewScopeDecisionRejectsUnreadableStoredAuthority(t *testing.T) {
	database := openTestDB(t)
	decision := seedScopeDecision(t, database)
	if _, err := database.sql.Exec(`INSERT INTO review_scope_decisions (id, run_id, step_result_id, round_id, decision_json) VALUES (?, ?, ?, ?, ?)`, decision.DecisionID, decision.RunID, decision.StepResultID, decision.RoundID, "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetReviewScopeDecisions(decision.RunID); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable scope did not fail closed: %v", err)
	}
}
