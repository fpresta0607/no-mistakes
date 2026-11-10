package pipeline

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewBudgetNoticeIDs are the notices that report a spent budget at a Review
// gate, which never join the outstanding findings.
var reviewBudgetNoticeIDs = []string{types.FindingIDReviewFixRoundsExhausted, types.FindingIDRepairAgentBudget}

// RepairBudgetCutError is a repair turn stopped at its agent budget. The turn
// may have been working and not stuck, so it costs the run a decision, never
// the run: the executor parks it, with what the turn left kept in the worktree.
type RepairBudgetCutError struct {
	// Cut is the budget expiry.
	Cut error
	// Left names what the turn left that nothing has checked, "" for nothing.
	Left string
}

func (e *RepairBudgetCutError) Error() string { return e.Cut.Error() }

func (e *RepairBudgetCutError) Unwrap() error { return e.Cut }

// RepairBudgetOutcome turns a repair budget cut into the park it asks for, or
// returns nil for any other error. The round read nothing, so its one finding
// is a notice to decide on: never auto-fixable, and never a certified head.
func RepairBudgetOutcome(err error) *StepOutcome {
	var cut *RepairBudgetCutError
	if !errors.As(err, &cut) {
		return nil
	}
	left := "It left no work in the run worktree."
	if cut.Left != "" {
		left = "Nothing has checked what it left, and none of it is published: the run worktree holds " + cut.Left + "."
	}
	findings, _ := types.MarshalFindingsJSON(types.Findings{
		Summary: "Repair turn reached its agent budget",
		Items: []types.Finding{{
			ID:       types.FindingIDRepairAgentBudget,
			Severity: types.FindingSeverityWarning,
			Action:   types.ActionAskUser,
			Description: fmt.Sprintf("A repair turn was stopped at its agent budget, so this round checked nothing: %s. %s "+
				"Respond with fix selecting only this notice to keep that work and check it, with no repair turn and no decision on any other finding. Selecting other findings too spends another budget on them. Or abort.",
				safeurl.RedactText(err.Error()), left),
		}},
	})
	return &StepOutcome{NeedsApproval: true, Findings: findings}
}

// HasRepairBudgetCut identifies a gate parked on a repair budget cut.
func HasRepairBudgetCut(findingsJSON string) bool {
	return hasFindingID(findingsJSON, types.FindingIDRepairAgentBudget)
}

// isRepairCutRecheck reports whether a fix response asks only that what a cut
// turn left be kept and checked. That decides no finding, so the others need no
// accounting and no selection is recorded: as for an answer round, a selection
// here would read as the operator declining every finding it leaves out.
func isRepairCutRecheck(findingIDs, ignoreFindingIDs []string, addedFindings []types.Finding) bool {
	return len(findingIDs) == 1 && findingIDs[0] == types.FindingIDRepairAgentBudget && len(ignoreFindingIDs) == 0 && len(addedFindings) == 0
}

// OnlyRepairBudgetCut reports whether a fix selection holds nothing but the
// budget notice, which is no work for a repair turn.
func OnlyRepairBudgetCut(findingsJSON string) bool {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	return err == nil && len(findings.Items) > 0 && len(types.ExcludeFindings(findings, []string{types.FindingIDRepairAgentBudget}).Items) == 0
}
