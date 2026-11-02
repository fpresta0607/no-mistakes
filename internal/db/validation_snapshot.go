package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
)

// TerminalSnapshot owns what an immutable predecessor binding covers: the run's
// validation record, gates, steps and rounds, with the repository by ID only.
type TerminalSnapshot struct {
	Run             Run
	Gates           string
	Steps           []StepResult
	Rounds          [][]StepRound
	IsAuthoredChild bool
}

func (snapshot TerminalSnapshot) Digest() string {
	payload, _ := json.Marshal(snapshot)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func (d *DB) ReadTerminalSnapshot(runID string) (*TerminalSnapshot, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return readTerminalSnapshot(tx, runID)
}

func readTerminalSnapshot(tx *sql.Tx, runID string) (*TerminalSnapshot, error) {
	var snapshot TerminalSnapshot
	if err := scanRun(tx.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, runID), &snapshot.Run); err != nil {
		return nil, err
	}
	snapshot.Run.CustodyReturnedAt, snapshot.Run.UpdatedAt = nil, 0
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM validation_admissions WHERE child_id = ?)`, runID).Scan(&snapshot.IsAuthoredChild); err != nil {
		return nil, err
	}
	var gates sql.NullString
	if err := tx.QueryRow(`SELECT gates_json FROM runs WHERE id = ?`, runID).Scan(&gates); err != nil {
		return nil, err
	}
	snapshot.Gates = gates.String
	rows, err := tx.Query(`SELECT `+stepResultColumns+`, round_started_at, override_reason, skip_reason, approval_reason FROM step_results WHERE run_id = ? ORDER BY step_order, id`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var step StepResult
		if err := rows.Scan(&step.ID, &step.RunID, &step.StepName, &step.StepOrder, &step.Status, &step.ExitCode, &step.DurationMS, &step.LogPath, &step.FindingsJSON, &step.Error, &step.StartedAt, &step.CompletedAt, &step.LastActivityAt, &step.LastActivity, &step.AgentPID, &step.AutoFixLimit, &step.RoundStartedAt, &step.OverrideReason, &step.SkipReason, &step.ApprovalReason); err != nil {
			rows.Close()
			return nil, err
		}
		snapshot.Steps = append(snapshot.Steps, step)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, step := range snapshot.Steps {
		rows, err := tx.Query(`SELECT id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, repair_published, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round, id`, step.ID)
		if err != nil {
			return nil, err
		}
		var rounds []StepRound
		for rows.Next() {
			var round StepRound
			if err := rows.Scan(&round.ID, &round.StepResultID, &round.Round, &round.Trigger, &round.FindingsJSON, &round.ReviewedHeadSHA, &round.StartingHeadSHA, &round.TrustedConfigSHA, &round.GlobalConfigYAML, &round.RepoConfigYAML, &round.UserFindingsJSON, &round.SelectedFindingIDs, &round.SelectionSource, &round.FixSummary, &round.RepairPublished, &round.DurationMS, &round.CreatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			rounds = append(rounds, round)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		snapshot.Rounds = append(snapshot.Rounds, rounds)
	}
	return &snapshot, nil
}
