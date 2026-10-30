package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CIHandback keeps the unresolved round and its publication binding intact.
// Acceptance appends a new head to this receipt; it never changes its evidence.
type CIHandback struct {
	ID                string
	RunID             string
	RoundID           string
	CallerDir         string
	ReleasedHead      string
	PRURL             string
	BaseBranch        string
	BaseHead          string
	PushGeneration    int64
	TargetFingerprint string
	PushRef           string
	AcceptedHead      *string
}

func (d *DB) LatestCIHandback(runID string) (*CIHandback, error) {
	var receipt CIHandback
	err := d.sql.QueryRow(`SELECT id, run_id, round_id, caller_dir, released_head, pr_url, base_branch, base_head, push_generation, target_fingerprint, push_ref, accepted_head FROM ci_handbacks WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID).Scan(
		&receipt.ID, &receipt.RunID, &receipt.RoundID, &receipt.CallerDir, &receipt.ReleasedHead, &receipt.PRURL, &receipt.BaseBranch, &receipt.BaseHead, &receipt.PushGeneration, &receipt.TargetFingerprint, &receipt.PushRef, &receipt.AcceptedHead)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read CI handback: %w", err)
	}
	return &receipt, nil
}

func (d *DB) ReleaseCIHandback(receipt *CIHandback, run *Run, repo *Repo, findings string) error {
	receipt.ID = newID()
	result, err := d.sql.Exec(`INSERT INTO ci_handbacks (id, run_id, round_id, caller_dir, released_head, pr_url, base_branch, base_head, push_generation, target_fingerprint, push_ref, released_at)
		SELECT ?, r.id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE r.id = ? AND r.status = ? AND r.head_sha = ? AND r.last_pushed_sha = r.head_sha AND r.push_generation = ?
		AND r.push_target_fingerprint = ? AND r.push_ref = ? AND r.pr_url = ? AND r.pr_state = 'open' AND COALESCE(r.push_active, 0) = 0
		AND p.upstream_url = ? AND COALESCE(p.fork_url, '') = ?
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = r.repo_id AND other.branch = r.branch AND other.id <> r.id AND other.status IN ('pending', 'running'))
		AND EXISTS (SELECT 1 FROM step_results s JOIN step_rounds round ON round.step_result_id = s.id WHERE s.run_id = r.id AND s.step_name = 'ci' AND s.status = 'awaiting_approval' AND s.findings_json = ? AND s.auto_fix_limit IS NULL AND round.id = ? AND round.findings_json = s.findings_json)`,
		receipt.ID, receipt.RoundID, receipt.CallerDir, receipt.ReleasedHead, receipt.PRURL, receipt.BaseBranch, receipt.BaseHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, now(),
		run.ID, types.RunRunning, receipt.ReleasedHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, receipt.PRURL, repo.UpstreamURL, repo.ForkURL, findings, receipt.RoundID)
	if err != nil {
		return fmt.Errorf("release CI handback: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("CI handback ownership changed before release")
	}
	return nil
}

func (d *DB) AcceptCIHandback(receipt *CIHandback, head string, repo *Repo) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE runs SET head_sha = ?, review_approved_head_sha = NULL, ci_ready_at = NULL, ci_ready_no_ci = 0, updated_at = ?
		WHERE id = ? AND status = 'running' AND head_sha = ? AND last_pushed_sha = ? AND push_generation = ? AND push_target_fingerprint = ? AND push_ref = ? AND pr_url = ? AND pr_state = 'open' AND COALESCE(push_active, 0) = 0
		AND EXISTS (SELECT 1 FROM ci_handbacks WHERE id = ? AND accepted_head IS NULL)
		AND repo_id = ? AND EXISTS (SELECT 1 FROM repos WHERE id = ? AND upstream_url = ? AND COALESCE(fork_url, '') = ?)
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = runs.repo_id AND other.branch = runs.branch AND other.id <> runs.id AND other.status NOT IN ('completed', 'failed', 'cancelled', 'ci_monitor_interrupted'))
		AND EXISTS (SELECT 1 FROM step_results s JOIN step_rounds round ON round.step_result_id = s.id WHERE s.run_id = runs.id AND s.step_name = 'ci' AND s.status = 'awaiting_approval' AND s.auto_fix_limit IS NULL AND round.id = ? AND round.findings_json = s.findings_json)`,
		head, now(), receipt.RunID, receipt.ReleasedHead, receipt.ReleasedHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, receipt.PRURL, receipt.ID,
		repo.ID, repo.ID, repo.UpstreamURL, repo.ForkURL, receipt.RoundID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("CI handback binding changed before acceptance")
	}
	if _, err := tx.Exec(`UPDATE ci_handbacks SET accepted_head = ?, accepted_at = ? WHERE id = ? AND accepted_head IS NULL`, head, now(), receipt.ID); err != nil {
		return err
	}
	return tx.Commit()
}
