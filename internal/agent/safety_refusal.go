package agent

import (
	"errors"
	"strings"
)

var ErrSafetyRefusal = errors.New("native agent safety refusal")

// IsSafetyRefusal recognizes the explicit native refusal reported by Codex.
// Keep this specific to the provider's refusal, rather than ordinary errors
// such as a refused network connection or a tool permission failure.
func IsSafetyRefusal(err error) bool {
	return errors.Is(err, ErrSafetyRefusal)
}

func isNativeSafetyRefusal(message string) bool {
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "this content was flagged for possible cybersecurity risk") {
			return true
		}
	}
	return false
}
