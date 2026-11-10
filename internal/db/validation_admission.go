package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

type ValidationAdmission struct {
	ID             string
	CreatedAt      int64
	ChildID        string
	Request        authoredvalidation.Request
	Snapshot       TerminalSnapshot
	InitialChild   Run
	AssertionProof *launchassert.Proof
	WorktreeRoot   string
	EvidenceRoot   string
	Plan           *verificationplan.Snapshot
	Gates          string
	Steps          []types.StepName
	Operator       string
}

func (snapshot TerminalSnapshot) ValidateAuthoredRequest(request authoredvalidation.Request) error {
	parent := &snapshot.Run
	if err := request.Validate(); err != nil {
		return err
	}
	if request.SnapshotDigest != snapshot.Digest() || request.PredecessorID != parent.ID || request.RepoID != parent.RepoID || request.Branch != parent.Branch {
		return fmt.Errorf("authored admission predecessor snapshot or repository binding changed")
	}
	if !parent.Status.Terminal() || (parent.Status == types.RunCompleted && !snapshot.IsAuthoredChild) || parent.PushActive || parent.LaunchNonce == nil || parent.LaunchValidationGeneration == nil || parent.LaunchIntentDigest == nil || parent.LaunchAssertion == nil {
		return fmt.Errorf("authored admission requires a terminal predecessor that did not complete, or a completed authored child, with its original launch receipt and no active publisher")
	}
	if err := snapshot.validateNativeRefusals(); err != nil {
		return err
	}
	if request.TerminalGeneration != "" {
		if request.CandidateSHA != parent.HeadSHA || parent.Status == types.RunCompleted {
			return fmt.Errorf("terminal validation requires the exact uncompleted predecessor head")
		}
	} else if request.CandidateSHA == parent.HeadSHA || parent.SubmittedHeadSHA != nil && request.CandidateSHA == *parent.SubmittedHeadSHA || parent.LastPushedSHA != nil && request.CandidateSHA == *parent.LastPushedSHA {
		return fmt.Errorf("authored admission requires a newly authored candidate, not a submitted, terminal or published predecessor head")
	}
	if request.LaunchNonce != *parent.LaunchNonce || request.ValidationGeneration != *parent.LaunchValidationGeneration || request.IntentDigest != *parent.LaunchIntentDigest {
		return fmt.Errorf("authored admission original launch binding changed")
	}
	if parent.Intent == nil || fmt.Sprintf("%x", sha256.Sum256([]byte(*parent.Intent))) != request.IntentDigest {
		return fmt.Errorf("terminal intent no longer matches its original launch receipt")
	}
	if err := parent.LaunchAssertionProof.Check(parent.LaunchAssertion); err != nil {
		return err
	}
	expected := *request.LaunchAssertion
	expected.TrustedSHA = parent.LaunchAssertion.TrustedSHA
	if request.TerminalGeneration == "" && !expected.Matches(parent.LaunchAssertion) {
		return fmt.Errorf("authored admission cannot change the original agent selection")
	}
	if parent.LastPushedSHA == nil {
		if request.PublishedHeadSHA != "" || request.PushGeneration != 0 || request.PushTargetFingerprint != "" || parent.PRURL != nil || parent.PushGeneration != nil {
			return fmt.Errorf("authored admission publication binding is incomplete")
		}
	} else if parent.PushGeneration == nil || parent.PushTargetFingerprint == nil || request.PublishedHeadSHA != *parent.LastPushedSHA || request.PushGeneration != *parent.PushGeneration || request.PushTargetFingerprint != *parent.PushTargetFingerprint {
		return fmt.Errorf("authored admission publication head, target or generation changed")
	}
	return nil
}

func (snapshot TerminalSnapshot) validateNativeRefusals() error {
	for _, step := range snapshot.Steps {
		if step.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
		if err != nil {
			return fmt.Errorf("authored admission cannot inspect predecessor %s findings: %w", step.StepName, err)
		}
		for _, finding := range findings.Items {
			if finding.ID == types.FindingIDAgentSafetyRefusal {
				return fmt.Errorf("authored admission refuses a predecessor with an unresolved native agent safety refusal in %s", step.StepName)
			}
		}
	}
	return nil
}

