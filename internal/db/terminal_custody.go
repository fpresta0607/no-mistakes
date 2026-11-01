package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type TerminalSnapshot struct {
	Run    Run
	Repo   Repo
	Gates  string
	Steps  []StepResult
	Rounds [][]StepRound
}

func (d *DB) RecordTerminalSourceState(runID, state string) error {
	_, err := d.sql.Exec(`UPDATE runs SET terminal_source_state = ? WHERE id = ?`, state, runID)
	return err
}

func (snapshot TerminalSnapshot) Digest() string {
	payload, _ := json.Marshal(snapshot)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// TerminalAdoption is an operator acceptance of unvalidated source, never a
// replacement for the original launch receipt or evidence of a successful gate.
type TerminalAdoption struct {
	ID            string
	CreatedAt     int64
	Snapshot      TerminalSnapshot
	CandidateSHA  string
	CandidateTree string
	SourceRef     string
	GateHead      string
	RecoveryHead  string
	CallerDir     string
	CallerHead    string
	ManagedDir    string
	Reason        string
	Operator      string
	Assertion     *launchassert.Expectation
	Proof         *launchassert.Proof
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
	repo := &snapshot.Repo
	if err := tx.QueryRow(`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos WHERE id = ?`, snapshot.Run.RepoID).Scan(&repo.ID, &repo.WorkingPath, &repo.UpstreamURL, &repo.ForkURL, &repo.DefaultBranch, &repo.CreatedAt); err != nil {
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

func (d *DB) GetTerminalAdoption(runID string) (*TerminalAdoption, error) {
	return readTerminalAdoption(d.sql.QueryRow(`SELECT id, event_json FROM terminal_adoptions WHERE run_id = ?`, runID), runID)
}

func readTerminalAdoption(row *sql.Row, runID string) (*TerminalAdoption, error) {
	var id, payload string
	if err := row.Scan(&id, &payload); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var event TerminalAdoption
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, err
	}
	if event.ID != id || event.Snapshot.Run.ID != runID {
		return nil, fmt.Errorf("terminal adoption identity is inconsistent")
	}
	if event.Assertion == nil || event.Snapshot.Run.LaunchAssertion == nil {
		return nil, fmt.Errorf("terminal adoption launch provenance is missing")
	}
	if err := event.Snapshot.Run.LaunchAssertionProof.Check(event.Snapshot.Run.LaunchAssertion); err != nil {
		return nil, err
	}
	if err := event.Proof.Check(event.Assertion); err != nil {
		return nil, err
	}
	return &event, nil
}

func (d *DB) AdoptTerminalCandidate(ctx context.Context, event TerminalAdoption) (*TerminalAdoption, bool, error) {
	run := &event.Snapshot.Run
	for _, object := range []string{event.CandidateSHA, event.CandidateTree} {
		decoded, err := hex.DecodeString(object)
		if err != nil || len(decoded) != 20 || object != strings.ToLower(object) {
			return nil, false, fmt.Errorf("terminal adoption requires full commit and tree identities")
		}
	}
	if !run.Status.Terminal() || run.Status == types.RunCompleted || run.CustodyReturnedAt != nil || run.PushActive || run.LaunchAssertion == nil || run.LaunchValidationGeneration == nil || event.Assertion == nil || strings.TrimSpace(event.Reason) == "" || len(event.Reason) > 4096 || event.Operator == "" {
		return nil, false, fmt.Errorf("terminal adoption requires an unrecovered terminal run, original launch authority and an explicit operator reason")
	}
	if err := run.LaunchAssertionProof.Check(run.LaunchAssertion); err != nil {
		return nil, false, err
	}
	if err := event.Proof.Check(event.Assertion); err != nil {
		return nil, false, err
	}
	fresh := *event.Assertion
	fresh.TrustedSHA = run.LaunchAssertion.TrustedSHA
	if !fresh.Matches(run.LaunchAssertion) {
		return nil, false, fmt.Errorf("recovery cannot change the authorized agent selection")
	}
	if event.ID != "" || event.CreatedAt != 0 {
		return nil, false, fmt.Errorf("recovery cannot supply historical event identity")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runs SET id = id WHERE id = ?`, run.ID); err != nil {
		return nil, false, err
	}
	existing, err := readTerminalAdoption(tx.QueryRow(`SELECT id, event_json FROM terminal_adoptions WHERE run_id = ?`, run.ID), run.ID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		comparison := *existing
		comparison.ID, comparison.CreatedAt = "", 0
		if !reflect.DeepEqual(comparison, event) {
			return nil, false, fmt.Errorf("terminal adoption already has a different binding")
		}
		return existing, false, nil
	}
	current, err := readTerminalSnapshot(tx, run.ID)
	if err != nil || !reflect.DeepEqual(current, &event.Snapshot) {
		return nil, false, fmt.Errorf("terminal source, publication, receipt or history changed")
	}
	var competing bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM runs WHERE repo_id = ? AND branch = ? AND id <> ? AND (status IN ('pending','running') OR id > ?))`, run.RepoID, run.Branch, run.ID, run.ID).Scan(&competing); err != nil {
		return nil, false, err
	}
	if competing {
		return nil, false, fmt.Errorf("another run owns this branch")
	}
	event.ID, event.CreatedAt = newID(), now()
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`INSERT INTO terminal_adoptions (run_id, id, event_json) VALUES (?, ?, ?)`, run.ID, event.ID, string(payload)); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE runs SET status = ?, head_sha = ?, review_approved_head_sha = NULL, terminal_head_verified_at = NULL, ci_ready_at = NULL, ci_ready_no_ci = 0, push_active = 0, error = NULL, awaiting_agent_since = NULL, updated_at = ? WHERE id = ?`, types.RunRunning, event.CandidateSHA, event.CreatedAt, run.ID); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE step_results SET status = ?, exit_code = NULL, duration_ms = NULL, log_path = NULL, findings_json = NULL, error = NULL, started_at = NULL, round_started_at = NULL, completed_at = NULL, last_activity_at = NULL, last_activity = NULL, agent_pid = NULL, override_reason = NULL, approval_reason = NULL, skip_reason = NULL WHERE run_id = ? AND step_order >= ?`, types.StepStatusPending, run.ID, types.StepReview.Order()); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &event, true, nil
}

