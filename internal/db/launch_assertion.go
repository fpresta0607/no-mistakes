package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/launchassert"
)

func (d *DB) SetLaunchAssertionProof(runID string, expected *launchassert.Expectation, proof *launchassert.Proof) error {
	if expected == nil {
		return fmt.Errorf("captured launch assertion is required")
	}
	if err := proof.Check(expected); err != nil {
		return err
	}
	result, err := d.sql.Exec(`UPDATE runs SET launch_assertion_proof = ? WHERE id = ? AND launch_assertion IS ? AND launch_assertion_proof IS NULL`, proof, runID, expected)
	if err != nil {
		return fmt.Errorf("record launch proof: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return fmt.Errorf("record launch proof: immutable binding missing or already proved")
	}
	return nil
}
