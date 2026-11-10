package db

import (
	"database/sql"
	"fmt"
)

// A step's work started beside the review under global side_by_side runs in
// a lane whose outcome lives in the daemon's memory. Its record on the step's
// row carries the one outcome that must outlive that daemon: a native agent
// safety refusal, which the step reports at its turn instead of doing the
// refused work again. The record is written before the lane starts and stays
// until the lane ends without a refusal or the step's turn takes it, so a lane
// that never recorded how it ended is still visible to a later daemon.

// SideLaneRecord is a step's outstanding side lane record.
type SideLaneRecord struct {
	// Head is the commit the lane ran on.
	Head string
	// Refusal is the lane's native safety refusal diagnostic, or "" when the
	// lane never recorded how it ended.
	Refusal string
}

// StartSideLane records that stepID's work starts beside the review on head.
// It refuses while an earlier lane's record for the step is outstanding, since
// a new lane would do that lane's possibly refused work again.
func (d *DB) StartSideLane(stepID, head string) error {
	result, err := d.sql.Exec(`UPDATE step_results SET side_lane_head = ?, side_lane_refusal = NULL WHERE id = ? AND side_lane_head IS NULL`, head, stepID)
	if err != nil {
		return fmt.Errorf("record side lane start: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("record side lane start rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("record side lane start: step %s is missing or still holds an earlier lane's record", stepID)
	}
	return nil
}

// EndSideLane clears the record of stepID's lane on head, which ended without
// a native safety refusal. A record the step's turn already took is left alone.
func (d *DB) EndSideLane(stepID, head string) error {
	if _, err := d.sql.Exec(`UPDATE step_results SET side_lane_head = NULL WHERE id = ? AND side_lane_head = ? AND side_lane_refusal IS NULL`, stepID, head); err != nil {
		return fmt.Errorf("record side lane end: %w", err)
	}
	return nil
}

// RecordSideLaneRefusal records that stepID's lane on head ended in a native
// agent safety refusal.
func (d *DB) RecordSideLaneRefusal(stepID, head, diagnostic string) error {
	if diagnostic == "" {
		return fmt.Errorf("record side lane refusal: the diagnostic is empty")
	}
	result, err := d.sql.Exec(`UPDATE step_results SET side_lane_refusal = ? WHERE id = ? AND side_lane_head = ?`, diagnostic, stepID, head)
	if err != nil {
		return fmt.Errorf("record side lane refusal: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("record side lane refusal rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("record side lane refusal: step %s holds no lane record on %s", stepID, head)
	}
	return nil
}

// TakeSideLane returns and clears stepID's outstanding side lane record, or
// nil when there is none.
func (d *DB) TakeSideLane(stepID string) (*SideLaneRecord, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("take side lane record: %w", err)
	}
	defer tx.Rollback()
	var head, refusal sql.NullString
	err = tx.QueryRow(`SELECT side_lane_head, side_lane_refusal FROM step_results WHERE id = ?`, stepID).Scan(&head, &refusal)
	if err == sql.ErrNoRows || (err == nil && !head.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("take side lane record: %w", err)
	}
	if _, err := tx.Exec(`UPDATE step_results SET side_lane_head = NULL, side_lane_refusal = NULL WHERE id = ?`, stepID); err != nil {
		return nil, fmt.Errorf("take side lane record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("take side lane record: %w", err)
	}
	return &SideLaneRecord{Head: head.String, Refusal: refusal.String}, nil
}
