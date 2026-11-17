package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepHandback releases one parked Test or Lint round to its contributor.
// A round has at most one receipt, and acceptance only appends the head the
// contributor's single correction commit moved the run to.
type StepHandback struct {
	ID           string
	RunID        string
	StepResultID string
	StepName     types.StepName
	RoundID      string
	CallerDir    string
	ReleasedHead string
	AcceptedHead *string
}

const stepHandbackColumns = `id, run_id, step_result_id, step_name, round_id, caller_dir, released_head, accepted_head`

// stepHandbackGateSQL holds while the step is still parked on the receipt's
// round at the released head with automatic repair disabled.
const stepHandbackGateSQL = `EXISTS (SELECT 1 FROM runs r JOIN step_results s ON s.run_id = r.id
	WHERE r.id = ? AND r.status = 'running' AND r.head_sha = ? AND COALESCE(r.push_active, 0) = 0
	AND s.id = ? AND s.step_name = ? AND s.status = 'awaiting_approval' AND s.auto_fix_limit IS NULL
	AND ? = (SELECT id FROM step_rounds WHERE step_result_id = s.id ORDER BY round DESC, id DESC LIMIT 1))`

func scanStepHandback(row *sql.Row) (*StepHandback, error) {
	var receipt StepHandback
	err := row.Scan(&receipt.ID, &receipt.RunID, &receipt.StepResultID, &receipt.StepName, &receipt.RoundID, &receipt.CallerDir, &receipt.ReleasedHead, &receipt.AcceptedHead)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read step handback: %w", err)
	}
	return &receipt, nil
}

func (d *DB) GetStepHandback(id string) (*StepHandback, error) {
	return scanStepHandback(d.sql.QueryRow(`SELECT `+stepHandbackColumns+` FROM step_handbacks WHERE id = ?`, id))
}

func (d *DB) GetStepHandbackByRound(roundID string) (*StepHandback, error) {
	return scanStepHandback(d.sql.QueryRow(`SELECT `+stepHandbackColumns+` FROM step_handbacks WHERE round_id = ?`, roundID))
}

// ReleaseStepHandback records the receipt only while its gate is still parked
// exactly as the caller verified it.
func (d *DB) ReleaseStepHandback(receipt *StepHandback) error {
	id := newID()
	result, err := d.sql.Exec(`INSERT INTO step_handbacks (id, run_id, step_result_id, step_name, round_id, caller_dir, released_head, released_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ? WHERE `+stepHandbackGateSQL,
		id, receipt.RunID, receipt.StepResultID, receipt.StepName, receipt.RoundID, receipt.CallerDir, receipt.ReleasedHead, now(),
		receipt.RunID, receipt.ReleasedHead, receipt.StepResultID, receipt.StepName, receipt.RoundID)
	if err != nil {
		return fmt.Errorf("release step handback: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("%s gate changed before handback release", receipt.StepName)
	}
	receipt.ID = id
	return nil
}

// AcceptStepHandback moves the run to the contributor's correction and closes
// the receipt in one transaction, only while its gate is still parked.
func (d *DB) AcceptStepHandback(receipt *StepHandback, head string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	result, err := tx.Exec(`UPDATE runs SET head_sha = ?, updated_at = ? WHERE id = ? AND `+stepHandbackGateSQL,
		head, ts, receipt.RunID, receipt.RunID, receipt.ReleasedHead, receipt.StepResultID, receipt.StepName, receipt.RoundID)
	if err != nil {
		return fmt.Errorf("accept step handback: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("%s gate changed before handback acceptance", receipt.StepName)
	}
	result, err = tx.Exec(`UPDATE step_handbacks SET accepted_head = ?, accepted_at = ? WHERE id = ? AND run_id = ? AND round_id = ? AND accepted_head IS NULL`,
		head, ts, receipt.ID, receipt.RunID, receipt.RoundID)
	if err != nil {
		return fmt.Errorf("accept step handback: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("step handback receipt %s is no longer open", receipt.ID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	receipt.AcceptedHead = &head
	return nil
}