func validateAdmissionBinding(event *ValidationAdmission) error {
	if err := event.Snapshot.ValidateAuthoredRequest(event.Request); err != nil {
		return err
	}
	if err := event.AssertionProof.Check(event.Request.LaunchAssertion); err != nil {
		return err
	}
	if !filepath.IsAbs(event.WorktreeRoot) || event.Operator == "" {
		return fmt.Errorf("authored admission needs a registered worktree placement and authenticated operator")
	}
	return nil
}

func readValidationAdmission(row *sql.Row) (*ValidationAdmission, error) {
	var id, parentID, childID, digest, payload string
	if err := row.Scan(&id, &parentID, &childID, &digest, &payload); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var event ValidationAdmission
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, err
	}
	if event.ID != id || event.Request.PredecessorID != parentID || event.ChildID != childID || event.InitialChild.ID != childID || parentID == childID || event.Request.Digest() != digest {
		return nil, fmt.Errorf("authored admission identity is inconsistent")
	}
	if err := validateAdmissionBinding(&event); err != nil {
		return nil, err
	}
	return &event, nil
}

const validationAdmissionColumns = `id, predecessor_id, child_id, request_digest, event_json`

func (d *DB) GetValidationAdmission(predecessorID string) (*ValidationAdmission, error) {
	return readValidationAdmission(d.sql.QueryRow(`SELECT `+validationAdmissionColumns+` FROM validation_admissions WHERE predecessor_id = ?`, predecessorID))
}

func (d *DB) GetValidationPredecessor(childID string) (*ValidationAdmission, error) {
	return readValidationAdmission(d.sql.QueryRow(`SELECT `+validationAdmissionColumns+` FROM validation_admissions WHERE child_id = ?`, childID))
}

