//go:build windows

package agent

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// readClaudePolicyRegistry returns the JSON Settings values of Claude Code's
// machine and user policy keys, skipping a key or value that does not exist.
func readClaudePolicyRegistry() ([]string, error) {
	var values []string
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		key, err := registry.OpenKey(root, `SOFTWARE\Policies\ClaudeCode`, registry.QUERY_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		value, _, err := key.GetStringValue("Settings")
		key.Close()
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}
