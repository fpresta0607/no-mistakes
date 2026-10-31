package db

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CIRepairAdoption records an operator's authorization to validate retained
// work. Its original snapshots are history, never approval of the candidate.
type CIRepairAdoption struct {
	ID                string
	CreatedAt         int64
	CandidateSHA      string
	CandidateTreeSHA  string
	Operator          string
	Reason            string
	CallerDir         string
	CallerHeadSHA     string
	ManagedDir        string
	GateDir           string
	GateBranch        string
	ExpectedRepo      Repo
	ExpectedRun       Run
	ExpectedGatesJSON string
	Steps             []CIRepairAdoptionStep
}

type CIRepairAdoptionStep struct {
	Snapshot              StepResult
	Rounds                []StepRound
	EffectiveAutoFixLimit int
}

func (d *DB) GetCIRepairAdoption(runID string) (*CIRepairAdoption, error) {
	return readCIRepairAdoption(d.sql.QueryRow(`SELECT run_id, id, event_json, created_at FROM ci_repair_adoptions WHERE run_id = ?`, runID))
}

// AdoptCIRepairForRevalidation atomically accepts one exact operator request.
// Replaying it returns the immutable event without renewing validation or budgets.
// Git objects, forge truth, and filesystem ownership must be checked by the caller.
func (d *DB) AdoptCIRepairForRevalidation(adoption CIRepairAdoption) (*CIRepairAdoption, bool, error) {
	adoption.ExpectedRepo.URLsVerified = false
	if err := validateCIRepairAdoptionRequest(adoption); err != nil {
		return nil, false, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin CI repair adoption: %w", err)
	}
	defer tx.Rollback()

	// Acquire SQLite's writer reservation before reading the CAS snapshot, so
	// separate connections serialize identical requests instead of racing a
	// deferred read transaction's upgrade. A refusal rolls this write back too.
	if _, err := tx.Exec(`UPDATE runs SET id = id WHERE id = ?`, adoption.ExpectedRun.ID); err != nil {
		return nil, false, fmt.Errorf("reserve CI repair adoption: %w", err)
	}
	existing, err := readCIRepairAdoption(tx.QueryRow(`SELECT run_id, id, event_json, created_at FROM ci_repair_adoptions WHERE run_id = ?`, adoption.ExpectedRun.ID))
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		expected := *existing
		expected.ID, expected.CreatedAt = adoption.ID, adoption.CreatedAt
		if !reflect.DeepEqual(expected, adoption) || (adoption.ID != "" && adoption.ID != existing.ID) || (adoption.CreatedAt != 0 && adoption.CreatedAt != existing.CreatedAt) {
			return nil, false, fmt.Errorf("CI repair adoption for run %s already has a different binding", adoption.ExpectedRun.ID)
		}
		return existing, false, nil
	}
	if adoption.ID != "" || adoption.CreatedAt != 0 {
		return nil, false, fmt.Errorf("new CI repair adoption cannot supply historical event identity or time")
	}
	if err := compareCIRepairAdoptionSnapshot(tx, adoption); err != nil {
		return nil, false, err
	}

	adoption.ID, adoption.CreatedAt = newID(), now()
	payload, err := json.Marshal(adoption)
	if err != nil {
		return nil, false, fmt.Errorf("encode CI repair adoption: %w", err)
	}
	auditResult, err := tx.Exec(`INSERT INTO ci_repair_adoptions (run_id, id, event_json, created_at) VALUES (?, ?, ?, ?)`, adoption.ExpectedRun.ID, adoption.ID, string(payload), adoption.CreatedAt)
	if err != nil {
		return nil, false, fmt.Errorf("record CI repair adoption: %w", err)
	}
	if changed, err := auditResult.RowsAffected(); err != nil || changed != 1 {
		return nil, false, fmt.Errorf("CI repair adoption did not append exactly one audit event")
	}
	runResult, err := tx.Exec(`UPDATE runs SET head_sha = ?, review_approved_head_sha = NULL, ci_ready_at = NULL, ci_ready_no_ci = 0,
		error = NULL, terminal_head_verified_at = NULL,
		parked_ms = COALESCE(parked_ms, 0) + CASE WHEN ? > awaiting_agent_since THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
		awaiting_agent_since = NULL, updated_at = ? WHERE id = ?`,
		adoption.CandidateSHA, adoption.CreatedAt, adoption.CreatedAt, adoption.CreatedAt, adoption.ExpectedRun.ID)
	if err != nil {
		return nil, false, fmt.Errorf("revoke CI repair validation authority: %w", err)
	}
	if changed, err := runResult.RowsAffected(); err != nil || changed != 1 {
		return nil, false, fmt.Errorf("CI repair adoption did not update exactly one run")
	}
	// Ordinary ResetStepsFrom deliberately clears auto_fix_limit. Adoption
	// retains the exact original numeric limits in these rows and in its audit.
	stepResult, err := tx.Exec(`UPDATE step_results SET status = ?, exit_code = NULL, duration_ms = NULL, log_path = NULL,
		findings_json = NULL, error = NULL, started_at = NULL, round_started_at = NULL, completed_at = NULL,
		last_activity_at = NULL, last_activity = NULL, agent_pid = NULL, override_reason = NULL, approval_reason = NULL, skip_reason = NULL
		WHERE run_id = ? AND step_order >= ?`, types.StepStatusPending, adoption.ExpectedRun.ID, types.StepReview.Order())
	if err != nil {
		return nil, false, fmt.Errorf("reset adopted CI repair validation: %w", err)
	}
	var requiredCount int64
	for _, original := range adoption.Steps {
		if original.Snapshot.StepOrder >= types.StepReview.Order() {
			requiredCount++
		}
	}
	if changed, err := stepResult.RowsAffected(); err != nil || changed != requiredCount {
		return nil, false, fmt.Errorf("CI repair adoption did not reset every required step")
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit CI repair adoption: %w", err)
	}
	return &adoption, true, nil
}

