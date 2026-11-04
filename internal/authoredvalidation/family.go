package authoredvalidation

import "github.com/kunchenguid/no-mistakes/internal/types"

type RepairSpending struct {
	Automatic int `json:"automatic"`
	Manual    int `json:"manual"`
	Remaining int `json:"remaining"`
}

type Family struct {
	HasChildRepairs              bool                              `json:"has_child_repairs"`
	AdmissionID                  string                            `json:"admission_id"`
	PredecessorID                string                            `json:"predecessor_id"`
	PolicyOrigin                 string                            `json:"policy_origin"`
	ControllerPolicy             ControllerPolicy                  `json:"controller_policy"`
	HistoricalSourceCompleteness string                            `json:"historical_source_completeness"`
	Spending                     map[types.StepName]RepairSpending `json:"spending"`
}
