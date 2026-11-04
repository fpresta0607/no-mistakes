package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsSafetyRefusal(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error"},
		{name: "native refusal", err: ErrSafetyRefusal, want: true},
		{name: "wrapped refusal", err: fmt.Errorf("agent fix: %w", ErrSafetyRefusal), want: true},
		{name: "quoted task data", err: errors.New(`invalid JSON (output snippet: "This content was flagged for possible cybersecurity risk.")`)},
		{name: "connection refused", err: errors.New("dial tcp: connection refused")},
		{name: "missing session", err: errors.New("session not found")},
		{name: "tool permission", err: errors.New("tool execution refused: permission denied")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsSafetyRefusal(test.err); got != test.want {
				t.Fatalf("IsSafetyRefusal(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestStructuredOutputQuotingRefusalIsNotSafetyRefusal(t *testing.T) {
	_, err := finalizeTextResult("codex", `{"finding":"This content was flagged for possible cybersecurity risk."`, []byte(`{"type":"object"}`), TokenUsage{})
	if err == nil || !IsStructuredOutputRejected(err) || IsSafetyRefusal(err) {
		t.Fatalf("error = %v, want an ordinary structured output rejection", err)
	}
}