func readCIRepairAdoption(row *sql.Row) (*CIRepairAdoption, error) {
	var runID, id, payload string
	var createdAt int64
	if err := row.Scan(&runID, &id, &payload, &createdAt); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read CI repair adoption: %w", err)
	}
	var adoption CIRepairAdoption
	if err := json.Unmarshal([]byte(payload), &adoption); err != nil {
		return nil, fmt.Errorf("decode CI repair adoption: %w", err)
	}
	if adoption.ExpectedRun.ID != runID || adoption.ID != id || adoption.CreatedAt != createdAt {
		return nil, fmt.Errorf("CI repair adoption audit binding is inconsistent")
	}
	return &adoption, nil
}

func validateCIRepairAdoptionRequest(adoption CIRepairAdoption) error {
	for _, value := range []string{adoption.Operator, adoption.Reason, adoption.CallerDir, adoption.ManagedDir, adoption.GateDir, adoption.GateBranch, adoption.ExpectedRepo.ID, adoption.ExpectedRun.ID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("CI repair adoption requires operator, reason, caller, managed/gate directories, branch, repository, and run")
		}
	}
	for _, value := range []string{adoption.CandidateSHA, adoption.CandidateTreeSHA, adoption.CallerHeadSHA, adoption.ExpectedRun.HeadSHA} {
		if len(value) != 40 && len(value) != 64 {
			return fmt.Errorf("CI repair adoption requires full commit and tree object IDs")
		}
		if _, err := hex.DecodeString(value); err != nil {
			return fmt.Errorf("CI repair adoption object ID is malformed: %w", err)
		}
	}
	if adoption.CandidateSHA == adoption.ExpectedRun.HeadSHA || adoption.ExpectedRun.RepoID != adoption.ExpectedRepo.ID || adoption.ExpectedRun.Branch != adoption.GateBranch {
		return fmt.Errorf("CI repair adoption candidate or repository/branch binding is invalid")
	}
	if len(adoption.Steps) == 0 {
		return fmt.Errorf("CI repair adoption requires complete original step snapshots")
	}
	return nil
}

