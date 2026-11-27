package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// claudeSettingsHost locates the Claude Code settings sources that live outside
// the working directory. Its zero value locates none, so no probe is made.
type claudeSettingsHost struct {
	// homeDir finds ~ when CLAUDE_CONFIG_DIR does not move the user settings.
	homeDir func() (string, error)
	// managedDir holds managed-settings.json and managed-settings.d.
	managedDir string
	// managedPreferencesDir is macOS's managed-preferences root, empty elsewhere.
	managedPreferencesDir string
	// wslInteropPath exists inside WSL, and is empty where WSL cannot apply.
	wslInteropPath string
	// policyRegistry reads the Windows policy Settings values.
	policyRegistry func() ([]string, error)
	// ancestorsStopAt ends the walk up from the working directory; empty walks
	// to the filesystem root.
	ancestorsStopAt string
}

func defaultClaudeSettingsHost() claudeSettingsHost {
	host := claudeSettingsHost{homeDir: os.UserHomeDir, policyRegistry: readClaudePolicyRegistry}
	switch runtime.GOOS {
	case "windows":
		host.managedDir = `C:\Program Files\ClaudeCode`
	case "darwin":
		host.managedDir = "/Library/Application Support/ClaudeCode"
		host.managedPreferencesDir = "/Library/Managed Preferences"
	default:
		host.managedDir = "/etc/claude-code"
		host.wslInteropPath = "/proc/sys/fs/binfmt_misc/WSLInterop"
	}
	return host
}

// claudeRouteFollow says how far the probe can follow a network setting.
type claudeRouteFollow int

const (
	// claudeRouteFollowedFromDaemon: the probe's client follows the daemon
	// process's own value, so only a different value, or one set on one side
	// only, routes the CLI where the probe does not go.
	claudeRouteFollowedFromDaemon claudeRouteFollow = iota
	// claudeRouteNeverFollowed: the probe reads no value, so any non-empty one
	// routes the CLI where the probe does not go.
	claudeRouteNeverFollowed
)

// claudeNetworkRouteEnvVars are the proxy, certificate and launcher settings
// of Claude Code's network configuration (code.claude.com/docs/en/network-config),
// plus the Anthropic AWS endpoint. Set in the CLI's environment or in a
// settings env block in a way the probe cannot follow, they mean no probe.
var claudeNetworkRouteEnvVars = map[string]claudeRouteFollow{
	"HTTPS_PROXY":                       claudeRouteFollowedFromDaemon,
	"https_proxy":                       claudeRouteFollowedFromDaemon,
	"HTTP_PROXY":                        claudeRouteFollowedFromDaemon,
	"http_proxy":                        claudeRouteFollowedFromDaemon,
	"NO_PROXY":                          claudeRouteFollowedFromDaemon,
	"no_proxy":                          claudeRouteFollowedFromDaemon,
	"NODE_EXTRA_CA_CERTS":               claudeRouteNeverFollowed,
	"CLAUDE_CODE_CERT_STORE":            claudeRouteNeverFollowed,
	"CLAUDE_CODE_CLIENT_CERT":           claudeRouteNeverFollowed,
	"CLAUDE_CODE_CLIENT_KEY":            claudeRouteNeverFollowed,
	"CLAUDE_CODE_CLIENT_KEY_PASSPHRASE": claudeRouteNeverFollowed,
	"NODE_TLS_REJECT_UNAUTHORIZED":      claudeRouteNeverFollowed,
	"CLAUDE_CODE_PROCESS_WRAPPER":       claudeRouteNeverFollowed,
	"ANTHROPIC_AWS_BASE_URL":            claudeRouteNeverFollowed,
}

// claudeRouteDiverts reports whether the network setting name, set to value
// (isSet false when unset), routes the CLI where the probe cannot follow.
// daemonEnv is the daemon process's own environment.
func claudeRouteDiverts(name, value string, isSet bool, daemonEnv map[string]string) bool {
	if claudeNetworkRouteEnvVars[name] == claudeRouteNeverFollowed {
		return value != ""
	}
	daemonValue, isDaemonSet := daemonEnv[claudeEnvName(name)]
	return isSet != isDaemonSet || value != daemonValue
}

// claudeEnvName is the key an environment variable is looked up by. Windows
// environment names are case-insensitive and a variable keeps the casing it was
// created with, so there https_proxy and HTTPS_PROXY are one variable.
func claudeEnvName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// claudeEnvMap indexes environment entries by claudeEnvName, the last entry
// winning as os/exec does.
func claudeEnvMap(entries []string) map[string]string {
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[claudeEnvName(key)] = value
		}
	}
	return env
}

// claudeSettingsDocument is one settings source to check; a managed one may
// also delegate to a policyHelper.
type claudeSettingsDocument struct {
	path    string
	managed bool
}

