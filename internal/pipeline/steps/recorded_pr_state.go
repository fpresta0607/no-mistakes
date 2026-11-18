package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// readRecordedPRState reads the live state of the PR recorded on the run.
func readRecordedPRState(sctx *pipeline.StepContext) (scm.Host, *scm.PR, scm.PRState, error) {
	host, skipReason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return nil, nil, "", fmt.Errorf("cannot check PR state: %s", skipReason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, nil, "", err
	}
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = strings.TrimSpace(*sctx.Run.PRURL)
	}
	if prURL == "" {
		return nil, nil, "", fmt.Errorf("run has no PR URL")
	}
	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil {
		return nil, nil, "", err
	}
	return host, pr, state, nil
}

// ObserveTerminalPRState reads a terminal run's recorded PR on its forge and
// records a merge or close the way the CI step does, merged proof included.
// The CI step stops observing the PR once the run ends, which can happen with
// the PR still open (ci_end_at_green, ci.release_after_checks_pass, the CI
// idle timeout, a failure or a cancel). An open PR records nothing, and any
// state that cannot be read or proven is an error, so callers fail closed.
// The proof names the head the run last published: a run that ended before
// its own push still carries the PR its predecessor published.
func ObserveTerminalPRState(sctx *pipeline.StepContext) error {
	host, pr, state, err := readRecordedPRState(sctx)
	if err != nil {
		return err
	}
	switch state {
	case scm.PRStateMerged:
		publishedHead := sctx.Run.HeadSHA
		if sctx.Run.LastPushedSHA != nil {
			publishedHead = *sctx.Run.LastPushedSHA
		}
		if err := verifyMergedProof(sctx.Ctx, host, pr, publishedHead); err != nil {
			return err
		}
		return sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged")
	case scm.PRStateClosed:
		return sctx.DB.UpdateRunPRState(sctx.Run.ID, "closed")
	case scm.PRStateOpen:
		return nil
	default:
		return fmt.Errorf("PR state is unresolved: %q", state)
	}
}