func compareCIRepairAdoptionSnapshot(tx *sql.Tx, adoption CIRepairAdoption) error {
	var run Run
	if err := scanRun(tx.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, adoption.ExpectedRun.ID), &run); err != nil {
		return fmt.Errorf("read CI repair run: %w", err)
	}
	if !reflect.DeepEqual(run, adoption.ExpectedRun) {
		return fmt.Errorf("CI repair run snapshot changed")
	}
	if run.Status != types.RunRunning || run.PRState == nil || *run.PRState != "open" || run.PRStateObservedAt == nil || run.PRURL == nil || strings.TrimSpace(*run.PRURL) == "" || run.CustodyReturnedAt != nil || run.PushActive || run.AwaitingAgentSince == nil || run.TerminalHeadVerifiedAt != nil {
		return fmt.Errorf("CI repair adoption requires an active parked run with an observed open PR and pipeline custody")
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != run.HeadSHA || run.PushGeneration == nil || *run.PushGeneration <= 0 || run.LastPushedAt == nil || run.PushTargetKind == nil || strings.TrimSpace(*run.PushTargetKind) == "" || run.PushTargetFingerprint == nil || strings.TrimSpace(*run.PushTargetFingerprint) == "" || run.PushRef == nil || strings.TrimSpace(*run.PushRef) == "" {
		return fmt.Errorf("CI repair adoption requires the exact original publication binding")
	}
	if run.WorktreeDir != nil && *run.WorktreeDir != adoption.ManagedDir {
		return fmt.Errorf("CI repair adoption managed directory changed")
	}
	var repo Repo
	if err := tx.QueryRow(`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos WHERE id = ?`, run.RepoID).Scan(
		&repo.ID, &repo.WorkingPath, &repo.UpstreamURL, &repo.ForkURL, &repo.DefaultBranch, &repo.CreatedAt); err != nil {
		return fmt.Errorf("read CI repair repository: %w", err)
	}
	if !reflect.DeepEqual(repo, adoption.ExpectedRepo) {
		return fmt.Errorf("CI repair repository snapshot changed")
	}
	var gates sql.NullString
	if err := tx.QueryRow(`SELECT gates_json FROM runs WHERE id = ?`, run.ID).Scan(&gates); err != nil {
		return fmt.Errorf("read CI repair gate pin: %w", err)
	}
	if gates.String != adoption.ExpectedGatesJSON {
		return fmt.Errorf("CI repair gate pin changed")
	}
	var competingRun, activeAgent bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM runs WHERE repo_id = ? AND branch = ? AND id != ? AND status IN ('pending', 'running'))`, run.RepoID, run.Branch, run.ID).Scan(&competingRun); err != nil {
		return fmt.Errorf("read competing CI repair runs: %w", err)
	}
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM step_results WHERE agent_pid IS NOT NULL AND agent_pid != 0)`).Scan(&activeAgent); err != nil {
		return fmt.Errorf("read CI repair agent ownership: %w", err)
	}
	if competingRun || activeAgent {
		return fmt.Errorf("CI repair adoption refused while another run or agent owns work")
	}
	steps, err := readCIRepairAdoptionSteps(tx, run.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(steps, adoption.Steps) {
		return fmt.Errorf("CI repair step snapshots, limits, or round history changed")
	}
	return validateCIRepairAdoptionSteps(steps, gates.String)
}

