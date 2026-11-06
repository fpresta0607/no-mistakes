package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type ValidationHistoryEntry struct {
	Snapshot  TerminalSnapshot
	Admission *ValidationAdmission
	Adoption  *TerminalAdoption
}

func (d *DB) ValidationHistory(runID string) ([]ValidationHistoryEntry, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return readValidationHistory(tx, runID)
}

func readValidationHistory(tx *sql.Tx, runID string) ([]ValidationHistoryEntry, error) {
	var history []ValidationHistoryEntry
	seen := make(map[string]bool)
	for runID != "" {
		if seen[runID] {
			return nil, fmt.Errorf("validation predecessor cycle")
		}
		seen[runID] = true
		current, err := readTerminalSnapshot(tx, runID)
		if err != nil {
			return nil, err
		}
		if len(history) > 0 {
			if err := current.validateNativeRefusals(); err != nil {
				return nil, err
			}
		}
		admission, err := readValidationAdmission(tx.QueryRow(`SELECT `+validationAdmissionColumns+` FROM validation_admissions WHERE child_id = ?`, runID))
		if err != nil {
			return nil, err
		}
		adoption, err := readTerminalAdoption(tx.QueryRow(`SELECT id, successor_run_id, event_json FROM terminal_adoptions WHERE successor_run_id = ?`, runID), "")
		if err != nil {
			return nil, err
		}
		if admission != nil && adoption != nil {
			return nil, fmt.Errorf("validation predecessor ownership is ambiguous")
		}
		history = append(history, ValidationHistoryEntry{Snapshot: *current, Admission: admission, Adoption: adoption})
		runID = ""
		switch {
		case admission != nil:
			if current.Run.RepoID != admission.Request.RepoID || current.Run.Branch != admission.Request.Branch || current.Run.SubmittedHeadSHA == nil || *current.Run.SubmittedHeadSHA != admission.Request.CandidateSHA || !current.Run.LaunchAssertion.Matches(admission.Request.LaunchAssertion) || current.Run.LaunchNonce == nil || admission.InitialChild.LaunchNonce == nil || *current.Run.LaunchNonce != *admission.InitialChild.LaunchNonce || current.Run.LaunchValidationGeneration == nil || *current.Run.LaunchValidationGeneration != admission.Request.SuccessorGeneration() || current.Run.LaunchIntentDigest == nil || *current.Run.LaunchIntentDigest != admission.Request.IntentDigest {
				return nil, fmt.Errorf("authored child source or launch provenance changed")
			}
			original, err := readTerminalSnapshot(tx, admission.Snapshot.Run.ID)
			if err != nil || !reflect.DeepEqual(original, &admission.Snapshot) {
				return nil, fmt.Errorf("immutable validation predecessor history changed")
			}
			runID = original.Run.ID
		case adoption != nil:
			source, initial := current.Run, adoption.Successor
			if source.RepoID != adoption.Snapshot.Run.RepoID || source.Branch != adoption.Snapshot.Run.Branch || initial.RepoID != source.RepoID || initial.Branch != source.Branch || source.SubmittedHeadSHA == nil || *source.SubmittedHeadSHA != adoption.CandidateSHA || !source.LaunchAssertion.Matches(adoption.Assertion) || !reflect.DeepEqual(source.LaunchNonce, initial.LaunchNonce) || !reflect.DeepEqual(source.LaunchValidationGeneration, initial.LaunchValidationGeneration) || !reflect.DeepEqual(source.LaunchIntentDigest, initial.LaunchIntentDigest) {
				return nil, fmt.Errorf("terminal source predecessor identity changed")
			}
			original, err := readContinuationSnapshot(tx, adoption.Snapshot.Run.ID)
			if err != nil {
				return nil, err
			}
			original.Repo = adoption.Snapshot.Repo
			if !reflect.DeepEqual(original, &adoption.Snapshot) {
				return nil, fmt.Errorf("immutable terminal source predecessor history changed")
			}
			runID = original.Run.ID
		}
	}
	return history, nil
}

// ValidationFamily uses the snapshot/round ledger for both history and spending.
// A dispatched selection is spent even if cancellation preceded its fix round.
func (d *DB) ValidationFamily(runID string) (*authoredvalidation.Family, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return readValidationFamily(tx, runID)
}

