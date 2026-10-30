package agent

import "strings"

// IsSafetyRefusal recognizes the explicit native refusal reported by Codex.
// Keep this specific to the provider's refusal, rather than ordinary errors
// such as a refused network connection or a tool permission failure.
func IsSafetyRefusal(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "this content was flagged for possible cybersecurity risk")
}
