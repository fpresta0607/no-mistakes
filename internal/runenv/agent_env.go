package runenv

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// agentEnvNames are the only variables `axi run --agent-env` may set: the
// package managers' release-cutoff settings. The list is exact-case, so a
// caller cannot reach any other variable through a run.
var agentEnvNames = []string{"PIP_UPLOADED_PRIOR_TO", "UV_EXCLUDE_NEWER", "npm_config_before"}

// ParseAgentEnv admits NAME=VALUE entries (split on the first "=") and returns
// them as a map, or nil when there are none. Values pass through exactly as
// given.
func ParseAgentEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("--agent-env %q must be NAME=VALUE", pair)
		}
		if _, seen := values[name]; seen {
			return nil, fmt.Errorf("--agent-env name %q is given more than once", name)
		}
		values[name] = value
	}
	if err := ValidateAgentEnv(values); err != nil {
		return nil, err
	}
	return values, nil
}

// ValidateAgentEnv refuses a map that names a variable outside the accepted
// list, or holds an empty value or one with a control character (push options
// are line-oriented).
func ValidateAgentEnv(values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.Contains(agentEnvNames, name) {
			return fmt.Errorf("--agent-env name %q is not accepted; accepted names: %s", name, strings.Join(agentEnvNames, ", "))
		}
		value := values[name]
		if value == "" {
			return fmt.Errorf("--agent-env %s requires a nonempty value", name)
		}
		if strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("--agent-env %s value must not contain control characters", name)
		}
	}
	return nil
}

// FormatAgentEnv returns the admitted pairs as NAME=VALUE entries in sorted
// name order.
func FormatAgentEnv(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+values[name])
	}
	return pairs
}
