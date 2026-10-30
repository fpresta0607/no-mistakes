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
		{name: "native refusal", err: errors.New("This content was flagged for possible cybersecurity risk."), want: true},
		{name: "wrapped refusal", err: fmt.Errorf("agent fix: %w", errors.New("THIS CONTENT WAS FLAGGED FOR POSSIBLE CYBERSECURITY RISK.")), want: true},
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
