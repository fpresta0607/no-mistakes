package ipc

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
	RunID        string `json:"run_id"`
	CandidateSHA string `json:"candidate_sha"`
	AdoptionID   string `json:"adoption_id"`
	Started      bool   `json:"started"`
}
