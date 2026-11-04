package pipeline

import (
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const SafetyRefusalFindingID = "agent-safety-refusal"

// HasSafetyRefusal identifies a native refusal requiring an explicit decision.
func HasSafetyRefusal(findingsJSON string) bool {
	return hasFindingID(findingsJSON, SafetyRefusalFindingID)
}

func SafetyRefusalOutcome(err error) *StepOutcome {
	if !agent.IsSafetyRefusal(err) {
		return nil
	}
	findings, _ := types.MarshalFindingsJSON(types.Findings{
		Summary: "Native agent safety refusal; automatic execution stopped",
		Items: []types.Finding{{
			ID:          SafetyRefusalFindingID,
			Severity:    "error",
			Description: safeurl.RedactText(err.Error()),
			Action:      types.ActionAskUser,
		}},
	})
	return &StepOutcome{NeedsApproval: true, Findings: findings}
}
