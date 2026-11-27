package db

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A fix response selecting only the repair-agent-budget notice keeps what the
// cut turn left and rechecks it in a fix round that, by design, records no
// selection on the round it answers (#418). The family ledger must count that
// operator decision rather than refuse every authored successor of the run.
func TestValidationFamilyCountsARepairCutRecheckAsManualSpending(t *testing.T) {
	for _, test := range []struct {
		name               string
		cutRoundFindings   string
		shouldAdmit        bool
		wantManualSpending int
	}{
		{
			name:               "recheck of a repair cut at its agent budget",
			cutRoundFindings:   `{"findings":[{"id":"F1","action":"auto-fix","description":"repair"},{"id":"repair-agent-budget","severity":"warning","action":"ask-user","description":"cut"}]}`,
			shouldAdmit:        true,
			wantManualSpending: 2,
		},
		{
			name:             "unselected fix round after an ordinary round",
			cutRoundFindings: `{"findings":[{"id":"F1","action":"auto-fix","description":"repair"}]}`,
			shouldAdmit:      false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: the issue's Review ledger - a user-selected initial round,
			// the fix round it started, and a second fix round with no selection
			// recorded on the round before it, which the operator then approved.
			database, proposal := admissionFixture(t)
			step := proposal.Snapshot.Steps[0]
			if _, err := database.sql.Exec(`DELETE FROM step_rounds WHERE step_result_id = ?`, step.ID); err != nil {
				t.Fatal(err)
			}
			initialFindings, selected := `{"findings":[{"id":"F1","action":"auto-fix","description":"repair"}]}`, `["F1"]`
			initial, err := database.InsertStepRound(step.ID, 1, "initial", &initialFindings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepRoundSelection(initial.ID, &selected, RoundSelectionSourceUser); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(step.ID, 2, "auto_fix", &test.cutRoundFindings, nil, 1); err != nil {
				t.Fatal(err)
			}
			recheck, err := database.InsertStepRound(step.ID, 3, "auto_fix", &initialFindings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepRoundDeclined(recheck.ID); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(proposal.Request.PredecessorID)
			if err != nil {
				t.Fatal(err)
			}
			proposal.Snapshot, proposal.Request.SnapshotDigest = *snapshot, snapshot.Digest()

			// Act
			event, _, err := database.AcceptValidationAdmission(context.Background(), proposal)

			// Assert
			if !test.shouldAdmit {
				if err == nil || !strings.Contains(err.Error(), "has no provable automatic/manual selection") {
					t.Fatalf("admission of an unprovable fix round = %v, want the provable-selection refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("authored admission refused: %v", err)
			}
			family, err := database.ValidationFamily(event.ChildID)
			if err != nil {
				t.Fatal(err)
			}
			if spending := family.Spending[types.StepReview]; spending.Automatic != 0 || spending.Manual != test.wantManualSpending {
				t.Fatalf("family spending = %+v, want %d manual and no automatic", spending, test.wantManualSpending)
			}
		})
	}
}
