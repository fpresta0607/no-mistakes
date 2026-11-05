package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

// ContinuationSnapshot is what a terminal adoption binds: the whole run row and
// its repository record, wider than the authored-validation TerminalSnapshot.
type ContinuationSnapshot struct {
	Run    Run
	Repo   Repo
	Gates  string
	Steps  []StepResult
	Rounds [][]StepRound
}

func (d *DB) RecordTerminalSourceState(runID, state string) error {
	_, err := d.sql.Exec(`UPDATE runs SET terminal_source_state = ? WHERE id = ? AND NOT EXISTS (SELECT 1 FROM terminal_adoptions WHERE run_id = runs.id)`, state, runID)
	return err
}

func (snapshot ContinuationSnapshot) Digest() string {
	payload, _ := json.Marshal(snapshot)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// TerminalAdoption is an operator acceptance of unvalidated source, never a
// replacement for the original launch receipt or evidence of a successful gate.
type TerminalAdoption struct {
	ID            string
	CreatedAt     int64
	Successor     Run
	SuccessorID   string
	WorktreeRoot  string
	EvidenceRoot  string
	Plan          *verificationplan.Snapshot
	Gates         string
	StepNames     []types.StepName
	Snapshot      ContinuationSnapshot
	CandidateSHA  string
	RetainedSHA   string
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

func (d *DB) ReadContinuationSnapshot(runID string) (*ContinuationSnapshot, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return readContinuationSnapshot(tx, runID)
}

func readContinuationSnapshot(tx *sql.Tx, runID string) (*ContinuationSnapshot, error) {
	history, err := readTerminalSnapshot(tx, runID)
	if err != nil {
		return nil, err
	}
	snapshot := ContinuationSnapshot{Gates: history.Gates, Steps: history.Steps, Rounds: history.Rounds}
	if err := scanRun(tx.QueryRow(`SELECT `+runColumnsFrom(tx)+` FROM runs WHERE id = ?`, runID), &snapshot.Run); err != nil {
		return nil, err
	}
	repo := &snapshot.Repo
	if err := tx.QueryRow(`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos WHERE id = ?`, snapshot.Run.RepoID).Scan(&repo.ID, &repo.WorkingPath, &repo.UpstreamURL, &repo.ForkURL, &repo.DefaultBranch, &repo.CreatedAt); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (d *DB) GetTerminalAdoption(runID string) (*TerminalAdoption, error) {
	return readTerminalAdoption(d.sql.QueryRow(`SELECT id, successor_run_id, event_json FROM terminal_adoptions WHERE run_id = ?`, runID), runID)
}

func (d *DB) GetTerminalPredecessor(runID string) (*TerminalAdoption, error) {
	return readTerminalAdoption(d.sql.QueryRow(`SELECT id, successor_run_id, event_json FROM terminal_adoptions WHERE successor_run_id = ?`, runID), "")
}

// CustodySubmittedHead is the head the operator's branch held when the run took
// custody, which every choice between the operator's head and the run's head
// must use instead of the raw submitted head. A continuation successor records
// its adopted candidate as its submitted head, but the operator stood at the
// adoption's caller head, so unless the caller stood at the candidate, the
// candidate's commits are pipeline-created and still owed to the operator.
func (d *DB) CustodySubmittedHead(run *Run) (*string, error) {
	adoption, err := d.GetTerminalPredecessor(run.ID)
	if err != nil {
		return nil, err
	}
	if adoption == nil || adoption.CallerHead == adoption.CandidateSHA {
		return run.SubmittedHeadSHA, nil
	}
	return &adoption.CallerHead, nil
}

func readTerminalAdoption(row *sql.Row, runID string) (*TerminalAdoption, error) {
	var id, successorID, payload string
	if err := row.Scan(&id, &successorID, &payload); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var event TerminalAdoption
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, err
	}
	if event.ID != id || runID != "" && event.Snapshot.Run.ID != runID || event.Successor.ID == "" || event.Successor.ID != successorID || event.SuccessorID != successorID || event.Successor.ID == event.Snapshot.Run.ID {
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
	if event.ID != "" || event.CreatedAt != 0 || !reflect.DeepEqual(event.Successor, Run{}) {
		return nil, false, fmt.Errorf("recovery cannot supply historical event identity")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO terminal_adoptions (run_id,id,event_json) SELECT '', '', '' WHERE 0`); err != nil {
		return nil, false, err
	}
	existing, err := readTerminalAdoption(tx.QueryRow(`SELECT id, successor_run_id, event_json FROM terminal_adoptions WHERE run_id = ?`, run.ID), run.ID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		comparison := *existing
		comparison.ID, comparison.CreatedAt = "", 0
		comparison.Successor = Run{}
		if event.SuccessorID == "" {
			comparison.SuccessorID = ""
		}
		if !reflect.DeepEqual(comparison, event) {
			return nil, false, fmt.Errorf("terminal adoption already has a different binding")
		}
		if err := validateTerminalSuccessor(tx, existing); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	current, err := readContinuationSnapshot(tx, run.ID)
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
	// Only the successor receives fresh launch authority and inherited public binding.
	childID, nonce, ts := event.SuccessorID, "terminal-"+newID(), now()
	if event.Plan != nil {
		if run.VerificationPlan == nil || event.Plan.SHA256 != run.VerificationPlan.SHA256 || event.SuccessorID != "" && event.SuccessorID != event.Plan.ID {
			return nil, false, fmt.Errorf("successor verification input binding changed")
		}
		if _, err := event.Plan.Read(); err != nil {
			return nil, false, err
		}
		childID = event.Plan.ID
	} else if run.VerificationPlan != nil {
		return nil, false, fmt.Errorf("successor cannot drop captured verification input")
	}
	if childID == "" {
		childID = newID()
	}
	if _, err := ulid.ParseStrict(childID); err != nil || childID <= run.ID {
		return nil, false, fmt.Errorf("invalid successor identity")
	}
	var workDir *string
	if event.WorktreeRoot != "" {
		if !filepath.IsAbs(event.WorktreeRoot) {
			return nil, false, fmt.Errorf("successor worktree root must be absolute")
		}
		resolved := filepath.Join(event.WorktreeRoot, childID)
		workDir = &resolved
	}
	var evidenceDir *string
	if event.EvidenceRoot != "" {
		resolved := filepath.Join(event.EvidenceRoot, childID)
		evidenceDir = &resolved
	}
	if _, err := tx.Exec(`INSERT INTO runs
		(id, repo_id, branch, head_sha, base_sha, submitted_head_sha, worktree_dir, no_mistakes_version, no_mistakes_build_sha, status,
		intent, intent_source, intent_session_id, intent_score, launch_nonce, launch_validation_generation, launch_intent_digest,
		launch_assertion, launch_assertion_proof, pr_base_branch, omit_intent, pi_profile, quota_routing, pr_title, pr_body, verification_plan, gates_json,
		pr_url, pr_state, pr_state_observed_at, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation,
		closing_issue_refs, closing_issue_refs_locked_at, evidence_dir, created_at, updated_at)
		SELECT ?, repo_id, branch, ?, base_sha, ?, ?, ?, ?, 'pending',
		intent, intent_source, intent_session_id, intent_score, ?, launch_validation_generation, launch_intent_digest,
		?, ?, pr_base_branch, omit_intent, pi_profile, quota_routing, pr_title, pr_body, ?, ?,
		pr_url, pr_state, pr_state_observed_at, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation,
		closing_issue_refs, closing_issue_refs_locked_at, ?, ?, ? FROM runs WHERE id = ?`,
		childID, event.CandidateSHA, event.CandidateSHA, workDir, buildinfo.CurrentVersion(), buildinfo.Commit, nonce,
		event.Assertion, event.Proof, event.Plan, event.Gates, evidenceDir, ts, ts, run.ID); err != nil {
		return nil, false, err
	}
	event.ID, event.CreatedAt, event.SuccessorID = newID(), now(), childID
	if err := scanRun(tx.QueryRow(`SELECT `+runColumnsFrom(tx)+` FROM runs WHERE id = ?`, childID), &event.Successor); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`INSERT INTO terminal_adoptions (run_id, id, successor_run_id, event_json) VALUES (?, ?, ?, ?)`, run.ID, event.ID, childID, string(payload)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &event, true, nil
}

func validateTerminalSuccessor(tx *sql.Tx, event *TerminalAdoption) error {
	parent, err := readContinuationSnapshot(tx, event.Snapshot.Run.ID)
	if err != nil || !reflect.DeepEqual(parent, &event.Snapshot) {
		return fmt.Errorf("terminal predecessor changed after acceptance")
	}
	child, err := readContinuationSnapshot(tx, event.Successor.ID)
	if err != nil || !reflect.DeepEqual(child.Run, event.Successor) || !reflect.DeepEqual(child.Repo, event.Snapshot.Repo) || child.Gates != event.Gates || len(child.Steps) != 0 {
		return fmt.Errorf("terminal successor changed, progressed or was cancelled")
	}
	var competing bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM runs WHERE repo_id = ? AND branch = ? AND id NOT IN (?, ?) AND (status IN ('pending','running') OR id > ?))`, event.Snapshot.Run.RepoID, event.Snapshot.Run.Branch, event.Snapshot.Run.ID, event.Successor.ID, event.Snapshot.Run.ID).Scan(&competing); err != nil {
		return err
	}
	if competing {
		return fmt.Errorf("another run owns this branch")
	}
	return nil
}

// ValidateTerminalSuccessor permits only the durable acceptance-to-execution window.
func (d *DB) ValidateTerminalSuccessor(event *TerminalAdoption) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateTerminalSuccessor(tx, event); err != nil {
		return err
	}

	return nil
}

type TerminalPhaseBudget struct {
	Limit     int `json:"limit"`
	Spent     int `json:"spent"`
	Remaining int `json:"remaining"`
}

// TerminalPhaseRemaining includes manual fixes and dispatched attempts in family spending.
func (d *DB) TerminalPhaseRemaining(runID string, name types.StepName, configured int) (TerminalPhaseBudget, bool, error) {
	event, err := d.GetTerminalPredecessor(runID)
	if err != nil || event == nil {
		return TerminalPhaseBudget{Limit: configured, Remaining: configured}, false, err
	}
	remaining := configured
	current, err := d.ReadContinuationSnapshot(runID)
	if err != nil {
		return TerminalPhaseBudget{}, true, err
	}
	snapshots := []ContinuationSnapshot{*current}
	seen := map[string]bool{runID: true}
	for event != nil {
		if seen[event.Snapshot.Run.ID] {
			return TerminalPhaseBudget{}, true, fmt.Errorf("terminal predecessor cycle")
		}
		seen[event.Snapshot.Run.ID] = true
		original, err := d.ReadContinuationSnapshot(event.Snapshot.Run.ID)
		if err != nil {
			return TerminalPhaseBudget{}, true, fmt.Errorf("terminal predecessor history changed")
		}
		// The repository record is registration, not run history: re-registering
		// may change its remotes or default branch while the successor runs.
		original.Repo = event.Snapshot.Repo
		if !reflect.DeepEqual(original, &event.Snapshot) {
			return TerminalPhaseBudget{}, true, fmt.Errorf("terminal predecessor history changed")
		}
		snapshots = append(snapshots, event.Snapshot)
		event, err = d.GetTerminalPredecessor(event.Snapshot.Run.ID)
		if err != nil {
			return TerminalPhaseBudget{}, true, err
		}
	}
	spent := 0
	for _, snapshot := range snapshots {
		hasPhase := false
		for index, step := range snapshot.Steps {
			if step.StepName != name {
				continue
			}
			hasPhase = true
			if snapshot.Run.ID != runID && step.StartedAt != nil {
				limit := 0
				if step.AutoFixLimit != nil {
					limit = *step.AutoFixLimit
				}
				remaining = min(remaining, limit)
			}
			pending := false
			for _, round := range snapshot.Rounds[index] {
				if round.IsFixRound() {
					spent++
					pending = false
				}
				if round.SelectionSource != nil && (*round.SelectionSource == RoundSelectionSourceAutoFix || *round.SelectionSource == RoundSelectionSourceUser) {
					pending = true
				}
			}
			if pending {
				spent++
			}
		}
		if !hasPhase && snapshot.Run.ID != runID {
			remaining = 0
		}
	}
	return TerminalPhaseBudget{Limit: remaining, Spent: spent, Remaining: max(0, remaining-spent)}, true, nil
}