func readValidationFamily(tx *sql.Tx, runID string) (*authoredvalidation.Family, error) {
	event, err := readValidationAdmission(tx.QueryRow(`SELECT `+validationAdmissionColumns+` FROM validation_admissions WHERE child_id = ?`, runID))
	if err != nil || event == nil {
		return nil, err
	}
	family := &authoredvalidation.Family{AdmissionID: event.ID, PredecessorID: event.Request.PredecessorID, PolicyOrigin: "controller_assertion", ControllerPolicy: event.Request.ControllerPolicy, HistoricalSourceCompleteness: event.Request.HistoricalSourceCompleteness, Spending: make(map[types.StepName]authoredvalidation.RepairSpending)}
	for _, name := range append([]types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}, event.Steps...) {
		family.Spending[name] = authoredvalidation.RepairSpending{}
	}
	history, err := readValidationHistory(tx, runID)
	if err != nil {
		return nil, err
	}
	validationChildren := make(map[string]bool)
	for _, entry := range history {
		event := entry.Admission
		if event == nil {
			continue
		}
		child := entry.Snapshot
		validationChildren[child.Run.ID] = true
		if child.Run.HeadSHA != event.Request.CandidateSHA {
			family.HasChildRepairs = true
		}
		for _, rounds := range child.Rounds {
			for _, round := range rounds {
				if round.StartingHeadSHA != nil && *round.StartingHeadSHA != event.Request.CandidateSHA || round.ReviewedHeadSHA != nil && *round.ReviewedHeadSHA != event.Request.CandidateSHA {
					family.HasChildRepairs = true
				}
			}
		}
		if event.Request.HistoricalSourceCompleteness != family.HistoricalSourceCompleteness || !reflect.DeepEqual(event.Request.ControllerPolicy, family.ControllerPolicy) {
			return nil, fmt.Errorf("validation descendants cannot change the controller policy assertion or historical uncertainty")
		}
	}
	for _, entry := range history {
		snapshot := entry.Snapshot
		if len(snapshot.Steps) != len(snapshot.Rounds) {
			return nil, fmt.Errorf("validation round ledger is incomplete")
		}
		for index, step := range snapshot.Steps {
			spending := family.Spending[step.StepName]
			pending := ""
			for _, round := range snapshot.Rounds[index] {
				if validationChildren[snapshot.Run.ID] && (round.IsFixRound() || round.SelectionSource != nil && (*round.SelectionSource == RoundSelectionSourceAutoFix || *round.SelectionSource == RoundSelectionSourceUser)) {
					family.HasChildRepairs = true
				}
				if round.IsFixRound() {
					source := pending
					if source == "" && round.Trigger == "user_fix" {
						source = RoundSelectionSourceUser
					}
					switch source {
					case RoundSelectionSourceAutoFix:
						spending.Automatic++
					case RoundSelectionSourceUser:
						spending.Manual++
					default:
						return nil, fmt.Errorf("repair round %s has no provable automatic/manual selection", round.ID)
					}
					pending = ""
				}
				if round.SelectionSource == nil {
					continue
				}
				switch *round.SelectionSource {
				case RoundSelectionSourceAutoFix, RoundSelectionSourceUser:
					var selected []string
					if pending != "" || round.SelectedFindingIDs == nil || json.Unmarshal([]byte(*round.SelectedFindingIDs), &selected) != nil || selected == nil {
						return nil, fmt.Errorf("repair selection %s is ambiguous or unreadable", round.ID)
					}
					pending = *round.SelectionSource
				case RoundSelectionSourceUserDeclined:
				default:
					return nil, fmt.Errorf("unknown repair selection source in round %s", round.ID)
				}
			}
			switch pending {
			case RoundSelectionSourceAutoFix:
				spending.Automatic++
			case RoundSelectionSourceUser:
				spending.Manual++
			}
			if spending.Automatic != 0 || spending.Manual != 0 || snapshot.Run.ID == runID {
				family.Spending[step.StepName] = spending
			}
		}
	}
	return family, nil
}
