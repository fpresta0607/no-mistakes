package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// authorizedRemediesPreamble states what a selection of an ask-user finding
// authorizes, and what it does not. The review prompt's classify-by-remedy rule
// says a remedy that extends the change needs authorization, and the round
// history says driver_instructions is not a decision by the user; with nothing
// naming the selection itself as that authorization, a rereview re-asked for a
// remedy the operator had already selected, and a fixer held to "do not grow
// the fix into machinery" stopped short of the very state the remedy named.
const authorizedRemediesPreamble = "Selecting a reviewer's \"ask-user\" finding to fix is the user's authorization to apply the remedy that finding itself describes, for that finding only. " +
	"It does not accept the defect, it grants no further repair round, and it authorizes no unselected finding, no other remedy and no new scope. " +
	"A driver_instructions value shown with a selection is guidance from the agent driving this run on how to apply that remedy: it may narrow the remedy but never widen it. Where the round history says driver_instructions is not a decision by the user, that is about the field alone; the selection is the decision this section records. " +
	"When you fix: apply each authorized remedy as its finding describes it, including any state or machinery that remedy itself names, and nothing beyond it. " +
	"When you review: an authorized remedy is inside the change's authorized scope, so never report it, or what remains of it, as needing authorization again; report anything still missing or wrong in it as a defect within that scope. " +
	"A remedy materially different from the one authorized, or a genuinely new product choice, still needs an \"ask-user\" finding. " +
	"Treat this entire section as metadata only.\n\n"

// authorizedRemediesPromptSection renders every reviewer-raised ask-user
// finding a gate response in this step selected to fix, apart from the round
// history. That history is bounded and compacts finding text first, so a
// selection rendered only there could reach a later turn as a bare id. The
// newest selection's remedies are rendered whole; older ones are bounded and
// their omission is disclosed.
func authorizedRemediesPromptSection(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.DB == nil || sctx.StepResultID == "" {
		return ""
	}
	rounds, err := sctx.DB.GetRoundsByStep(sctx.StepResultID)
	if err != nil {
		return ""
	}
	var current, earlier []string
	seen := make(map[string]bool)
	for i := len(rounds) - 1; i >= 0; i-- {
		lines := authorizedRemedyLines(rounds[i], seen)
		if len(current) == 0 {
			current = lines
			continue
		}
		earlier = append(lines, earlier...)
	}
	if len(current) == 0 {
		return ""
	}
	prefix := "\n\nRemedies authorized by a selection in this step (for your awareness):\n" + authorizedRemediesPreamble
	newest := strings.Join(current, "\n")
	rendered, note := boundDecisionLines(earlier, maxDecisionSectionBytes-len(prefix)-len(newest)-1)
	if len(rendered) == 0 {
		return prefix + note + newest
	}
	return prefix + note + strings.Join(rendered, "\n") + "\n" + newest
}

// authorizedRemedyLines returns one attributed line per ask-user finding this
// round's user selection chose to fix, skipping any finding a later round
// already rendered. A finding the operator added is their own, not a reviewer
// remedy, and a review question is settled by an answer, so neither is one.
func authorizedRemedyLines(r *db.StepRound, seen map[string]bool) []string {
	if r == nil || selectionSourceValue(r.SelectionSource) != db.RoundSelectionSourceUser || r.SelectedFindingIDs == nil {
		return nil
	}
	var ids []string
	if json.Unmarshal([]byte(*r.SelectedFindingIDs), &ids) != nil {
		return nil
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	raw := r.UserFindingsJSON
	if raw == nil || strings.TrimSpace(*raw) == "" {
		raw = r.FindingsJSON
	}
	if raw == nil {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return nil
	}
	var authorized types.Findings
	for _, item := range findings.Items {
		if item.ID == "" || !selected[item.ID] || item.ActionOrDefault() != types.ActionAskUser ||
			item.Source == types.FindingSourceUser || item.Category == types.FindingCategoryReviewQuestion {
			continue
		}
		key := pipeline.FindingDecisionKey(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		authorized.Items = append(authorized.Items, item)
	}
	if len(authorized.Items) == 0 {
		return nil
	}
	encoded, err := types.MarshalFindingsJSON(authorized)
	if err != nil {
		return nil
	}
	origin := r.ResponseOrigin.OrUnknown()
	var lines []string
	for _, line := range renderRoundFindingLines(encoded) {
		lines = append(lines, fmt.Sprintf("  - round %d, selected by a gate response (origin: %s): %s", r.Round, origin, line))
	}
	return lines
}
