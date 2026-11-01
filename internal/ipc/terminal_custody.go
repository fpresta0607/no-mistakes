package ipc

import "github.com/kunchenguid/no-mistakes/internal/db"

const MethodRecoverTerminal = "recover_terminal"

type RecoverTerminalParams struct {
	RepoID         string `json:"repo_id"`
	RunID          string `json:"run_id"`
	Branch         string `json:"branch"`
	CallerDir      string `json:"caller_dir"`
	SnapshotDigest string `json:"snapshot_digest"`
	Reason         string `json:"reason"`
}

type RecoverTerminalResult struct {
	RunID         string                            `json:"run_id"`
	PredecessorID string                            `json:"predecessor_id"`
	CandidateTree string                            `json:"candidate_tree"`
	CandidateSHA  string                            `json:"candidate_sha"`
	AdoptionID    string                            `json:"adoption_id"`
	PhaseBudgets  map[string]db.TerminalPhaseBudget `json:"phase_budgets"`
	Started       bool                              `json:"started"`
}
