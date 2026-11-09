package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// SetReviewScopeDecision records the explicit scope and the selection that
// accepted it together. Ordinary fix instructions never enter this record.
func (d *DB) SetReviewScopeDecision(decision types.ReviewScopeDecision, selectedJSON, payload string, origin types.ResponseOrigin) error {
	if err := decision.Validate(); err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var runID, stepName, head, base, roundID, findingJSON string
	var reviewed, trusted sql.NullString
	err = tx.QueryRow(`SELECT s.run_id, s.step_name, r.head_sha, r.base_sha, sr.id, sr.findings_json, sr.reviewed_head_sha, sr.trusted_config_sha
		FROM step_rounds sr JOIN step_results s ON s.id = sr.step_result_id JOIN runs r ON r.id = s.run_id
		WHERE sr.id = ? AND s.id = ? AND r.id = ? AND r.status = ?
		AND sr.round = (SELECT MAX(round) FROM step_rounds WHERE step_result_id = s.id)`,
		decision.RoundID, decision.StepResultID, decision.RunID, types.RunRunning).Scan(&runID, &stepName, &head, &base, &roundID, &findingJSON, &reviewed, &trusted)
	if err != nil {
		return fmt.Errorf("scope decision source unavailable; inspect the current Review run and round: %w", err)
	}
	if stepName != string(types.StepReview) || head != decision.SourceHeadSHA || base != decision.SourceBaseSHA || reviewed.String != head || trusted.String != decision.TrustedConfigSHA {
		return fmt.Errorf("scope decision does not match the current run, review round and source")
	}
	if err := validateScopeFinding(decision, findingJSON, selectedJSON); err != nil {
		return err
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	var previous string
	err = tx.QueryRow(`SELECT decision_json FROM review_scope_decisions WHERE id = ? OR round_id = ?`, decision.DecisionID, decision.RoundID).Scan(&previous)
	if err == nil && previous != string(encoded) {
		return fmt.Errorf("scope decision identity or round already has a different immutable decision")
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		var historyBytes int
		if err := tx.QueryRow(`SELECT COALESCE(SUM(length(CAST(decision_json AS BLOB)) + 1), 0) FROM review_scope_decisions WHERE run_id = ?`, runID).Scan(&historyBytes); err != nil {
			return err
		}
		if historyBytes+len(encoded)+2 > 64*1024 {
			return fmt.Errorf("scope decision exceeds this run's 64 KiB history budget; shorten the proposed decision before responding")
		}
		if _, err := tx.Exec(`INSERT INTO review_scope_decisions (id, run_id, step_result_id, round_id, decision_json) VALUES (?, ?, ?, ?, ?)`, decision.DecisionID, runID, decision.StepResultID, roundID, string(encoded)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ?, response_origin = ? WHERE id = ?`, selectedJSON, RoundSelectionSourceUser, payload, origin.OrUnknown(), roundID); err != nil {
		return err
	}
	return tx.Commit()
}

func validateScopeFinding(decision types.ReviewScopeDecision, raw, selectedJSON string) error {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return fmt.Errorf("scope decision original findings are unreadable: %w", err)
	}
	var selected []string
	if err := json.Unmarshal([]byte(selectedJSON), &selected); err != nil {
		return fmt.Errorf("scope decision selection is unreadable: %w", err)
	}
	isSelected := false
	for _, id := range selected {
		isSelected = isSelected || id == decision.Finding.ID
	}
	matches := 0
	for _, finding := range findings.Items {
		if finding.ID == decision.Finding.ID {
			if finding != decision.Finding {
				return fmt.Errorf("scope decision finding content differs from the original review finding")
			}
			matches++
		}
	}
	if !isSelected || matches != 1 {
		return fmt.Errorf("scope decision requires exactly one matching selected review finding")
	}
	return nil
}

// GetReviewScopeDecisions loads only this run's explicit assertions, validating
// their immutable source rounds rather than a later mutable run head.
func (d *DB) GetReviewScopeDecisions(runID string) ([]types.ReviewScopeDecision, error) {
	rows, err := d.sql.Query(`SELECT d.id, d.run_id, d.step_result_id, d.round_id, d.decision_json, s.run_id, s.step_name,
		sr.findings_json, sr.selected_finding_ids, sr.reviewed_head_sha, sr.trusted_config_sha
		FROM review_scope_decisions d LEFT JOIN step_results s ON s.id = d.step_result_id
		LEFT JOIN step_rounds sr ON sr.id = d.round_id AND sr.step_result_id = s.id
		WHERE d.run_id = ? ORDER BY sr.round, d.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decisions []types.ReviewScopeDecision
	for rows.Next() {
		var id, owner, stepID, roundID, raw string
		var stepRun, stepName, findings, selected, reviewed, trusted sql.NullString
		if err := rows.Scan(&id, &owner, &stepID, &roundID, &raw, &stepRun, &stepName, &findings, &selected, &reviewed, &trusted); err != nil {
			return nil, err
		}
		var decision types.ReviewScopeDecision
		if err := json.Unmarshal([]byte(raw), &decision); err != nil {
			return nil, fmt.Errorf("scope decision %s is unreadable", id)
		}
		if err := decision.Validate(); err != nil {
			return nil, err
		}
		if decision.DecisionID != id || decision.RunID != runID || owner != runID || stepRun.String != runID || decision.StepResultID != stepID || decision.RoundID != roundID || stepName.String != string(types.StepReview) || decision.SourceHeadSHA != reviewed.String || decision.TrustedConfigSHA != trusted.String {
			return nil, fmt.Errorf("scope decision %s lost its run, review round or source binding", id)
		}
		if err := validateScopeFinding(decision, findings.String, selected.String); err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}