func readCIRepairAdoptionSteps(tx *sql.Tx, runID string) ([]CIRepairAdoptionStep, error) {
	rows, err := tx.Query(`SELECT `+stepResultColumns+`, round_started_at, override_reason, skip_reason, approval_reason FROM step_results WHERE run_id = ? ORDER BY step_order, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("read CI repair step snapshots: %w", err)
	}
	defer rows.Close()
	var steps []CIRepairAdoptionStep
	for rows.Next() {
		var step StepResult
		if err := rows.Scan(&step.ID, &step.RunID, &step.StepName, &step.StepOrder, &step.Status, &step.ExitCode, &step.DurationMS, &step.LogPath, &step.FindingsJSON, &step.Error, &step.StartedAt, &step.CompletedAt, &step.LastActivityAt, &step.LastActivity, &step.AgentPID, &step.AutoFixLimit, &step.RoundStartedAt, &step.OverrideReason, &step.SkipReason, &step.ApprovalReason); err != nil {
			return nil, fmt.Errorf("scan CI repair step snapshot: %w", err)
		}
		snapshot := CIRepairAdoptionStep{Snapshot: step}
		if step.AutoFixLimit != nil {
			snapshot.EffectiveAutoFixLimit = *step.AutoFixLimit
		}
		steps = append(steps, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate CI repair step snapshots: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close CI repair step snapshots: %w", err)
	}
	for index := range steps {
		rounds, err := tx.Query(`SELECT id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, repair_published, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round, id`, steps[index].Snapshot.ID)
		if err != nil {
			return nil, fmt.Errorf("read CI repair original rounds: %w", err)
		}
		for rounds.Next() {
			var round StepRound
			if err := rounds.Scan(&round.ID, &round.StepResultID, &round.Round, &round.Trigger, &round.FindingsJSON, &round.ReviewedHeadSHA, &round.StartingHeadSHA, &round.TrustedConfigSHA, &round.GlobalConfigYAML, &round.RepoConfigYAML, &round.UserFindingsJSON, &round.SelectedFindingIDs, &round.SelectionSource, &round.FixSummary, &round.RepairPublished, &round.DurationMS, &round.CreatedAt); err != nil {
				rounds.Close()
				return nil, fmt.Errorf("scan CI repair original round: %w", err)
			}
			steps[index].Rounds = append(steps[index].Rounds, round)
		}
		if err := rounds.Err(); err != nil {
			rounds.Close()
			return nil, fmt.Errorf("iterate CI repair original rounds: %w", err)
		}
		if err := rounds.Close(); err != nil {
			return nil, fmt.Errorf("close CI repair original rounds: %w", err)
		}
	}
	return steps, nil
}

func validateCIRepairAdoptionSteps(steps []CIRepairAdoptionStep, gatePin string) error {
	gates, err := config.ParseGates(gatePin)
	if err != nil {
		return fmt.Errorf("CI repair adoption gate history is unreadable: %w", err)
	}
	var names []types.StepName
	for _, name := range types.AllSteps() {
		names = append(names, name)
		for _, gate := range gates {
			if gate.After == name {
				names = append(names, gate.StepName())
			}
		}
	}
	if len(steps) != len(names) {
		return fmt.Errorf("CI repair adoption requires the complete original step sequence")
	}
	for index, original := range steps {
		step := original.Snapshot
		if step.StepName != names[index] || step.StepOrder != names[index].Order() || original.EffectiveAutoFixLimit < 0 {
			return fmt.Errorf("CI repair adoption step order or original limit is invalid")
		}
		if step.StepName == types.StepIntent && step.Status == types.StepStatusSkipped {
			continue
		}
		if step.StepName == types.StepCI {
			if step.Status != types.StepStatusAwaitingApproval && step.Status != types.StepStatusFixReview {
				return fmt.Errorf("CI repair adoption requires the original CI approval gate")
			}
		} else if step.Status != types.StepStatusCompleted {
			return fmt.Errorf("CI repair adoption refuses skipped, incomplete, or active required step %s", step.StepName)
		}
		if len(original.Rounds) == 0 {
			return fmt.Errorf("CI repair adoption is missing original round history for %s", step.StepName)
		}
		for roundIndex, round := range original.Rounds {
			if round.Round != roundIndex+1 || round.StepResultID != step.ID || round.ID == "" {
				return fmt.Errorf("CI repair adoption round history is ambiguous for %s", step.StepName)
			}
			switch round.Trigger {
			case "initial", "auto_fix", "user_fix", "answer":
			default:
				return fmt.Errorf("CI repair adoption round trigger is unknown for %s", step.StepName)
			}
			if (round.SelectedFindingIDs == nil) != (round.SelectionSource == nil) {
				return fmt.Errorf("CI repair adoption selection history is incomplete for %s", step.StepName)
			}
			if round.SelectedFindingIDs != nil {
				var selected []string
				if err := json.Unmarshal([]byte(*round.SelectedFindingIDs), &selected); err != nil || selected == nil {
					return fmt.Errorf("CI repair adoption selection history is unreadable for %s", step.StepName)
				}
				switch *round.SelectionSource {
				case RoundSelectionSourceUser, RoundSelectionSourceAutoFix, RoundSelectionSourceUserDeclined:
				default:
					return fmt.Errorf("CI repair adoption selection source is unknown for %s", step.StepName)
				}
			}
		}
	}
	ci := steps[len(steps)-1]
	latest := ci.Rounds[len(ci.Rounds)-1]
	if !latest.IsFixRound() || latest.RepairPublished || latest.FindingsJSON == nil || ci.Snapshot.FindingsJSON == nil {
		return fmt.Errorf("CI repair adoption requires the exact unpublished repair round and CI findings")
	}
	if _, err := types.ParseFindingsJSON(*ci.Snapshot.FindingsJSON); err != nil {
		return fmt.Errorf("CI repair adoption CI findings are unreadable: %w", err)
	}
	if _, err := types.ParseFindingsJSON(*latest.FindingsJSON); err != nil {
		return fmt.Errorf("CI repair adoption latest-round findings are unreadable: %w", err)
	}
	if len(ci.Rounds) < 2 || !hasSelectedFinding(ci.Rounds[len(ci.Rounds)-2].SelectedFindingIDs) {
		return fmt.Errorf("CI repair adoption is missing the original repair selection")
	}
	return nil
}
