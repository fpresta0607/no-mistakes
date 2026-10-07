package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
)

const launchAssertionPushOptionPrefix = "no-mistakes.launch-assertion="

func requireDaemonHonorsLaunchAssertion(client closingIssueRefsUpdateClient, expected *launchassert.Expectation) error {
	if expected == nil {
		return nil
	}
	var result ipc.ProbeLaunchAssertionResult
	if err := client.Call(ipc.MethodProbeLaunchAssertion, &ipc.ProbeLaunchAssertionParams{LaunchAssertion: expected}, &result); err != nil {
		return fmt.Errorf("running daemon cannot honor --launch-assertion; launch refused before custody changes: %w", err)
	}
	if result.AssertionDigest != expected.Digest() {
		return fmt.Errorf("running daemon did not acknowledge the captured launch assertion; launch refused before custody changes")
	}
	return nil
}

func formatLaunchAssertionPushOptions(expected *launchassert.Expectation) ([]string, error) {
	if expected == nil {
		return nil, nil
	}
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	if len(data) > launchassert.MaxBytes {
		return nil, fmt.Errorf("launch assertion exceeds the push-option bound")
	}
	return []string{launchAssertionPushOptionPrefix + base64.RawURLEncoding.EncodeToString(data)}, nil
}

func parseLaunchAssertionPushOptions(options []string) (*launchassert.Expectation, error) {
	var expected *launchassert.Expectation
	for _, option := range options {
		encoded, hasAssertion := strings.CutPrefix(option, launchAssertionPushOptionPrefix)
		if !hasAssertion {
			continue
		}
		if expected != nil || len(encoded) > base64.RawURLEncoding.EncodedLen(launchassert.MaxBytes) {
			return nil, fmt.Errorf("duplicate or oversized launch assertion push option")
		}
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("unreadable launch assertion push option")
		}
		var captured launchassert.Expectation
		if err := json.Unmarshal(data, &captured); err != nil {
			return nil, err
		}
		expected = &captured
	}
	return expected, nil
}