func (d *DB) RestartAcceptedTerminal(ctx context.Context, expected *TerminalSnapshot) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runs SET id = id WHERE id = ?`, expected.Run.ID); err != nil {
		return err
	}
	current, err := readTerminalSnapshot(tx, expected.Run.ID)
	if err != nil || !reflect.DeepEqual(current, expected) {
		return fmt.Errorf("accepted recovery changed before restart")
	}
	event, err := readTerminalAdoption(tx.QueryRow(`SELECT id, event_json FROM terminal_adoptions WHERE run_id = ?`, expected.Run.ID), expected.Run.ID)
	if err != nil || event == nil || current.Run.HeadSHA != event.CandidateSHA || current.Run.Status != types.RunRunning && current.Run.Status != types.RunFailed || current.Run.PushActive || !reflect.DeepEqual(current.Rounds, event.Snapshot.Rounds) {
		return fmt.Errorf("accepted recovery cannot replay completed, cancelled or progressed validation")
	}
	bound := event.Snapshot.Run
	bound.Status, bound.HeadSHA, bound.Error, bound.UpdatedAt = current.Run.Status, event.CandidateSHA, current.Run.Error, current.Run.UpdatedAt
	bound.ReviewApprovedHeadSHA, bound.CIReadyAt, bound.TerminalHeadVerifiedAt = nil, nil, current.Run.TerminalHeadVerifiedAt
	bound.TerminalSourceState = current.Run.TerminalSourceState
	bound.AwaitingAgentSince, bound.CIReadyNoCI = nil, false
	if !reflect.DeepEqual(bound, current.Run) || !reflect.DeepEqual(current.Repo, event.Snapshot.Repo) || current.Gates != event.Snapshot.Gates {
		return fmt.Errorf("accepted recovery receipt, target, generation or source binding changed")
	}
	for _, step := range current.Steps {
		if step.StepOrder >= types.StepReview.Order() && step.Status != types.StepStatusPending {
			return fmt.Errorf("accepted recovery already started a validation turn")
		}
	}
	if _, err := tx.Exec(`UPDATE runs SET status = ?, error = NULL, terminal_head_verified_at = NULL, updated_at = ? WHERE id = ?`, types.RunRunning, now(), expected.Run.ID); err != nil {
		return err
	}
	return tx.Commit()
}
