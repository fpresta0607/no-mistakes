package launchassert

import (
	"bytes"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

const MaxBytes = 8192

// Expectation is a caller-captured, immutable input to a nonce-bound launch.
// It contains no commands, paths, credentials or inferred provider identity.
type Expectation struct {
	TrustedSHA string                          `json:"trusted_sha"`
	Profiles   map[string][]agentcfg.Selection `json:"profiles"`
}

func (e *Expectation) Validate() error {
	if e == nil {
		return nil
	}
	decoded, err := hex.DecodeString(e.TrustedSHA)
	if err != nil || len(decoded) != 20 || e.TrustedSHA != strings.ToLower(e.TrustedSHA) {
		return fmt.Errorf("launch assertion requires a full trusted commit SHA")
	}
	for _, role := range []string{"primary", "reviewer", "fixer"} {
		if len(e.Profiles[role]) == 0 {
			return fmt.Errorf("launch assertion requires primary, reviewer and fixer profiles")
		}
	}
	for role, profiles := range e.Profiles {
		switch role {
		case "primary", "reviewer", "fixer", "reviewer_after_round", "fixer_after_round":
		default:
			return fmt.Errorf("unknown launch assertion role")
		}
		if len(profiles) == 0 || len(profiles) > 8 {
			return fmt.Errorf("launch assertion requires a bounded resolved agent chain")
		}
		for _, profile := range profiles {
			if err := profile.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Expectation) UnmarshalJSON(data []byte) error {
	type wire Expectation
	var decoded wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if len(data) > MaxBytes || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("invalid launch assertion")
	}
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("unreadable launch assertion")
	}
	*e = Expectation(decoded)
	return e.Validate()
}

func Read(path string) (*Expectation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read required launch assertion: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, fmt.Errorf("launch assertion must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read required launch assertion: %w", err)
	}
	var expected Expectation
	if err := json.Unmarshal(data, &expected); err != nil {
		return nil, err
	}
	return &expected, nil
}

func (e *Expectation) Digest() string {
	data, _ := json.Marshal(e)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest)
}

func (e *Expectation) Matches(other *Expectation) bool {
	return reflect.DeepEqual(e, other)
}

func (e *Expectation) Verify(trustedSHA string, profiles map[string][]agentcfg.Selection) (*Proof, error) {
	if e == nil {
		return nil, nil
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if trustedSHA != e.TrustedSHA {
		return nil, fmt.Errorf("launch assertion trusted source differs after fresh fetch")
	}
	if !reflect.DeepEqual(e.Profiles, profiles) {
		return nil, fmt.Errorf("launch assertion effective agent profiles differ")
	}
	return &Proof{AssertionDigest: e.Digest(), TrustedSHA: trustedSHA, Profiles: profiles}, nil
}

func (e Expectation) Value() (driver.Value, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(e)
	return string(data), err
}

func (e *Expectation) Scan(source interface{}) error {
	var data []byte
	switch value := source.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		return fmt.Errorf("unreadable persisted launch assertion")
	}
	return json.Unmarshal(data, e)
}