// leaveRoute reports whether every Claude Code settings source the CLI loads
// is absent or readable and sets neither ANTHROPIC_BASE_URL nor a cloud
// provider switch, so the CLI's own environment decides its endpoint, and
// whether neither that environment nor any settings source sets a network
// route the probe cannot follow (claudeNetworkRouteEnvVars). env is
// the CLI's environment and cwd its working directory; readsProject is false
// when the project and local sources are suppressed. A source that cannot be
// located or read means false.
func (h claudeSettingsHost) leaveRoute(env map[string]string, cwd string, readsProject bool) bool {
	if h.homeDir == nil || h.managedDir == "" {
		return false
	}
	daemonEnv := claudeEnvMap(os.Environ())
	for name := range claudeNetworkRouteEnvVars {
		value, isSet := env[claudeEnvName(name)]
		if claudeRouteDiverts(name, value, isSet, daemonEnv) {
			return false
		}
	}
	if h.wslInteropPath != "" {
		if env["WSL_DISTRO_NAME"] != "" || !claudeSettingsAbsent(h.wslInteropPath) {
			return false
		}
	}
	if h.managedPreferencesDir != "" && !claudeNoManagedPreferences(h.managedPreferencesDir) {
		return false
	}
	if h.policyRegistry != nil {
		values, err := h.policyRegistry()
		if err != nil {
			return false
		}
		for _, value := range values {
			if !claudeSettingsLeaveRoute([]byte(value), true, daemonEnv) {
				return false
			}
		}
	}
	configDir := env["CLAUDE_CONFIG_DIR"]
	if configDir == "" {
		home, err := h.homeDir()
		if err != nil || home == "" {
			return false
		}
		configDir = filepath.Join(home, ".claude")
	}
	documents := []claudeSettingsDocument{
		{path: filepath.Join(configDir, "settings.json")},
		{path: filepath.Join(configDir, "remote-settings.json"), managed: true},
		{path: filepath.Join(h.managedDir, "managed-settings.json"), managed: true},
	}
	dropIns := filepath.Join(h.managedDir, "managed-settings.d")
	entries, err := os.ReadDir(dropIns)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for _, entry := range entries {
		if name := entry.Name(); !strings.HasPrefix(name, ".") && filepath.Ext(name) == ".json" {
			documents = append(documents, claudeSettingsDocument{path: filepath.Join(dropIns, name), managed: true})
		}
	}
	if readsProject {
		projectDirs, ok := claudeProjectSettingsDirs(cwd, h.ancestorsStopAt)
		if !ok {
			return false
		}
		for _, dir := range projectDirs {
			documents = append(documents,
				claudeSettingsDocument{path: filepath.Join(dir, ".claude", "settings.json")},
				claudeSettingsDocument{path: filepath.Join(dir, ".claude", "settings.local.json")})
		}
	}
	for _, document := range documents {
		data, err := os.ReadFile(document.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !claudeSettingsLeaveRoute(data, document.managed, daemonEnv) {
			return false
		}
	}
	return true
}

// claudeSettingsLeaveRoute reports whether one settings document is a JSON
// object with no processWrapper launcher whose env, if present, is an object
// that sets neither ANTHROPIC_BASE_URL nor a cloud provider switch, nor a
// network route the probe cannot follow from daemonEnv. A managed document
// must also name no policyHelper, whose settings cannot be read ahead of the
// CLI.
func claudeSettingsLeaveRoute(data []byte, managed bool, daemonEnv map[string]string) bool {
	var settings map[string]json.RawMessage
	if json.Unmarshal(data, &settings) != nil || settings == nil {
		return false
	}
	if _, ok := settings["policyHelper"]; ok && managed {
		return false
	}
	if _, ok := settings["processWrapper"]; ok {
		return false
	}
	raw, ok := settings["env"]
	if !ok {
		return true
	}
	var block map[string]json.RawMessage
	if json.Unmarshal(raw, &block) != nil || block == nil {
		return false
	}
	env := make(map[string]json.RawMessage, len(block))
	for name, raw := range block {
		env[claudeEnvName(name)] = raw
	}
	if _, ok := env[claudeEnvName("ANTHROPIC_BASE_URL")]; ok {
		return false
	}
	for _, key := range claudeCloudProviderEnvVars {
		if _, ok := env[claudeEnvName(key)]; ok {
			return false
		}
	}
	for name, raw := range env {
		if _, ok := claudeNetworkRouteEnvVars[name]; !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || claudeRouteDiverts(name, value, true, daemonEnv) {
			return false
		}
	}
	return true
}

// claudeProjectSettingsDirs returns cwd, each of its ancestors up to stopAt
// and, inside a linked git worktree, the main checkout's root: every directory
// whose .claude project settings are checked. It reports false when cwd or the
// worktree's main checkout cannot be resolved.
func claudeProjectSettingsDirs(cwd, stopAt string) ([]string, bool) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return nil, false
	}
	var dirs []string
	hasGitEntry := false
	for {
		dirs = append(dirs, dir)
		if !hasGitEntry {
			info, err := os.Stat(filepath.Join(dir, ".git"))
			switch {
			case err == nil:
				hasGitEntry = true
				if !info.IsDir() {
					mainCheckout, ok := claudeMainCheckout(dir)
					if !ok {
						return nil, false
					}
					if mainCheckout != "" {
						dirs = append(dirs, mainCheckout)
					}
				}
			case !errors.Is(err, fs.ErrNotExist):
				return nil, false
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir || dir == stopAt {
			return dirs, true
		}
		dir = parent
	}
}

// claudeMainCheckout resolves the main checkout of the worktree whose .git
// file is in dir, the directory holding its common git directory. It returns
// "" for a .git file that is not a linked worktree's (a submodule or a
// separate git directory), and false when the pointer cannot be followed.
func claudeMainCheckout(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", false
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	gitDir = claudeResolvePath(dir, strings.TrimSpace(gitDir))
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return "", false
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	return filepath.Dir(claudeResolvePath(gitDir, strings.TrimSpace(string(common)))), true
}

func claudeResolvePath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

// claudeNoManagedPreferences reports whether no com.anthropic.claudecode
// managed-preferences plist sits in dir or a per-user directory under it. Its
// settings have no documented file shape to read.
func claudeNoManagedPreferences(dir string) bool {
	const plist = "com.anthropic.claudecode.plist"
	if !claudeSettingsAbsent(filepath.Join(dir, plist)) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && !claudeSettingsAbsent(filepath.Join(dir, entry.Name(), plist)) {
			return false
		}
	}
	return true
}

func claudeSettingsAbsent(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}
