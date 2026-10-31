package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReadCIRepairSnapshot captures the original inputs to the adoption CAS.
// The transaction independently reloads every value before accepting them.
func ReadCIRepairSnapshot(database *db.DB, runID string) (*db.CIRepairAdoption, error) {
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read CI repair run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("CI repair run is missing")
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("read CI repair repository: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("CI repair repository is missing")
	}
	pin, err := database.GetRunGates(runID)
	if err != nil {
		return nil, err
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, err
	}
	snapshot := &db.CIRepairAdoption{ExpectedRepo: *repo, ExpectedRun: *run, ExpectedGatesJSON: pin}
	for _, step := range steps {
		original := db.CIRepairAdoptionStep{Snapshot: *step}
		if step.AutoFixLimit != nil {
			original.EffectiveAutoFixLimit = *step.AutoFixLimit
		}
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			return nil, err
		}
		for _, round := range rounds {
			original.Rounds = append(original.Rounds, *round)
		}
		snapshot.Steps = append(snapshot.Steps, original)
	}
	return snapshot, nil
}

func CIRepairSnapshotDigest(snapshot *db.CIRepairAdoption) (string, error) {
	data, err := json.Marshal(struct {
		Repo  db.Repo
		Run   db.Run
		Gates string
		Steps []db.CIRepairAdoptionStep
	}{snapshot.ExpectedRepo, snapshot.ExpectedRun, snapshot.ExpectedGatesJSON, snapshot.Steps})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// RetainedCIHead is a preservation test, never publication provenance.
// A failed read cannot release a run that still owns an unpublished head.
func RetainedCIHead(ctx context.Context, run *db.Run, workDir string) (bool, error) {
	if run == nil || run.LastPushedSHA == nil || run.PushGeneration == nil || *run.PushGeneration <= 0 {
		return false, nil
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return true, fmt.Errorf("cannot read retained CI head; preserve the source")
	}
	return head != run.HeadSHA, nil
}

// AdoptCIRepair reserves an existing CI gate while the caller proves and
// atomically adopts its exact snapshot. Ordinary responses and reconciliation
// cannot claim the gate between the proof and the durable transition.
func (e *Executor) AdoptCIRepair(adopt func() (*db.CIRepairAdoption, error), publication *forgecontext.Context, login string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.waiting || e.waitingStep != types.StepCI {
		return fmt.Errorf("CI repair adoption requires an exclusively parked CI executor")
	}
	e.ciAdoptionReserved = true
	event, err := adopt()
	if err != nil {
		e.ciAdoptionReserved = false
		return err
	}
	e.waiting = false
	e.approvalCh <- approvalResponse{action: types.ActionRevalidate, adoption: event, publication: publication, publicationLogin: login}
	return nil
}

func (e *Executor) ReselectPublication(publication *forgecontext.Context, login string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.adoption == nil || !e.waiting || e.waitingStep != types.StepPush || !e.publicationSelectionRequired {
		return fmt.Errorf("publication credential reselection requires the preserved Push credential gate")
	}
	results, err := e.db.GetStepsByRun(e.runID)
	if err != nil {
		return err
	}
	isSelectionGate := false
	for _, result := range results {
		if result.StepName == types.StepPush && result.FindingsJSON != nil && hasFindingID(*result.FindingsJSON, PublicationSelectionFindingID) {
			isSelectionGate = true
		}
	}
	if !isSelectionGate {
		return fmt.Errorf("fresh validation is required; credential reselection cannot authorize it")
	}
	e.waiting = false
	e.approvalCh <- approvalResponse{action: types.ActionRevalidate, publication: publication, publicationLogin: login}
	return nil
}

func (e *Executor) acceptCIAdoption(run *db.Run, event *db.CIRepairAdoption, publication *forgecontext.Context, login string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.adoption = event
	e.publicationForge, e.publicationLogin = publication, login
	*run = event.ExpectedRun
	run.HeadSHA = event.CandidateSHA
	run.ReviewApprovedHeadSHA, run.CIReadyAt, run.TerminalHeadVerifiedAt = nil, nil, nil
	run.CIReadyNoCI, run.PushActive = false, false
	run.AwaitingAgentSince, run.Error = nil, nil
	if e.shared != nil {
		e.shared.Close()
	}
	e.initializeRunScopes(run.ID)
	e.sessions.Forget(SessionRoleReviewer)
	e.sessions.Forget(SessionRoleFixer)
	e.stepRecords = nil
	e.adoptionRestartPending = true
	e.ciAdoptionReserved = false
}

func (e *Executor) originalAdoptedReviewRound(stepResultID string, round int) bool {
	if e.adoption != nil {
		for _, original := range e.adoption.Steps {
			if original.Snapshot.ID == stepResultID && original.Snapshot.StepName == types.StepReview {
				return round <= len(original.Rounds)
			}
		}
	}
	return false
}

const PublicationSelectionFindingID = "ci-publication-credentials"
const PublicationRevalidationFindingID = "ci-publication-revalidation-required"

func (e *Executor) executeWithPublicationSelection(step Step, sctx *StepContext) (*StepOutcome, error) {
	if e.adoption == nil || step.Name().Order() < types.StepPush.Order() {
		return step.Execute(sctx)
	}
	stored, err := e.db.GetRun(sctx.Run.ID)
	if err != nil || stored == nil {
		return nil, fmt.Errorf("cannot read publication state; preserve adopted validation")
	}
	if stored.PushGeneration != nil && e.adoption.ExpectedRun.PushGeneration != nil && *stored.PushGeneration > *e.adoption.ExpectedRun.PushGeneration {
		return step.Execute(sctx)
	}
	if e.publicationForge == nil && step.Name() != types.StepPush {
		return nil, fmt.Errorf("publication selection is unavailable; preserve adopted validation")
	}
	if step.Name() == types.StepPush {
		if e.publicationForge == nil || forgecontext.VerifyPublication(sctx.Ctx, sctx.WorkDir, e.publicationForge, e.publicationLogin) != nil {
			e.mu.Lock()
			e.publicationSelectionRequired = true
			e.mu.Unlock()
			return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"ci-publication-credentials","severity":"warning","action":"ask-user","description":"Publication credential selection is unavailable. Explicitly reselect it with axi respond --action revalidate --reselect-publication; completed fresh validation is retained."}]}`}, nil
		}
	}
	copyContext := *sctx
	copyContext.ForgeContext = e.publicationForge
	copyContext.Ctx = git.WithEnvironment(sctx.Ctx, e.publicationForge.Environment)
	outcome, err := step.Execute(&copyContext)
	if step.Name() == types.StepPush && e.publicationOnlyResume && err == nil {
		if outcome.RestartFrom != "" {
			e.mu.Lock()
			e.publicationSelectionRequired = true
			e.mu.Unlock()
			return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"ci-publication-revalidation-required","severity":"error","action":"ask-user","description":"The publication safety check requires fresh validation. Credential reselection cannot reset validation or dispatch a fixer; the source and completed results are preserved."}]}`}, nil
		}
		e.publicationOnlyResume = false
	}
	return outcome, err
}

