package authoredvalidation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/launchassert"
)

const MaxBytes = 65536

// ControllerPolicy is a controller's historical assertion, not a native launch fact.
// It conveys no repair authority: every authored admission has zero capacity.
type ControllerPolicy struct {
	SHA256       string `json:"sha256"`
	Class        string `json:"class"`
	ReviewCycles int    `json:"review_cycles"`
}

type Request struct {
	PredecessorID                string                    `json:"predecessor_id"`
	SnapshotDigest               string                    `json:"snapshot_digest"`
	RepoID                       string                    `json:"repo_id"`
	Branch                       string                    `json:"branch"`
	CallerDir                    string                    `json:"caller_dir"`
	CandidateSHA                 string                    `json:"candidate_sha"`
	CandidateTree                string                    `json:"candidate_tree"`
	LaunchNonce                  string                    `json:"launch_nonce"`
	ValidationGeneration         string                    `json:"validation_generation"`
	IntentDigest                 string                    `json:"intent_digest"`
	PublishedHeadSHA             string                    `json:"published_head_sha"`
	PushGeneration               int64                     `json:"push_generation"`
	PushTargetFingerprint        string                    `json:"push_target_fingerprint"`
	ControllerPolicy             ControllerPolicy          `json:"controller_policy"`
	LaunchAssertion              *launchassert.Expectation `json:"launch_assertion"`
	HistoricalSourceCompleteness string                    `json:"historical_source_completeness"`
	Reason                       string                    `json:"reason"`
	TerminalGeneration           string                    `json:"terminal_generation,omitempty"`
	AcceptUnknownSource          bool                      `json:"accept_unknown_source,omitempty"`
	SourceAnchors                map[string]string         `json:"source_anchors,omitempty"`
}

func (request Request) Validate() error {
	if request.TerminalGeneration != "" {
		if strings.TrimSpace(request.TerminalGeneration) != request.TerminalGeneration || len(request.TerminalGeneration) > 128 || request.TerminalGeneration == request.ValidationGeneration || !request.AcceptUnknownSource || request.LaunchAssertion == nil || !request.LaunchAssertion.Apply {
			return fmt.Errorf("terminal validation requires a distinct generation, explicit unknown-source acceptance and an explicit launch selection")
		}
	} else if request.AcceptUnknownSource || len(request.SourceAnchors) != 0 {
		return fmt.Errorf("terminal source acceptance requires a terminal validation generation")
	}
	if request.PredecessorID == "" || request.RepoID == "" || request.Branch == "" || request.CallerDir == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 {
		return fmt.Errorf("authored admission requires predecessor, repository, branch, caller checkout and explicit reason")
	}
	for _, value := range []string{request.SnapshotDigest, request.IntentDigest, request.ControllerPolicy.SHA256} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 32 || value != strings.ToLower(value) {
			return fmt.Errorf("authored admission requires full snapshot, intent and controller policy digests")
		}
	}
	for _, value := range []string{request.CandidateSHA, request.CandidateTree} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 20 || value != strings.ToLower(value) {
			return fmt.Errorf("authored admission requires full commit and tree identities")
		}
	}
	if request.HistoricalSourceCompleteness != "unknown" {
		return fmt.Errorf("declare historical_source_completeness as unknown; authored validation cannot certify missing historical source")
	}
	if request.ControllerPolicy.Class == "" || len(request.ControllerPolicy.Class) > 64 || request.ControllerPolicy.ReviewCycles < 0 || request.LaunchNonce == "" || request.ValidationGeneration == "" || request.LaunchAssertion == nil {
		return fmt.Errorf("authored admission requires the controller's frozen policy assertion and original launch binding")
	}
	return request.LaunchAssertion.Validate()
}

func Decode(data []byte) (*Request, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("authored admission must be UTF-8 JSON")
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("authored admission exceeds %d bytes", MaxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("authored admission must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, isString := token.(string)
		if !isString {
			return nil, fmt.Errorf("invalid authored admission field")
		}
		switch key {
		case "predecessor_id", "snapshot_digest", "repo_id", "branch", "caller_dir", "candidate_sha", "candidate_tree", "launch_nonce", "validation_generation", "intent_digest", "published_head_sha", "push_generation", "push_target_fingerprint", "controller_policy", "launch_assertion", "historical_source_completeness", "reason", "terminal_generation", "accept_unknown_source", "source_anchors":
		default:
			return nil, fmt.Errorf("unknown or non-canonical authored admission field %q", key)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate authored admission field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("authored admission has trailing input")
	}
	var request Request
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&request); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &request, nil
}

func Read(path string) (*Request, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, fmt.Errorf("authored admission must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

func (request Request) Digest() string {
	payload, _ := json.Marshal(request)
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func (request Request) SuccessorGeneration() string {
	if request.TerminalGeneration != "" {
		return request.TerminalGeneration
	}
	return request.ValidationGeneration
}
