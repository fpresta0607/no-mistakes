package db

import (
	"database/sql"
	"encoding/json"
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
	Candidate         *CIHandbackCandidate
	Source            *CIHandbackSource
}

// Source distinguishes an ordinary failed-check return from conflict handback.
// Its original caller and validation binding survive acceptance and restarts.
type CIHandbackSource struct {
	CallerHead    string
	SubmittedHead string
	ReviewHead    string
	PushKind      string
	PushedAt      int64
	Findings      string
	AutoFixLimits map[types.StepName]int
}

type CIHandbackCandidate struct {
	Head          string
	BaseHead      string
	ManagedBranch string
	UpstreamURL   string
	ForkURL       string
	ReviewRoundID string
}

func (d *DB) LatestCIHandback(runID string) (*CIHandback, error) {
	var receipt CIHandback
	var candidateJSON *string
	var sourceJSON *string
	err := d.sql.QueryRow(`SELECT id, run_id, round_id, caller_dir, released_head, pr_url, base_branch, base_head, push_generation, target_fingerprint, push_ref, accepted_head, candidate_json, source_json FROM ci_handbacks WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID).Scan(
		&receipt.ID, &receipt.RunID, &receipt.RoundID, &receipt.CallerDir, &receipt.ReleasedHead, &receipt.PRURL, &receipt.BaseBranch, &receipt.BaseHead, &receipt.PushGeneration, &receipt.TargetFingerprint, &receipt.PushRef, &receipt.AcceptedHead, &candidateJSON, &sourceJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read CI handback: %w", err)
	}
	if sourceJSON != nil {
		if err := json.Unmarshal([]byte(*sourceJSON), &receipt.Source); err != nil {
			return nil, fmt.Errorf("read CI handback source: %w", err)
		}
		if receipt.Source == nil || receipt.Source.CallerHead == "" || receipt.Source.SubmittedHead == "" || receipt.Source.ReviewHead == "" || receipt.Source.PushKind == "" || receipt.Source.PushedAt == 0 || receipt.Source.Findings == "" || receipt.Source.AutoFixLimits == nil {
			return nil, fmt.Errorf("CI handback source is incomplete")
		}
	}
	if candidateJSON != nil {
		if err := json.Unmarshal([]byte(*candidateJSON), &receipt.Candidate); err != nil {
			return nil, fmt.Errorf("read CI handback candidate: %w", err)
		}
		if receipt.Candidate == nil || receipt.Candidate.Head == "" || receipt.Candidate.BaseHead == "" || receipt.Candidate.ManagedBranch == "" || receipt.Candidate.UpstreamURL == "" {
			return nil, fmt.Errorf("CI handback candidate is incomplete")
		}
	}
	return &receipt, nil
}

func (d *DB) LatestReviewRoundID(runID string) (string, error) {
	var id string
	err := d.sql.QueryRow(`SELECT COALESCE((SELECT round.id FROM step_rounds round JOIN step_results s ON s.id = round.step_result_id WHERE s.run_id = ? AND s.step_name = 'review' ORDER BY round.round DESC, round.id DESC LIMIT 1), '')`, runID).Scan(&id)
	return id, err
}

func (d *DB) PrepareCIHandback(receipt *CIHandback, head, baseHead, managedBranch string, repo *Repo) error {
	reviewRoundID, err := d.LatestReviewRoundID(receipt.RunID)
	if err != nil {
		return err
	}
	candidate := &CIHandbackCandidate{Head: head, BaseHead: baseHead, ManagedBranch: managedBranch, UpstreamURL: repo.UpstreamURL, ForkURL: repo.ForkURL, ReviewRoundID: reviewRoundID}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	result, err := d.sql.Exec(`UPDATE ci_handbacks SET candidate_json = ? WHERE id = ? AND run_id = ? AND accepted_head IS NULL AND candidate_json IS NULL
		AND EXISTS (SELECT 1 FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.id = ci_handbacks.run_id
		AND r.status = 'running' AND r.head_sha = ci_handbacks.released_head AND r.last_pushed_sha = r.head_sha
		AND r.push_generation = ci_handbacks.push_generation AND r.push_target_fingerprint = ci_handbacks.target_fingerprint AND r.push_ref = ci_handbacks.push_ref
		AND r.pr_url = ci_handbacks.pr_url AND r.pr_state = 'open' AND COALESCE(r.push_active, 0) = 0 AND r.custody_returned_at IS NULL
		AND p.id = ? AND p.upstream_url = ? AND COALESCE(p.fork_url, '') = ?
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = r.repo_id AND other.branch = r.branch AND other.id <> r.id AND other.status NOT IN ('completed', 'failed', 'cancelled', 'ci_monitor_interrupted'))
		AND EXISTS (SELECT 1 FROM step_results s JOIN step_rounds round ON round.step_result_id = s.id WHERE s.run_id = r.id AND s.step_name = 'ci' AND s.status = 'awaiting_approval' AND s.auto_fix_limit IS NULL AND round.id = ci_handbacks.round_id AND round.findings_json = s.findings_json))`,
		string(encoded), receipt.ID, receipt.RunID, repo.ID, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("CI handback binding changed before checkpoint")
	}
	receipt.Candidate = candidate
	return nil
}

func (d *DB) ClearCIHandbackCandidate(receipt *CIHandback) error {
	encoded, err := json.Marshal(receipt.Candidate)
	if err != nil {
		return err
	}
	result, err := d.sql.Exec(`UPDATE ci_handbacks SET candidate_json = NULL WHERE id = ? AND run_id = ? AND accepted_head IS NULL AND candidate_json = ?`, receipt.ID, receipt.RunID, string(encoded))
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("CI handback checkpoint changed before restoration")
	}
	receipt.Candidate = nil
	return nil
}

func (d *DB) ReleaseCIHandback(receipt *CIHandback, run *Run, repo *Repo, findings string) error {
	var sourceJSON *string
	if receipt.Source != nil {
		encoded, err := json.Marshal(receipt.Source)
		if err != nil {
			return err
		}
		value := string(encoded)
		sourceJSON = &value
	}
	receipt.ID = newID()
	result, err := d.sql.Exec(`INSERT INTO ci_handbacks (id, run_id, round_id, caller_dir, released_head, pr_url, base_branch, base_head, push_generation, target_fingerprint, push_ref, released_at, source_json)
		SELECT ?, r.id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE r.id = ? AND r.status = ? AND r.head_sha = ? AND r.last_pushed_sha = r.head_sha AND r.push_generation = ?
		AND r.push_target_fingerprint = ? AND r.push_ref = ? AND r.pr_url = ? AND r.pr_state = 'open' AND COALESCE(r.push_active, 0) = 0
		AND p.upstream_url = ? AND COALESCE(p.fork_url, '') = ?
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = r.repo_id AND other.branch = r.branch AND other.id <> r.id AND other.status IN ('pending', 'running'))
		AND (? IS NULL OR (r.submitted_head_sha = ? AND r.review_approved_head_sha = ? AND r.push_target_kind = ? AND r.last_pushed_at = ?
		AND NOT EXISTS (SELECT 1 FROM runs newer WHERE newer.repo_id = r.repo_id AND newer.branch = r.branch AND newer.id > r.id)
		AND NOT EXISTS (SELECT 1 FROM step_results writer WHERE writer.run_id = r.id AND (writer.agent_pid IS NOT NULL OR (writer.step_name <> 'ci' AND writer.status <> 'completed')))))
		AND EXISTS (SELECT 1 FROM step_results s JOIN step_rounds round ON round.step_result_id = s.id WHERE s.run_id = r.id AND s.step_name = 'ci' AND s.status = 'awaiting_approval' AND s.findings_json = ? AND s.auto_fix_limit IS NULL AND round.id = ? AND round.findings_json = s.findings_json)`,
		receipt.ID, receipt.RoundID, receipt.CallerDir, receipt.ReleasedHead, receipt.PRURL, receipt.BaseBranch, receipt.BaseHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, now(), sourceJSON,
		run.ID, types.RunRunning, receipt.ReleasedHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, receipt.PRURL, repo.UpstreamURL, repo.ForkURL,
		sourceJSON, run.SubmittedHeadSHA, run.ReviewApprovedHeadSHA, run.PushTargetKind, run.LastPushedAt, findings, receipt.RoundID)
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

func (d *DB) CancelReleasedCIHandback(runID string) error {
	receipt, err := d.LatestCIHandback(runID)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.Candidate != nil || receipt.AcceptedHead != nil {
		return fmt.Errorf("run %s has no settled released CI handback", runID)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	result, err := tx.Exec(`UPDATE runs SET status = ?, error = ?, awaiting_agent_since = NULL,
		parked_ms = COALESCE(parked_ms, 0) + CASE WHEN ? > awaiting_agent_since THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
		updated_at = ?
		WHERE id = ? AND status = 'running' AND awaiting_agent_since IS NOT NULL AND COALESCE(push_active, 0) = 0
		AND head_sha = ? AND last_pushed_sha = head_sha AND push_generation = ? AND push_target_fingerprint = ? AND push_ref = ?
		AND custody_returned_at IS NULL
		AND EXISTS (SELECT 1 FROM ci_handbacks h WHERE h.run_id = runs.id AND h.id = ? AND h.accepted_head IS NULL AND h.candidate_json IS NULL
		AND h.id = (SELECT id FROM ci_handbacks WHERE run_id = runs.id ORDER BY id DESC LIMIT 1))
		AND NOT EXISTS (SELECT 1 FROM step_results s WHERE s.run_id = runs.id AND (s.agent_pid IS NOT NULL
		OR (s.step_name <> 'ci' AND s.status <> 'completed') OR (s.step_name = 'ci' AND s.status <> 'awaiting_approval')))
		AND EXISTS (SELECT 1 FROM step_results s JOIN step_rounds round ON round.step_result_id = s.id
		WHERE s.run_id = runs.id AND s.step_name = 'ci' AND s.status = 'awaiting_approval' AND s.auto_fix_limit IS NULL
		AND round.id = ? AND round.findings_json = s.findings_json
		AND round.id = (SELECT id FROM step_rounds WHERE step_result_id = s.id ORDER BY round DESC, id DESC LIMIT 1))
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = runs.repo_id AND other.branch = runs.branch AND other.id <> runs.id AND other.status IN ('pending', 'running'))`,
		types.RunCancelled, types.RunCancelReasonAbortedByUser, ts, ts, ts, runID,
		receipt.ReleasedHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, receipt.ID, receipt.RoundID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("run %s is not a writer-free released CI handback", runID)
	}
	if _, err := tx.Exec(`UPDATE step_results SET status = ?, error = ?, completed_at = ?, last_activity_at = ?, last_activity = ?
		WHERE run_id = ? AND step_name = 'ci'`, types.StepStatusFailed, types.RunCancelReasonAbortedByUser, ts, ts, "step failed: "+types.RunCancelReasonAbortedByUser, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) AcceptCIHandback(receipt *CIHandback, head string, repo *Repo) error {
	if receipt.Candidate == nil || receipt.Candidate.Head != head || receipt.Candidate.UpstreamURL != repo.UpstreamURL || receipt.Candidate.ForkURL != repo.ForkURL {
		return fmt.Errorf("CI handback has no matching candidate checkpoint")
	}
	encoded, err := json.Marshal(receipt.Candidate)
	if err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE runs SET head_sha = ?, review_approved_head_sha = NULL, ci_ready_at = NULL, ci_ready_no_ci = 0, updated_at = ?
		WHERE id = ? AND status = 'running' AND head_sha = ? AND last_pushed_sha = ? AND push_generation = ? AND push_target_fingerprint = ? AND push_ref = ? AND pr_url = ? AND pr_state = 'open' AND COALESCE(push_active, 0) = 0
		AND EXISTS (SELECT 1 FROM ci_handbacks WHERE id = ? AND run_id = runs.id AND accepted_head IS NULL AND candidate_json = ?)
		AND repo_id = ? AND EXISTS (SELECT 1 FROM repos WHERE id = ? AND upstream_url = ? AND COALESCE(fork_url, '') = ?)
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = runs.repo_id AND other.branch = runs.branch AND other.id <> runs.id AND other.status NOT IN ('completed', 'failed', 'cancelled', 'ci_monitor_interrupted'))
		AND EXISTS (SELECT 1 FROM step_results s JOIN step_rounds round ON round.step_result_id = s.id WHERE s.run_id = runs.id AND s.step_name = 'ci' AND s.status = 'awaiting_approval' AND s.auto_fix_limit IS NULL AND round.id = ? AND round.findings_json = s.findings_json)`,
		head, now(), receipt.RunID, receipt.ReleasedHead, receipt.ReleasedHead, receipt.PushGeneration, receipt.TargetFingerprint, receipt.PushRef, receipt.PRURL, receipt.ID, string(encoded),
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
