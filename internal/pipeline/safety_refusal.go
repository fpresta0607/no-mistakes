package pipeline

import (
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const safetyRefusalFindingID = "agent-safety-refusal"

// HasSafetyRefusal identifies a native refusal requiring an explicit decision.
func HasSafetyRefusal(findingsJSON string) bool {
	return hasFindingID(findingsJSON, safetyRefusalFindingID)
}

func safetyRefusalOutcome(err error) *StepOutcome {
	if !agent.IsSafetyRefusal(err) {
		return nil
	}
	description := safeurl.RedactText(err.Error())
	for i, count := 0, 0; i < len(description); count++ {
		if count == 4096 {
			description = description[:i] + "... (truncated; full diagnostic in step log)"
			break
		}
		_, size := utf8.DecodeRuneInString(description[i:])
		i += size
	}
	findings, _ := types.MarshalFindingsJSON(types.Findings{
		Summary: "Native agent safety refusal; automatic execution stopped",
		Items: []types.Finding{{
			ID:          safetyRefusalFindingID,
			Severity:    "error",
			Description: description,
			Action:      types.ActionAskUser,
		}},
	})
	return &StepOutcome{NeedsApproval: true, Findings: findings}
}
