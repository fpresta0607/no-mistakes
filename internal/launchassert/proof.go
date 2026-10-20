package launchassert

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

type Proof struct {
	AssertionDigest string                          `json:"assertion_digest"`
	TrustedSHA      string                          `json:"trusted_sha"`
	Profiles        map[string][]agentcfg.Selection `json:"profiles"`
	Apply           bool                            `json:"apply,omitempty"`
}

// assertion is the expectation this proof says it proved, field for field.
// Value and Scan check the proof against it, so a proof that lost a field of
// its assertion is neither stored nor loaded.
func (p *Proof) assertion() *Expectation {
	return &Expectation{TrustedSHA: p.TrustedSHA, Profiles: p.Profiles, Apply: p.Apply}
}

func (p *Proof) Check(expected *Expectation) error {
	if expected == nil {
		if p != nil {
			return fmt.Errorf("launch proof has no captured assertion")
		}
		return nil
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	if p == nil || p.AssertionDigest != expected.Digest() || p.TrustedSHA != expected.TrustedSHA || !reflect.DeepEqual(p.Profiles, expected.Profiles) {
		return fmt.Errorf("missing or conflicting native launch proof")
	}
	return nil
}

func (p Proof) Value() (driver.Value, error) {
	if err := p.Check(p.assertion()); err != nil {
		return nil, err
	}
	data, err := json.Marshal(p)
	return string(data), err
}

func (p *Proof) Scan(source interface{}) error {
	var data []byte
	switch value := source.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		return fmt.Errorf("unreadable persisted launch proof")
	}
	if err := json.Unmarshal(data, p); err != nil {
		return fmt.Errorf("unreadable persisted launch proof")
	}
	return p.Check(p.assertion())
}
