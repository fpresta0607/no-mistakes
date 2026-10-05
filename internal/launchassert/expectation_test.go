package launchassert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testExpectation() *Expectation {
	profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	return &Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
}

func TestLaunchAssertionReadsRequiredProofOnce(t *testing.T) {
	expected := testExpectation()
	path := filepath.Join(t.TempDir(), "expectation.json")
	encoded, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	captured, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !captured.Matches(expected) {
		t.Fatal("captured expectation changed with its source file")
	}
	if _, err := Read(path); err == nil {
		t.Fatal("unreadable proof was accepted")
	}
	if _, err := Read(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing required proof was accepted")
	}
}

func TestLaunchAssertionRejectsIncompleteOrUnknownProof(t *testing.T) {
	for _, source := range []string{
		`null`, `{}`, `{"trusted_sha":"bad","profiles":{}}`,
		`{"trusted_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","profiles":{},"ignored":true}`,
	} {
		var expected Expectation
		if err := json.Unmarshal([]byte(source), &expected); err == nil {
			t.Fatalf("invalid proof was accepted: %s", source)
		}
	}
}

func TestLaunchAssertionProofBindsExactSourceAndEveryRole(t *testing.T) {
	expected := testExpectation()
	proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
	if err != nil || proof.Check(expected) != nil {
		t.Fatalf("matching proof rejected: %+v %v", proof, err)
	}
	if _, err := expected.Verify(strings.Repeat("b", 40), expected.Profiles); err == nil {
		t.Fatal("changed source accepted")
	}
	for _, role := range []string{"primary", "reviewer", "fixer"} {
		changed := testExpectation()
		changed.Profiles[role][0].ServiceTier = "priority"
		if _, err := expected.Verify(changed.TrustedSHA, changed.Profiles); err == nil {
			t.Fatalf("%s mismatch accepted", role)
		}
		if proof.Check(changed) == nil {
			t.Fatalf("%s conflicting replay accepted", role)
		}
	}
	if (*Proof)(nil).Check(expected) == nil {
		t.Fatal("missing native proof accepted")
	}
}
