package authoredvalidation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func requestFixture() Request {
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	return Request{
		PredecessorID: "predecessor", SnapshotDigest: strings.Repeat("a", 64), RepoID: "repository", Branch: "feature",
		CallerDir: "/caller", CandidateSHA: strings.Repeat("b", 40), CandidateTree: strings.Repeat("c", 40),
		LaunchNonce: "original-launch", ValidationGeneration: "original-generation", IntentDigest: strings.Repeat("d", 64),
		ControllerPolicy:             ControllerPolicy{SHA256: strings.Repeat("e", 64), Class: "ordinary", ReviewCycles: 2},
		LaunchAssertion:              &launchassert.Expectation{TrustedSHA: strings.Repeat("f", 40), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}},
		HistoricalSourceCompleteness: "unknown", Reason: "Validate this declared authored candidate",
	}
}

func TestRequestRequiresAnExplicitUncertaintyAndBoundPolicy(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Request)
	}{
		{"missing predecessor", func(request *Request) { request.PredecessorID = "" }},
		{"short commit", func(request *Request) { request.CandidateSHA = "1234567" }},
		{"missing tree", func(request *Request) { request.CandidateTree = "" }},
		{"claimed recovery", func(request *Request) { request.HistoricalSourceCompleteness = "recovered" }},
		{"missing policy", func(request *Request) { request.ControllerPolicy.SHA256 = "" }},
		{"missing launch", func(request *Request) { request.LaunchAssertion = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := requestFixture()
			test.change(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("accepted an incomplete admission")
			}
		})
	}
	if err := requestFixture().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsAmbiguousAndBroaderAdmissionInputs(t *testing.T) {
	payload, err := json.Marshal(requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"Candidate_SHA":"x"`, `"candidate_sha":"x"`, `"repair_limit":3`, `"launch_assertion":null`} {
		ambiguous := strings.TrimSuffix(string(payload), "}") + "," + field + "}"
		if _, err := Decode([]byte(ambiguous)); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	if _, err := Decode(payload); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsNonUTF8InsteadOfChangingTheDeclaredInput(t *testing.T) {
	payload, err := json.Marshal(requestFixture())
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(payload, []byte("Validate"), []byte{0xff}, 1)
	if _, err := Decode(payload); err == nil {
		t.Fatal("silently rewrote invalid UTF-8 in an admission")
	}
}

func TestTerminalRequestBindsAnExplicitIndependentSelectionAndUnknownSourceAcceptance(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Request)
	}{
		{"accepted", func(*Request) {}},
		{"missing acceptance", func(request *Request) { request.AcceptUnknownSource = false }},
		{"inherited generation", func(request *Request) { request.TerminalGeneration = request.ValidationGeneration }},
		{"whitespace generation", func(request *Request) { request.TerminalGeneration = " " }},
		{"oversized generation", func(request *Request) { request.TerminalGeneration = strings.Repeat("g", 129) }},
		{"implicit selection", func(request *Request) { request.LaunchAssertion.Apply = false }},
		{"missing selection", func(request *Request) { request.LaunchAssertion = nil }},
		{"claimed recovery", func(request *Request) { request.HistoricalSourceCompleteness = "clean" }},
		{"authored acceptance", func(request *Request) { request.TerminalGeneration = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := requestFixture()
			request.TerminalGeneration, request.AcceptUnknownSource, request.LaunchAssertion.Apply = "independent-lane", true, true
			test.change(&request)
			payload, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}

			decoded, err := Decode(payload)

			if test.name == "accepted" {
				if err != nil || decoded.SuccessorGeneration() != "independent-lane" || decoded.ValidationGeneration != "original-generation" {
					t.Fatalf("successor lost independent or historical generation: %+v %v", decoded, err)
				}
			} else if err == nil {
				t.Fatal("accepted an unbound terminal admission")
			}
		})
	}
}