// AcceptValidationAdmission records new launch authority only on a fresh child.
// The write transaction compares the entire predecessor before making either row.
func (d *DB) AcceptValidationAdmission(ctx context.Context, proposal ValidationAdmission) (*ValidationAdmission, bool, error) {
	if len(proposal.Request.SourceAnchors) == 0 {
		proposal.Request.SourceAnchors = nil
	}
	if err := validateAdmissionBinding(&proposal); err != nil {
		return nil, false, err
	}
	if proposal.ID != "" || proposal.ChildID != "" || proposal.CreatedAt != 0 || !reflect.DeepEqual(proposal.InitialChild, Run{}) {
		return nil, false, fmt.Errorf("authored admission cannot supply historical event identity")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	// Acquire SQLite's write reservation before checking for a competing launch.
	if _, err := tx.Exec(`INSERT INTO validation_admissions (predecessor_id, id, child_id, request_digest, event_json) SELECT '', '', '', '', '' WHERE 0`); err != nil {
		return nil, false, err
	}
	current, err := readTerminalSnapshot(tx, proposal.Request.PredecessorID)
	if err != nil || !reflect.DeepEqual(current, &proposal.Snapshot) {
		return nil, false, fmt.Errorf("terminal predecessor history changed before authored admission")
	}
	var competing bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM runs WHERE repo_id = ? AND branch = ? AND id <> ? AND (status IN ('pending','running') OR id > ?))`, current.Run.RepoID, current.Run.Branch, current.Run.ID, current.Run.ID).Scan(&competing); err != nil {
		return nil, false, err
	}
	if competing {
		return nil, false, fmt.Errorf("a newer or active run owns this branch")
	}
	family, err := readValidationFamily(tx, current.Run.ID)
	if err != nil {
		return nil, false, err
	}
	if family != nil && (family.HasChildRepairs || !reflect.DeepEqual(family.ControllerPolicy, proposal.Request.ControllerPolicy) || family.HistoricalSourceCompleteness != proposal.Request.HistoricalSourceCompleteness) {
		return nil, false, fmt.Errorf("authored descendants cannot renew repairs, change the frozen controller assertion or certify missing source")
	}
	if proposal.Request.TerminalGeneration == "" {
		history, err := readValidationHistory(tx, current.Run.ID)
		if err != nil {
			return nil, false, err
		}
		for _, entry := range history {
			run := entry.Snapshot.Run
			heads := []*string{&run.HeadSHA, run.SubmittedHeadSHA, run.LastPushedSHA, run.ReviewApprovedHeadSHA}
			if entry.Admission != nil {
				for _, head := range entry.Admission.Request.SourceAnchors {
					heads = append(heads, &head)
				}
			}
			if entry.Adoption != nil {
				adoption := entry.Adoption
				heads = append(heads, &adoption.CandidateSHA, &adoption.RetainedSHA, &adoption.GateHead, &adoption.RecoveryHead, &adoption.CallerHead)
			}
			for _, rounds := range entry.Snapshot.Rounds {
				for _, round := range rounds {
					heads = append(heads, round.StartingHeadSHA, round.ReviewedHeadSHA)
				}
			}
			for _, head := range heads {
				if head != nil && proposal.Request.CandidateSHA == *head {
					return nil, false, fmt.Errorf("authored admission requires a newly authored candidate, not a head bound in the validation family; use terminal validation for an unchanged terminal candidate")
				}
			}
		}
	}
	childID, timestamp := newID(), now()
	if proposal.Plan != nil {
		if current.Run.VerificationPlan == nil || proposal.Plan.SHA256 != current.Run.VerificationPlan.SHA256 {
			return nil, false, fmt.Errorf("authored admission verification input changed")
		}
		if _, err := proposal.Plan.Read(); err != nil {
			return nil, false, err
		}
		childID = proposal.Plan.ID
	} else if current.Run.VerificationPlan != nil {
		return nil, false, fmt.Errorf("authored admission cannot drop the original verification input")
	}
	workDir, nonce := filepath.Join(proposal.WorktreeRoot, childID), "authored-"+childID
	generation := proposal.Request.SuccessorGeneration()
	piProfile, quotaRouting := current.Run.PiProfile, current.Run.QuotaRouting
	if proposal.Request.TerminalGeneration != "" {
		piProfile, quotaRouting = nil, ""
	}
	var evidenceDir *string
	if proposal.EvidenceRoot != "" {
		resolved := filepath.Join(proposal.EvidenceRoot, childID)
		evidenceDir = &resolved
	}
	if _, err := tx.Exec(`INSERT INTO runs
		(id, repo_id, branch, head_sha, base_sha, submitted_head_sha, worktree_dir, no_mistakes_version, no_mistakes_build_sha, status,
		intent, intent_source, intent_session_id, intent_score, intent_check_json, launch_nonce, launch_validation_generation, launch_intent_digest,
		launch_assertion, launch_assertion_proof, pr_base_branch, omit_intent, pi_profile, quota_routing, pr_title, pr_body, verification_plan, gates_json,
		pr_url, pr_state, pr_state_observed_at, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation,
		closing_issue_refs, closing_issue_refs_locked_at, evidence_dir, created_at, updated_at)
		SELECT ?, repo_id, branch, ?, base_sha, ?, ?, ?, ?, 'pending',
		intent, intent_source, intent_session_id, intent_score, intent_check_json, ?, ?, launch_intent_digest,
		?, ?, pr_base_branch, omit_intent, ?, ?, pr_title, pr_body, ?, ?,
		pr_url, pr_state, pr_state_observed_at, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation,
		closing_issue_refs, closing_issue_refs_locked_at, ?, ?, ? FROM runs WHERE id = ?`,
		childID, proposal.Request.CandidateSHA, proposal.Request.CandidateSHA, workDir, buildinfo.CurrentVersion(), buildinfo.Commit, nonce, generation,
		proposal.Request.LaunchAssertion, proposal.AssertionProof, piProfile, quotaRouting, proposal.Plan, proposal.Gates, evidenceDir, timestamp, timestamp, current.Run.ID); err != nil {
		return nil, false, err
	}
	proposal.ID, proposal.CreatedAt, proposal.ChildID = newID(), timestamp, childID
	child, err := readTerminalSnapshot(tx, childID)
	if err != nil {
		return nil, false, err
	}
	proposal.InitialChild = child.Run
	payload, err := json.Marshal(proposal)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`INSERT INTO validation_admissions (id, predecessor_id, child_id, request_digest, event_json) VALUES (?, ?, ?, ?, ?)`, proposal.ID, current.Run.ID, childID, proposal.Request.Digest(), string(payload)); err != nil {
		return nil, false, err
	}
	family, err = readValidationFamily(tx, childID)
	if err != nil {
		return nil, false, err
	}
	if family == nil || family.HasChildRepairs {
		return nil, false, fmt.Errorf("validation-only admission has conflicting inherited repair history")
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &proposal, true, nil
}

// ValidatePendingAdmission permits replay only in the untouched acceptance window.
func (d *DB) ValidatePendingAdmission(event *ValidationAdmission) error {
	snapshot, err := d.ReadTerminalSnapshot(event.ChildID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(snapshot.Run, event.InitialChild) || len(snapshot.Steps) != 0 || snapshot.Gates != event.Gates {
		return fmt.Errorf("authored child progressed or was cancelled; it cannot be started again")
	}
	_, err = d.ValidationFamily(event.ChildID)
	return err
}

// RefuseUnlinkedValidation prevents ordinary run/rerun from dropping family authority.
// The lineage ends once its newest authored child is terminal with a merged or
// closed PR: authored admission requires an open PR, so nothing can continue it.
func (d *DB) RefuseUnlinkedValidation(repoID, branch string) error {
	var status, prState string
	err := d.sql.QueryRow(`SELECT r.status, COALESCE(r.pr_state, '') FROM validation_admissions a JOIN runs r ON r.id = a.child_id WHERE r.repo_id = ? AND r.branch = ? ORDER BY r.id DESC LIMIT 1`, repoID, branch).Scan(&status, &prState)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if types.RunStatus(status).Terminal() && terminalPRState(prState) {
		return nil
	}
	return fmt.Errorf("this branch has validation-only authored lineage; reattach its active child or use explicit authored admission for a terminal child")
}

func (d *DB) PendingValidationAdmissions() ([]*ValidationAdmission, error) {
	rows, err := d.sql.Query(`SELECT a.child_id FROM validation_admissions a JOIN runs r ON r.id = a.child_id WHERE r.status = 'pending' ORDER BY a.child_id`)
	if err != nil {
		return nil, err
	}
	var identifiers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		identifiers = append(identifiers, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	events := make([]*ValidationAdmission, 0, len(identifiers))
	for _, id := range identifiers {
		event, err := d.GetValidationPredecessor(id)
		if err != nil {
			return nil, err
		}
		if event == nil {
			return nil, fmt.Errorf("pending authored admission disappeared")
		}
		events = append(events, event)
	}
	return events, nil
}

func (d *DB) CancelPendingValidationAdmission(runID string) error {
	event, err := d.GetValidationPredecessor(runID)
	if err != nil {
		return err
	}
	if event == nil {
		return fmt.Errorf("no active run %s", runID)
	}
	if err := d.ValidatePendingAdmission(event); err != nil {
		return err
	}
	result, err := d.sql.Exec(`UPDATE runs SET status = ?, error = ?, updated_at = ? WHERE id = ? AND status = 'pending' AND NOT EXISTS(SELECT 1 FROM step_results WHERE run_id = runs.id)`, types.RunCancelled, types.RunCancelReasonAbortedByUser, now(), runID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("pending authored admission changed before cancellation")
	}
	return nil
}
