package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	current, err := readTerminalSnapshot(tx, runID)
	if err != nil {
		return nil, err
	}
	if current.Run.RepoID != event.Request.RepoID || current.Run.Branch != event.Request.Branch || current.Run.SubmittedHeadSHA == nil || *current.Run.SubmittedHeadSHA != event.Request.CandidateSHA || !current.Run.LaunchAssertion.Matches(event.Request.LaunchAssertion) || current.Run.LaunchNonce == nil || event.InitialChild.LaunchNonce == nil || *current.Run.LaunchNonce != *event.InitialChild.LaunchNonce || current.Run.LaunchValidationGeneration == nil || *current.Run.LaunchValidationGeneration != event.Request.SuccessorGeneration() || current.Run.LaunchIntentDigest == nil || *current.Run.LaunchIntentDigest != event.Request.IntentDigest {
		return nil, fmt.Errorf("authored child source or launch provenance changed")
	}
	snapshots := []TerminalSnapshot{*current}
	seen := map[string]bool{runID: true}
	validationChildren := make(map[string]bool)
	for {
		if event == nil {
			source := snapshots[len(snapshots)-1].Run
			adoption, err := readTerminalAdoption(tx.QueryRow(`SELECT id, successor_run_id, event_json FROM terminal_adoptions WHERE successor_run_id = ?`, source.ID), "")
			if err != nil {
				return nil, err
			}
			if adoption == nil {
				break
			}
			parentID := adoption.Snapshot.Run.ID
			if seen[parentID] || adoption.Successor.RepoID != source.RepoID || adoption.Successor.Branch != source.Branch {
				return nil, fmt.Errorf("terminal source predecessor identity changed or cycled")
			}
			original, err := readContinuationSnapshot(tx, parentID)
			if err != nil {
				return nil, err
			}
			original.Repo = adoption.Snapshot.Repo
			if !reflect.DeepEqual(original, &adoption.Snapshot) {
				return nil, fmt.Errorf("immutable terminal source predecessor history changed")
			}
			seen[parentID] = true
			history, err := readTerminalSnapshot(tx, parentID)
			if err != nil {
				return nil, err
			}
			snapshots = append(snapshots, *history)
			event, err = readValidationAdmission(tx.QueryRow(`SELECT `+validationAdmissionColumns+` FROM validation_admissions WHERE child_id = ?`, parentID))
			if err != nil {
				return nil, err
			}
			continue
		}
		child := snapshots[len(snapshots)-1]
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
		parentID := event.Snapshot.Run.ID
		if seen[parentID] {
			return nil, fmt.Errorf("validation predecessor cycle")
		}
		seen[parentID] = true
		original, err := readTerminalSnapshot(tx, parentID)
		if err != nil || !reflect.DeepEqual(original, &event.Snapshot) {
			return nil, fmt.Errorf("immutable validation predecessor history changed")
		}
		if event.Request.HistoricalSourceCompleteness != family.HistoricalSourceCompleteness || !reflect.DeepEqual(event.Request.ControllerPolicy, family.ControllerPolicy) {
			return nil, fmt.Errorf("validation descendants cannot change the controller policy assertion or historical uncertainty")
		}
		snapshots = append(snapshots, event.Snapshot)
		event, err = readValidationAdmission(tx.QueryRow(`SELECT `+validationAdmissionColumns+` FROM validation_admissions WHERE child_id = ?`, parentID))
		if err != nil {
			return nil, err
		}
	}
	for snapshotIndex, snapshot := range snapshots {
		if snapshotIndex > 0 {
			if err := snapshot.validateNativeRefusals(); err != nil {
				return nil, err
			}
		}
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
