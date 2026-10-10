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

func TestLaunchAssertionFileAcceptsClaudeEntriesWithoutServiceTier(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name    string
		entry   string
		isValid bool
	}{
		{"claude", `{"harness":"claude","model":"claude-fixture","effort":"high"}`, true},
		{"claude with tier", `{"harness":"claude","model":"claude-fixture","effort":"high","service_tier":"default"}`, false},
		{"codex without tier", `{"harness":"codex","model":"fixture","effort":"high"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := `{"trusted_sha":"` + sha + `","profiles":{"primary":[` + test.entry + `],"reviewer":[` + test.entry + `],"fixer":[` + test.entry + `]}}`
			var expected Expectation
			if err := json.Unmarshal([]byte(source), &expected); (err == nil) != test.isValid {
				t.Fatalf("decode = %v, want valid=%v", err, test.isValid)
			}
			if !test.isValid {
				return
			}
			encoded, err := json.Marshal(expected.Profiles["primary"][0])
			if err != nil || string(encoded) != test.entry {
				t.Fatalf("canonical Claude entry = %s, want %s (%v)", encoded, test.entry, err)
			}
		})
	}
}

func TestLaunchSelectionNamesOneChainForEveryRole(t *testing.T) {
	claude := agentcfg.Selection{Harness: types.AgentClaude, Model: "claude-fixture", Effort: agentcfg.EffortHigh}
	codex := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	selection := func(primary, reviewer, fixer []agentcfg.Selection) *Expectation {
		return &Expectation{TrustedSHA: strings.Repeat("a", 40), Apply: true, Profiles: map[string][]agentcfg.Selection{
			"primary": primary, "reviewer": reviewer, "fixer": fixer,
		}}
	}
	chain := []agentcfg.Selection{claude, codex}
	if err := selection(chain, chain, chain).Validate(); err != nil {
		t.Fatalf("one chain for every role was refused: %v", err)
	}
	for name, invalid := range map[string]*Expectation{
		"reviewer on another harness": selection(chain, []agentcfg.Selection{codex}, chain),
		"fixer without the fallback":  selection(chain, chain, []agentcfg.Selection{claude}),
		"one harness twice":           selection([]agentcfg.Selection{claude, claude}, []agentcfg.Selection{claude, claude}, []agentcfg.Selection{claude, claude}),
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("%s was accepted as a launch selection", name)
		}
	}
	later := selection(chain, chain, chain)
	later.Profiles["reviewer_after_round"] = chain
	if err := later.Validate(); err == nil {
		t.Fatal("a later-round role was accepted in a launch selection")
	}
}

func TestLaunchSelectionIsADifferentAssertionThanItsProof(t *testing.T) {
	asserted := testExpectation()
	selected := testExpectation()
	selected.Apply = true
	if asserted.Digest() == selected.Digest() || asserted.Matches(selected) {
		t.Fatal("a launch selection replays as the assertion it extends")
	}
	plain, err := json.Marshal(asserted)
	if err != nil || strings.Contains(string(plain), "apply") {
		t.Fatalf("an assertion that applies nothing changed its stored form: %s %v", plain, err)
	}
	proof, err := selected.Verify(selected.TrustedSHA, selected.Profiles)
	if err != nil || proof.Check(selected) != nil || proof.Check(asserted) == nil {
		t.Fatalf("selection proof = %+v %v", proof, err)
	}
	stored, err := proof.Value()
	if err != nil {
		t.Fatalf("a selection's proof cannot be stored: %v", err)
	}
	var reloaded Proof
	if err := reloaded.Scan(stored); err != nil || reloaded.Check(selected) != nil {
		t.Fatalf("a stored selection proof does not prove its selection: %+v %v", reloaded, err)
	}
	var decoded Expectation
	encoded, _ := json.Marshal(selected)
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.Matches(selected) {
		t.Fatalf("selection did not survive its wire form: %s %v", encoded, err)
	}
}

// A caller writes a selection as one JSON object. These are the bytes the
// code-goblins driver writes for a Claude Code task and for a Codex task.
func TestLaunchSelectionReadsACallersFile(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for harness, entry := range map[types.AgentName]string{
		types.AgentClaude: `{"harness":"claude","model":"claude-opus-5-5","effort":"xhigh"}`,
		types.AgentCodex:  `{"harness":"codex","model":"gpt-6.1-sol","effort":"high","service_tier":"default"}`,
	} {
		source := `{"trusted_sha":"` + sha + `","profiles":{"fixer":[` + entry + `],"primary":[` + entry + `],"reviewer":[` + entry + `]},"apply":true}`
		path := filepath.Join(t.TempDir(), "launch-selection.json")
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		selection, err := Read(path)
		if err != nil {
			t.Fatalf("%s selection file refused: %v", harness, err)
		}
		if !selection.Apply || selection.TrustedSHA != sha || len(selection.Profiles) != 3 || selection.Profiles["primary"][0].Harness != harness {
			t.Fatalf("%s selection = %+v", harness, selection)
		}
	}
}
