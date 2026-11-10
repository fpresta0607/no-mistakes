package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func seedSideLaneStep(t *testing.T, database *DB) string {
	t.Helper()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	return step.ID
}

func TestSideLaneRecordOutlivesTheConnectionUntilTaken(t *testing.T) {
	for _, test := range []struct {
		name string
		end  func(*DB, string) error
		want *SideLaneRecord
	}{
		{"refused", func(d *DB, id string) error {
			return d.RecordSideLaneRefusal(id, "head", "native agent safety refusal: flagged")
		}, &SideLaneRecord{Head: "head", Refusal: "native agent safety refusal: flagged"}},
		{"never said how it ended", func(*DB, string) error { return nil }, &SideLaneRecord{Head: "head"}},
		{"ended without a refusal", func(d *DB, id string) error { return d.EndSideLane(id, "head") }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.sqlite")
			database, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			stepID := seedSideLaneStep(t, database)
			if err := database.StartSideLane(stepID, "head"); err != nil {
				t.Fatal(err)
			}
			if err := test.end(database, stepID); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			database, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			got, err := database.TakeSideLane(stepID)

			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
				t.Fatalf("TakeSideLane() = %+v, want %+v", got, test.want)
			}
			if again, err := database.TakeSideLane(stepID); err != nil || again != nil {
				t.Fatalf("second TakeSideLane() = %+v, %v; want the record taken once", again, err)
			}
		})
	}
}

// A lane whose earlier record is outstanding cannot be replaced by a new one,
// which would forget that lane's refusal.
func TestStartSideLaneRefusesWhileAnEarlierRecordIsOutstanding(t *testing.T) {
	database := openTestDB(t)
	stepID := seedSideLaneStep(t, database)
	if err := database.StartSideLane(stepID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordSideLaneRefusal(stepID, "first", "refused"); err != nil {
		t.Fatal(err)
	}

	err := database.StartSideLane(stepID, "second")

	if err == nil {
		t.Fatal("StartSideLane() replaced an outstanding refusal")
	}
	if got, _ := database.TakeSideLane(stepID); got == nil || got.Head != "first" || got.Refusal != "refused" {
		t.Fatalf("record = %+v, want the first lane's refusal", got)
	}
	if err := database.StartSideLane(stepID, "second"); err != nil {
		t.Fatalf("StartSideLane() after the record was taken = %v", err)
	}
}

// A refusal that cannot be written leaves the lane recorded as never having
// said how it ended, which its step treats as a possible refusal.
func TestSideLaneRefusalThatCannotBeRecordedStaysOutstanding(t *testing.T) {
	database := openTestDB(t)
	stepID := seedSideLaneStep(t, database)
	if err := database.StartSideLane(stepID, "head"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.sql.Exec(`CREATE TRIGGER fail_side_lane_refusal BEFORE UPDATE OF side_lane_refusal ON step_results BEGIN SELECT RAISE(ABORT, 'transient write failure'); END`); err != nil {
		t.Fatal(err)
	}

	err := database.RecordSideLaneRefusal(stepID, "head", "refused")

	if err == nil {
		t.Fatal("RecordSideLaneRefusal() succeeded through a failing write")
	}
	if _, err := database.sql.Exec(`DROP TRIGGER fail_side_lane_refusal`); err != nil {
		t.Fatal(err)
	}
	if got, err := database.TakeSideLane(stepID); err != nil || got == nil || got.Head != "head" || got.Refusal != "" {
		t.Fatalf("record = %+v, %v; want the lane outstanding with no recorded outcome", got, err)
	}
}

func TestRecordSideLaneRefusalRejectsALaneNeverStarted(t *testing.T) {
	for _, test := range []struct {
		name, head, diagnostic string
	}{
		{"no lane", "head", "refused"},
		{"another head", "other", "refused"},
		{"empty diagnostic", "head", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openTestDB(t)
			stepID := seedSideLaneStep(t, database)
			if test.name != "no lane" {
				if err := database.StartSideLane(stepID, "head"); err != nil {
					t.Fatal(err)
				}
			}

			if err := database.RecordSideLaneRefusal(stepID, test.head, test.diagnostic); err == nil {
				t.Fatal("RecordSideLaneRefusal() = nil, want an error")
			}
		})
	}
}
