package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
)

func cliLaunchExpectation() *launchassert.Expectation {
	profile := agentcfg.Selection{Harness: "codex", Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	return &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{
		"primary": {profile}, "reviewer": {profile}, "fixer": {profile},
	}}
}

type launchAssertionProbeClient struct {
	shouldRefuse   bool
	shouldMismatch bool
	hasCalled      bool
}

func (client *launchAssertionProbeClient) Call(method string, params interface{}, result interface{}) error {
	client.hasCalled = true
	if method != ipc.MethodProbeLaunchAssertion || client.shouldRefuse {
		return &ipc.RPCError{Code: ipc.ErrMethodNotFound, Message: "method not found"}
	}
	request, isRequest := params.(*ipc.ProbeLaunchAssertionParams)
	response, isResponse := result.(*ipc.ProbeLaunchAssertionResult)
	if !isRequest || !isResponse {
		return fmt.Errorf("unexpected probe shape")
	}
	response.AssertionDigest = request.LaunchAssertion.Digest()
	if client.shouldMismatch {
		response.AssertionDigest = "ignored"
	}
	return nil
}

func TestLaunchAssertionRefusesOldDaemonBeforeCustody(t *testing.T) {
	for _, client := range []*launchAssertionProbeClient{{shouldRefuse: true}, {shouldMismatch: true}, {}} {
		err := requireDaemonHonorsLaunchAssertion(client, cliLaunchExpectation())
		shouldReject := client.shouldRefuse || client.shouldMismatch
		if !client.hasCalled || (err != nil) != shouldReject {
			t.Fatalf("probe=%+v error=%v", client, err)
		}
	}
	legacy := &launchAssertionProbeClient{shouldRefuse: true}
	if err := requireDaemonHonorsLaunchAssertion(legacy, nil); err != nil || legacy.hasCalled {
		t.Fatal("legacy unrequested launch acquired a new capability dependency")
	}
}

func TestLaunchAssertionCommandRefusesOldDaemonBeforeCustody(t *testing.T) {
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	writeGlobalConfig(t, "intent:\n  publish_intent: true\n")
	before, err := git.Run(context.Background(), ".", "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "expected.json")
	data, err := json.Marshal(cliLaunchExpectation())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	command := newAxiRunCmd()
	command.SetContext(context.Background())
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.ParseFlags([]string{"--launch-assertion", path}); err != nil {
		t.Fatal(err)
	}
	err = runAxiRunWithLaunchProof(command, false, nil, "assert source", "", false, "nonce", "generation", defaultAxiWait)
	if err == nil || !strings.Contains(output.String(), "cannot honor --launch-assertion") || len(*launched) != 0 {
		t.Fatalf("old daemon received launch work: %v %v %s", *launched, err, output.String())
	}
	after, err := git.Run(context.Background(), ".", "show-ref")
	if err != nil || after != before {
		t.Fatalf("capability refusal changed branch custody: %v", err)
	}
}

func TestLaunchAssertionMissingFileRefusesBeforeOpeningNativeHome(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.json")} {
		root := filepath.Join(t.TempDir(), "must-not-exist")
		t.Setenv("NM_HOME", root)
		command := newAxiRunCmd()
		command.SetContext(context.Background())
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		if err := command.ParseFlags([]string{"--launch-assertion", path, "--launch-nonce", "nonce", "--validation-generation", "generation"}); err != nil {
			t.Fatal(err)
		}
		if err := runAxiRunWithLaunchProof(command, false, nil, "required proof", "", false, "nonce", "generation", defaultAxiWait); err == nil {
			t.Fatal("missing required file was accepted")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("missing proof opened or created the native home")
		}
	}
}

func TestLaunchAssertionPushOptionsPreserveCapturedExpectation(t *testing.T) {
	expected := cliLaunchExpectation()
	options, err := formatLaunchAssertionPushOptions(expected)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := parseLaunchAssertionPushOptions(options)
	if err != nil || !captured.Matches(expected) {
		t.Fatalf("push expectation = %+v %v", captured, err)
	}
	for _, invalid := range [][]string{
		append(options, options...),
		{launchAssertionPushOptionPrefix},
		{launchAssertionPushOptionPrefix + "bad"},
		{launchAssertionPushOptionPrefix + strings.Repeat("a", launchassert.MaxBytes*2)},
	} {
		if _, err := parseLaunchAssertionPushOptions(invalid); err == nil {
			t.Fatal("invalid required proof option was accepted")
		}
	}
}

func TestLaunchAssertionReceiptRendersSourceAndEffectiveProfiles(t *testing.T) {
	expected := cliLaunchExpectation()
	expected.Profiles["primary"] = append(expected.Profiles["primary"], agentcfg.Selection{
		Harness: "codex", Model: "fixture/fallback", Effort: agentcfg.EffortHigh, ServiceTier: "fast",
	})
	expected.Profiles["reviewer_after_round"] = []agentcfg.Selection{{
		Harness: "claude", Model: "claude-reviewer", Effort: agentcfg.EffortMedium,
	}}
	expected.Profiles["fixer_after_round"] = []agentcfg.Selection{{
		Harness: "codex", Model: "fixture-fixer", Effort: agentcfg.EffortLow, ServiceTier: "fast",
	}}
	proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	command := newAxiRunCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	emitLaunchReceipt(command, ipc.LaunchReceipt{RunID: "fixture", LaunchAssertionProof: proof})
	for _, fact := range []string{expected.TrustedSHA, expected.Digest(), "primary", "reviewer", "fixer", "xhigh", "default"} {
		if !strings.Contains(output.String(), fact) {
			t.Fatalf("source-bound receipt omitted %s: %s", fact, output.String())
		}
	}
	expectedProof := fmt.Sprintf(`  launch_assertion_proof:
    assertion_digest: %s
    trusted_sha: %s
    profiles:
      fixer[1]{harness,model,effort,service_tier}:
        codex,fixture,xhigh,default
      fixer_after_round[1]{harness,model,effort,service_tier}:
        codex,fixture-fixer,low,fast
      primary[2]{harness,model,effort,service_tier}:
        codex,fixture,xhigh,default
        codex,fixture/fallback,high,fast
      reviewer[1]{harness,model,effort,service_tier}:
        codex,fixture,xhigh,default
      reviewer_after_round[1]{harness,model,effort,service_tier}:
        claude,claude-reviewer,medium,""
`, expected.Digest(), expected.TrustedSHA)
	if !strings.HasSuffix(output.String(), expectedProof) {
		t.Fatalf("source-bound receipt changed the structured proof: %s", output.String())
	}
}
