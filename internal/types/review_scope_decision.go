package types

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ReviewScopeDecision is an explicit controller assertion, not authenticated
// operator identity. The managed controller must authenticate its decision
// maker before submitting it through the existing local IPC boundary.
type ReviewScopeDecision struct {
	Version          int     `json:"version"`
	DecisionID       string  `json:"decision_id"`
	Authority        string  `json:"authority"`
	RunID            string  `json:"run_id"`
	StepResultID     string  `json:"step_result_id"`
	RoundID          string  `json:"round_id"`
	SourceHeadSHA    string  `json:"source_head_sha"`
	SourceBaseSHA    string  `json:"source_base_sha"`
	TrustedConfigSHA string  `json:"trusted_config_sha"`
	Finding          Finding `json:"finding"`
	AcceptedScope    string  `json:"accepted_scope"`
}

func (d ReviewScopeDecision) Validate() error {
	if d.Version != 1 || d.Authority != "controller_assertion" {
		return fmt.Errorf("scope decision requires version 1 and controller_assertion authority")
	}
	for _, value := range []string{d.DecisionID, d.RunID, d.StepResultID, d.RoundID, d.SourceHeadSHA, d.SourceBaseSHA, d.TrustedConfigSHA, d.Finding.ID, d.Finding.Description, d.AcceptedScope} {
		if strings.TrimSpace(value) == "" || len(value) > 8192 {
			return fmt.Errorf("scope decision requires complete bounded decision, source, finding and scope fields")
		}
	}
	if d.Finding.UserInstructions != "" || d.Finding.DriverInstructions != "" {
		return fmt.Errorf("scope decision must name the original finding without fix guidance")
	}
	encoded, err := json.Marshal(d)
	if err != nil || len(encoded) > 32*1024 {
		return fmt.Errorf("scope decision exceeds 32 KiB")
	}
	return nil
}
