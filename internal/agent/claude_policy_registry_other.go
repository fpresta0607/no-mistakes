//go:build !windows

package agent

// readClaudePolicyRegistry reads nothing: the policy registry is Windows-only.
func readClaudePolicyRegistry() ([]string, error) {
	return nil, nil
}
