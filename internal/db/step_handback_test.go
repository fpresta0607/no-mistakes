package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// parkedStepHandback parks a Test step on its failed initial round and returns
// the receipt a release of that round would record.
func parkedStepHandback(t *testing.T) (*DB, *StepHandback) {
	t.Helper()
	database := openTestDB(t)
	repo, err := database.InsertRepo("fixture", "https://github.com/example/fixture.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "topic", "released", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"failed"}]}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	return database, &StepHandback{RunID: run.ID, StepResultID: step.ID, StepName: types.StepTest, RoundID: round.ID, CallerDir: "caller", ReleasedHead: "released"}
}

func releasedStepHandback(t *testing.T) (*DB, *StepHandback) {
	t.Helper()
	database, receipt := parkedStepHandback(t)
	if err := database.ReleaseStepHandback(receipt); err != nil {
		t.Fatal(err)
	}
	return database, receipt
}

func TestAcceptStepHandbackRequiresTheReleasedGateUnchanged(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(t *testing.T, database *DB, receipt *StepHandback)
	}{
		{"run head moved", func(t *testing.T, database *DB, receipt *StepHandback) {
			if err := database.UpdateRunHeadSHA(receipt.RunID, "elsewhere"); err != nil {
				t.Fatal(err)
			}
		}},
		{"step left its gate", func(t *testing.T, database *DB, receipt *StepHandback) {
			if err := database.UpdateStepStatus(receipt.StepResultID, types.StepStatusRunning); err != nil {
				t.Fatal(err)
			}
		}},
		{"gate parked on a later round", func(t *testing.T, database *DB, receipt *StepHandback) {
			if _, err := database.InsertStepRound(receipt.StepResultID, 2, "auto_fix", nil, nil, 10); err != nil {
				t.Fatal(err)
			}
		}},
		{"validation-only run pinned no repair", pinValidationOnlyAutoFixLimit},
		{"receipt already accepted", func(t *testing.T, database *DB, receipt *StepHandback) {
			if _, err := database.sql.Exec(`UPDATE step_handbacks SET accepted_head = 'earlier' WHERE id = ?`, receipt.ID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			database, receipt := releasedStepHandback(t)
			test.change(t, database, receipt)
			before, err := database.GetRun(receipt.RunID)
			if err != nil {
				t.Fatal(err)
			}

			// Act
			err = database.AcceptStepHandback(receipt, "correction")

			// Assert
			if err == nil {
				t.Fatal("handback accepted a correction over a changed gate")
			}
			after, err := database.GetRun(receipt.RunID)
			if err != nil || after.HeadSHA != before.HeadSHA {
				t.Fatalf("refused acceptance moved the run head from %s to %+v, %v", before.HeadSHA, after, err)
			}
		})
	}
}

func TestReleaseStepHandbackIssuesOneReceiptPerRound(t *testing.T) {
	// Arrange
	database, receipt := releasedStepHandback(t)
	duplicate := *receipt
	duplicate.ID = ""

	// Act
	err := database.ReleaseStepHandback(&duplicate)

	// Assert
	if err == nil {
		t.Fatal("a round was released twice")
	}
	stored, err := database.GetStepHandbackByRound(receipt.RoundID)
	if err != nil || stored == nil || stored.ID != receipt.ID {
		t.Fatalf("round receipt = %+v, %v; want %s", stored, err, receipt.ID)
	}
}

// pinValidationOnlyAutoFixLimit records the zero limit StartStepWithAutoFixLimit
// pins on every step of a validation-only run, instead of the NULL an ordinary
// auto_fix.<step>: 0 stores.
func pinValidationOnlyAutoFixLimit(t *testing.T, database *DB, receipt *StepHandback) {
	t.Helper()
	if _, err := database.sql.Exec(`UPDATE step_results SET auto_fix_limit = 0 WHERE id = ?`, receipt.StepResultID); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseStepHandbackRefusesAValidationOnlyGate(t *testing.T) {
	// Arrange
	database, receipt := parkedStepHandback(t)
	pinValidationOnlyAutoFixLimit(t, database, receipt)

	// Act
	err := database.ReleaseStepHandback(receipt)

	// Assert
	if err == nil {
		t.Fatal("a validation-only gate was released to its contributor")
	}
	if stored, err := database.GetStepHandbackByRound(receipt.RoundID); err != nil || stored != nil {
		t.Fatalf("refused release recorded receipt %+v, %v", stored, err)
	}
}
