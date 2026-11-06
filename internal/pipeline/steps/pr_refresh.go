package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RefreshPublishedPR publishes only a stored Test exception, without running a pipeline step.
func RefreshPublishedPR(sctx *pipeline.StepContext) (bool, error) {
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return false, fmt.Errorf("PR refresh unavailable: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return false, err
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return false, err
	}
	return refreshPublishedPRWithHost(sctx, host, steps)
}

func refreshPublishedPRWithHost(sctx *pipeline.StepContext, host scm.Host, steps []*db.StepResult) (bool, error) {
	reader, isReader := host.(scm.PRContentReader)
	if !isReader || sctx.Run.PRURL == nil {
		return false, fmt.Errorf("PR refresh requires a recorded PR and raw content reader")
	}
	verifyIdentity := func() (*scm.PR, error) {
		pr, err := host.FindPR(sctx.Ctx, sctx.Run.Branch, "")
		if err != nil {
			return nil, err
		}
		number, err := scm.ExtractPRNumber(*sctx.Run.PRURL)
		if err != nil || pr == nil || pr.URL != *sctx.Run.PRURL || pr.Number != number {
			return nil, fmt.Errorf("PR refresh refused: live PR identity changed")
		}
		state, err := host.GetPRState(sctx.Ctx, pr)
		if err != nil || state != scm.PRStateOpen {
			return nil, fmt.Errorf("PR refresh refused: live PR is not verifiably open: %v", err)
		}
		pr.HeadSHA = sctx.Run.HeadSHA
		pr.ObservedHeadSHA = ""
		if _, err := host.GetChecks(sctx.Ctx, pr); err != nil {
			return nil, fmt.Errorf("PR refresh cannot verify the live head: %w", err)
		}
		if pr.ObservedHeadSHA != sctx.Run.HeadSHA {
			return nil, fmt.Errorf("PR refresh refused: live PR head changed or was not observed")
		}
		return pr, nil
	}
	pr, err := verifyIdentity()
	if err != nil {
		return false, err
	}
	content, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil {
		return false, err
	}
	body := content.Body
	var parts prOwnedBody
	isOwned := hasPRAppendixMarkers(body)
	if isOwned {
		parts, err = parsePROwnedBody(body)
		if err != nil {
			return false, err
		}
		body = parts.appendix
	}
	redactionRoot := sctx.Run.WorktreePath()
	if redactionRoot == "" {
		redactionRoot = sctx.WorkDir
	}
	updated, err := refreshPRTestException(body, sctx.Run.HeadSHA, steps, redactionRoot)
	if err != nil {
		return false, err
	}
	if isOwned {
		updated = parts.before + wrapPRAppendix(updated) + parts.after
	}
	if err := validateOwnedPRBudget(updated, scm.HostMaxPRBodyChars(host)); err != nil {
		return false, err
	}
	if updated == content.Body {
		return false, nil
	}
	pr, err = verifyIdentity()
	if err != nil {
		return false, err
	}
	latest, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil || latest.Body != content.Body {
		return false, fmt.Errorf("PR refresh refused: body changed or became unreadable before write: %v", err)
	}
	// A write can succeed before returning an error, so it is never replayed.
	if _, err := host.UpdatePR(sctx.Ctx, pr, scm.PRContent{Body: updated}); err != nil {
		return false, fmt.Errorf("PR refresh write outcome is uncertain; inspect without retrying: %w", err)
	}
	written, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil || written.Body != updated {
		return false, fmt.Errorf("PR refresh write could not be confirmed; inspect without retrying: %v", err)
	}
	if _, err := verifyIdentity(); err != nil {
		return false, fmt.Errorf("PR refresh write could not be confirmed against the live identity; inspect without retrying: %w", err)
	}
	return true, nil
}