func (e *Executor) restartIndex(runID string, name types.StepName, currentIndex int) (int, error) {
	if e.adoptionRestartPending && name == types.StepReview {
		e.adoptionRestartPending = false
		return e.stepIndex(name)
	}
	return e.prepareRestart(runID, name, currentIndex)
}

// ResumeCIAdoption continues only a durable pending prefix. A parked fresh
// validation uses ordinary Resume; an interrupted writer remains held.
func (e *Executor) ResumeCIAdoption(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, event *db.CIRepairAdoption, publication *forgecontext.Context, login string) error {
	e.mu.Lock()
	e.adoption, e.publicationForge, e.publicationLogin = event, publication, login
	e.mu.Unlock()
	if run.AwaitingAgentSince != nil {
		return e.Resume(ctx, run, repo, workDir)
	}
	start, err := CIAdoptionPendingIndex(e.db, run, e.steps)
	if err != nil {
		return err
	}
	isPublicationReplay := e.steps[start].Name() == types.StepPush
	if isPublicationReplay {
		if run.HeadSHA != event.CandidateSHA || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != run.HeadSHA {
			return fmt.Errorf("publication replay requires the exact freshly reviewed adopted head")
		}
		e.publicationOnlyResume = true
	}
	e.workDir, e.runID = workDir, run.ID
	ctx = e.runContext(ctx)
	e.initializeRunScopes(run.ID)
	defer func() { e.shared.Close() }()
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	return e.executeRecoveredRemainder(ctx, run, repo, workDir, logDir, start, isPublicationReplay || e.steps[start].Name() == types.StepReview)
}

func CIAdoptionPendingIndex(database *db.DB, run *db.Run, steps []Step) (int, error) {
	if run.Status != types.RunRunning {
		return 0, fmt.Errorf("adopted run is no longer active")
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return 0, err
	}
	if len(results) != len(steps) {
		return 0, fmt.Errorf("adopted step sequence changed")
	}
	start := -1
	for index, step := range results {
		if step.StepName != steps[index].Name() || (step.AgentPID != nil && *step.AgentPID != 0) {
			return 0, fmt.Errorf("adopted step identity or writer changed")
		}
		if start < 0 && (step.Status == types.StepStatusCompleted || (step.StepName == types.StepIntent && step.Status == types.StepStatusSkipped)) {
			continue
		}
		if step.Status != types.StepStatusPending {
			if start < 0 && step.StepName == types.StepPush && step.Status == types.StepStatusRunning && run.PushActive && run.ReviewApprovedHeadSHA != nil && *run.ReviewApprovedHeadSHA == run.HeadSHA {
				start = index
				continue
			}
			return 0, fmt.Errorf("adopted step %s was interrupted; preserve it without replay", step.StepName)
		}
		if start < 0 {
			start = index
		}
	}
	if start < 0 {
		return 0, fmt.Errorf("adopted run has no pending validation")
	}
	return start, nil
}