func refreshPRTestException(body, head string, steps []*db.StepResult, workDir string) (string, error) {
	var testStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepTest {
			if testStep != nil {
				return "", fmt.Errorf("PR refresh refused: ambiguous stored Test record")
			}
			testStep = step
		}
	}
	if testStep == nil || testStep.TestOverrideReason() == "" {
		return "", fmt.Errorf("PR refresh requires a completed, recorded Test exception")
	}
	if strings.Count(body, pipelineAttestationCommentPrefix) != 1 {
		return "", fmt.Errorf("PR refresh requires exactly one existing native attestation")
	}
	start := strings.Index(body, pipelineAttestationCommentPrefix)
	if markdownFenceOpen(body[:start]) {
		return "", fmt.Errorf("PR refresh refused: attestation is quoted")
	}
	payloadStart := start + len(pipelineAttestationCommentPrefix)
	payload, _, isClosed := strings.Cut(body[payloadStart:], pipelineAttestationCommentClosingToken)
	if !isClosed {
		return "", fmt.Errorf("PR refresh refused: incomplete attestation")
	}
	var attestation map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &attestation); err != nil {
		return "", fmt.Errorf("PR refresh refused: malformed attestation: %w", err)
	}
	var attestedHead string
	if json.Unmarshal(attestation["head_sha"], &attestedHead) != nil || attestedHead != head {
		return "", fmt.Errorf("PR refresh refused: attestation belongs to another head")
	}
	var recordedSteps []map[string]json.RawMessage
	if err := json.Unmarshal(attestation["steps"], &recordedSteps); err != nil {
		return "", fmt.Errorf("PR refresh refused: malformed attested steps: %w", err)
	}
	exception := redactPRContent(prContent{Body: testStep.TestOverrideReason()}, workDir, nmWorktreesRoot()).Body
	var isTestRecorded, isExceptionPublished bool
	for _, step := range recordedSteps {
		var name types.StepName
		if json.Unmarshal(step["step"], &name) != nil {
			return "", fmt.Errorf("PR refresh refused: malformed attested step identity")
		}
		if name != types.StepTest {
			continue
		}
		var status types.StepStatus
		if isTestRecorded || json.Unmarshal(step["status"], &status) != nil || status != types.StepStatusCompleted {
			return "", fmt.Errorf("PR refresh refused: Test attestation is not a unique completed record")
		}
		isTestRecorded = true
		var commandOverride string
		if raw, isPresent := step["override_reason"]; isPresent && json.Unmarshal(raw, &commandOverride) != nil {
			return "", fmt.Errorf("PR refresh refused: malformed command override")
		}
		storedOverride := ""
		if testStep.OverrideReason != nil {
			storedOverride = redactPRContent(prContent{Body: strings.TrimSpace(*testStep.OverrideReason)}, workDir, nmWorktreesRoot()).Body
		}
		if commandOverride != storedOverride {
			return "", fmt.Errorf("PR refresh refused: command override differs from the stored record")
		}
		if raw, isPresent := step["test_exception"]; isPresent {
			isExceptionPublished = true
			var recorded string
			if json.Unmarshal(raw, &recorded) != nil || recorded != exception {
				return "", fmt.Errorf("PR refresh refused: published Test exception differs from the stored record")
			}
		}
		step["test_exception"], _ = json.Marshal(exception)
	}
	if !isTestRecorded {
		return "", fmt.Errorf("PR refresh refused: no completed Test attestation")
	}
	attestation["steps"], _ = json.Marshal(recordedSteps)
	rebuilt, err := json.Marshal(attestation)
	if err != nil {
		return "", err
	}
	oldEnd := payloadStart + len(payload) + len(pipelineAttestationCommentClosingToken)
	updated := body[:payloadStart] + string(rebuilt) + body[oldEnd-len(pipelineAttestationCommentClosingToken):]
	section := redactPRContent(prContent{Body: renderTestExceptionSection(steps)}, workDir, nmWorktreesRoot()).Body
	starts, ends := strings.Count(updated, testExceptionStart), strings.Count(updated, testExceptionEnd)
	if starts == 0 && ends == 0 {
		if isExceptionPublished {
			return "", fmt.Errorf("PR refresh refused: published Test exception section was deleted")
		}
		return updated + "\n\n" + section, nil
	}
	if starts != 1 || ends != 1 || !strings.Contains(updated, section) {
		return "", fmt.Errorf("PR refresh refused: Test exception text was edited or is ambiguous")
	}
	return updated, nil
}
