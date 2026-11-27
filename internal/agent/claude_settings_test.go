package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// claudeSettingsFixture is a machine with no Claude Code settings anywhere:
// every source the probe consults lives under one temp directory, and the
// CLI's environment carries none of the routing variables.
type claudeSettingsFixture struct {
	root           string
	home           string
	managedDir     string
	preferencesDir string
	wslInterop     string
	cwd            string
	registry       []string
	registryErr    error
}

func newClaudeSettingsFixture(t *testing.T) *claudeSettingsFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &claudeSettingsFixture{
		root:           root,
		home:           filepath.Join(root, "home"),
		managedDir:     filepath.Join(root, "managed"),
		preferencesDir: filepath.Join(root, "managed-preferences"),
		wslInterop:     filepath.Join(root, "WSLInterop"),
		cwd:            filepath.Join(root, "work", "repo", "sub"),
	}
	if err := os.MkdirAll(fixture.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, key := range append([]string{"ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR", "WSL_DISTRO_NAME"}, claudeCloudProviderEnvVars...) {
		t.Setenv(key, "")
	}
	// Unset rather than set empty: Windows environment names are
	// case-insensitive and keep the spelling they were created with, so
	// creating both spellings here would make a later HTTPS_PROXY read back as
	// whichever spelling map iteration created first.
	for key := range claudeNetworkRouteEnvVars {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (f *claudeSettingsFixture) host() claudeSettingsHost {
	return claudeSettingsHost{
		homeDir:               func() (string, error) { return f.home, nil },
		managedDir:            f.managedDir,
		managedPreferencesDir: f.preferencesDir,
		wslInteropPath:        f.wslInterop,
		policyRegistry:        func() ([]string, error) { return f.registry, f.registryErr },
		ancestorsStopAt:       f.root,
	}
}

func writeClaudeSettingsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	routesToGateway = `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.example.test"}}`
	routesToBedrock = `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1","AWS_REGION":"us-east-1"}}`
	corporateProxy  = "http://proxy.corp.example:3128"
	routesToProxy   = `{"env":{"HTTPS_PROXY":"` + corporateProxy + `"}}`
)

// The probe target follows Claude Code's settings precedence as documented at
// code.claude.com/docs/en/settings: the CLI's environment, user settings
// (moved by CLAUDE_CONFIG_DIR), the server-managed cache, project and local
// settings unless suppressed, and managed settings, which always load. Any
// source that may route the CLI away from the endpoint in its environment, or
// cannot be read, means no probe.
func TestClaudeProviderProbeFollowsTheSettingsSources(t *testing.T) {
	for _, tc := range []struct {
		name         string
		arrange      func(t *testing.T, f *claudeSettingsFixture) []string
		extraArgs    []string
		readsProject bool
		wantEndpoint string
	}{
		{name: "default probes the Anthropic API", readsProject: true, wantEndpoint: "https://api.anthropic.com"},
		{name: "environment base URL is followed", wantEndpoint: "https://gateway.example.test", arrange: func(*testing.T, *claudeSettingsFixture) []string {
			return []string{"ANTHROPIC_BASE_URL=https://gateway.example.test"}
		}},
		{name: "environment bedrock", arrange: envOnly("CLAUDE_CODE_USE_BEDROCK=1")},
		{name: "environment vertex", arrange: envOnly("CLAUDE_CODE_USE_VERTEX=true")},
		{name: "environment foundry", arrange: envOnly("CLAUDE_CODE_USE_FOUNDRY=1")},
		{name: "environment mantle", arrange: envOnly("CLAUDE_CODE_USE_MANTLE=1")},
		{name: "environment anthropic aws", arrange: envOnly("CLAUDE_CODE_USE_ANTHROPIC_AWS=1")},
		{name: "environment bedrock off", arrange: envOnly("CLAUDE_CODE_USE_BEDROCK=0"), wantEndpoint: "https://api.anthropic.com"},
		{name: "settings argument", extraArgs: []string{"--settings", "routing.json"}},
		{name: "settings argument with value", extraArgs: []string{"--settings=routing.json"}},
		{name: "user settings set a base URL", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), routesToGateway)
			return nil
		}},
		{name: "user settings select bedrock", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), routesToBedrock)
			return nil
		}},
		{name: "user settings without routing still probe", wantEndpoint: "https://api.anthropic.com", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), `{"model":"opus","env":{"DISABLE_TELEMETRY":"1"}}`)
			return nil
		}},
		{name: "user file moved by CLAUDE_CONFIG_DIR", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			configDir := filepath.Join(f.root, "config")
			writeClaudeSettingsFile(t, filepath.Join(configDir, "settings.json"), routesToBedrock)
			return []string{"CLAUDE_CONFIG_DIR=" + configDir}
		}},
		{name: "home file not loaded once CLAUDE_CONFIG_DIR moves it", wantEndpoint: "https://api.anthropic.com", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), routesToBedrock)
			return []string{"CLAUDE_CONFIG_DIR=" + filepath.Join(f.root, "config")}
		}},
		{name: "server-managed cache", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "remote-settings.json"), routesToGateway)
			return nil
		}},
		{name: "managed file", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.managedDir, "managed-settings.json"), routesToGateway)
			return nil
		}},
		{name: "managed drop-in", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.managedDir, "managed-settings.d", "10-routing.json"), routesToBedrock)
			return nil
		}},
		{name: "hidden managed drop-in is not loaded", wantEndpoint: "https://api.anthropic.com", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.managedDir, "managed-settings.d", ".10-routing.json"), routesToBedrock)
			return nil
		}},
		{name: "managed policy helper", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.managedDir, "managed-settings.json"), `{"policyHelper":{"command":"/usr/local/bin/policy"}}`)
			return nil
		}},
		{name: "malformed user settings", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), `{"env":`)
			return nil
		}},
		{name: "user settings that are not an object", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), `[]`)
			return nil
		}},
		{name: "user settings env that is not an object", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), `{"env":["ANTHROPIC_BASE_URL"]}`)
			return nil
		}},
		{name: "unreadable user settings", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			if err := os.MkdirAll(filepath.Join(f.home, ".claude", "settings.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "project settings when not suppressed", readsProject: true, arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.cwd, ".claude", "settings.json"), routesToGateway)
			return nil
		}},
		{name: "project settings when suppressed", wantEndpoint: "https://api.anthropic.com", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.cwd, ".claude", "settings.json"), routesToGateway)
			writeClaudeSettingsFile(t, filepath.Join(f.cwd, ".claude", "settings.local.json"), routesToBedrock)
			return nil
		}},
		{name: "local settings in an ancestor", readsProject: true, arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.root, "work", ".claude", "settings.local.json"), routesToBedrock)
			return nil
		}},
		{name: "main checkout of a git worktree", readsProject: true, arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			mainCheckout := filepath.Join(f.root, "main")
			worktreeGitDir := filepath.Join(mainCheckout, ".git", "worktrees", "repo")
			writeClaudeSettingsFile(t, filepath.Join(worktreeGitDir, "commondir"), "../..\n")
			writeClaudeSettingsFile(t, filepath.Join(mainCheckout, ".claude", "settings.json"), routesToGateway)
			writeClaudeSettingsFile(t, filepath.Join(f.root, "work", "repo", ".git"), "gitdir: "+worktreeGitDir+"\n")
			return nil
		}},
		{name: "git worktree whose main checkout has no settings", readsProject: true, wantEndpoint: "https://api.anthropic.com", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			worktreeGitDir := filepath.Join(f.root, "main", ".git", "worktrees", "repo")
			writeClaudeSettingsFile(t, filepath.Join(worktreeGitDir, "commondir"), "../..\n")
			writeClaudeSettingsFile(t, filepath.Join(f.root, "work", "repo", ".git"), "gitdir: "+worktreeGitDir+"\n")
			return nil
		}},
		{name: "git worktree whose main checkout cannot be resolved", readsProject: true, arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.root, "work", "repo", ".git"), "gitdir: "+filepath.Join(f.root, "missing")+"\n")
			return nil
		}},
		{name: "policy registry routes", arrange: func(_ *testing.T, f *claudeSettingsFixture) []string {
			f.registry = []string{`{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`}
			return nil
		}},
		{name: "policy registry without routing still probes", wantEndpoint: "https://api.anthropic.com", arrange: func(_ *testing.T, f *claudeSettingsFixture) []string {
			f.registry = []string{`{"permissions":{"deny":["WebFetch"]}}`}
			return nil
		}},
		{name: "policy registry unreadable", arrange: func(_ *testing.T, f *claudeSettingsFixture) []string {
			f.registryErr = errors.New("access denied")
			return nil
		}},
		{name: "macOS managed preferences", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.preferencesDir, "operator", "com.anthropic.claudecode.plist"), "<plist/>")
			return nil
		}},
		{name: "user settings proxy the daemon lacks", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), routesToProxy)
			return nil
		}},
		{name: "user settings proxy the daemon shares", wantEndpoint: "https://api.anthropic.com", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			t.Setenv("HTTPS_PROXY", corporateProxy)
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), routesToProxy)
			return nil
		}},
		// Windows environment names are case-insensitive, so the daemon's
		// https_proxy is the settings file's HTTPS_PROXY there and the probe
		// follows it; elsewhere they are two variables and the CLI's differs.
		{name: "user settings proxy the daemon holds in lower case", wantEndpoint: map[bool]string{true: "https://api.anthropic.com"}[runtime.GOOS == "windows"], arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			if err := os.Unsetenv("HTTPS_PROXY"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("https_proxy", corporateProxy)
			writeClaudeSettingsFile(t, filepath.Join(f.home, ".claude", "settings.json"), routesToProxy)
			return nil
		}},
		{name: "run environment proxy the daemon lacks", arrange: envOnly("HTTPS_PROXY=" + corporateProxy)},
		{name: "certificate in the run environment the daemon shares", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			certificates := filepath.Join(f.root, "corporate-ca.pem")
			t.Setenv("NODE_EXTRA_CA_CERTS", certificates)
			return []string{"NODE_EXTRA_CA_CERTS=" + certificates}
		}},
		{name: "process wrapper in managed settings", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, filepath.Join(f.managedDir, "managed-settings.json"), `{"processWrapper":"/usr/local/bin/corp-launch"}`)
			return nil
		}},
		{name: "WSL distribution", arrange: envOnly("WSL_DISTRO_NAME=Ubuntu")},
		{name: "WSL interop", arrange: func(t *testing.T, f *claudeSettingsFixture) []string {
			writeClaudeSettingsFile(t, f.wslInterop, "enabled\n")
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			fixture := newClaudeSettingsFixture(t)
			var env []string
			if tc.arrange != nil {
				env = tc.arrange(t, fixture)
			}
			var probed string
			a := &claudeAgent{
				extraArgs:              tc.extraArgs,
				disableProjectSettings: !tc.readsProject,
				settingsHost:           fixture.host(),
				providerAnswers: func(_ context.Context, endpoint string) error {
					probed = endpoint
					return nil
				},
			}

			// Act
			probe := a.providerProbe(RunOpts{CWD: fixture.cwd, Env: env})

			// Assert
			if tc.wantEndpoint == "" {
				if probe != nil {
					t.Fatal("probe made, want none: a settings source may route the CLI elsewhere")
				}
				return
			}
			if probe == nil {
				t.Fatalf("no probe, want %s probed", tc.wantEndpoint)
			}
			if err := probe(context.Background()); err != nil || probed != tc.wantEndpoint {
				t.Fatalf("probed %q (err %v), want %q", probed, err, tc.wantEndpoint)
			}
		})
	}
}

func envOnly(entries ...string) func(*testing.T, *claudeSettingsFixture) []string {
	return func(*testing.T, *claudeSettingsFixture) []string { return entries }
}

func TestClaudeProviderProbeAbsentWithoutASettingsHost(t *testing.T) {
	// Arrange
	fixture := newClaudeSettingsFixture(t)
	a := &claudeAgent{providerAnswers: func(context.Context, string) error { return nil }}

	// Act
	probe := a.providerProbe(RunOpts{CWD: fixture.cwd})

	// Assert
	if probe != nil {
		t.Fatal("an agent that cannot locate the settings sources must keep the plain retry rule")
	}
}

// The probe is resolved when the retry needs it, so settings that route the
// CLI elsewhere by then keep the plain retry rule even though they were absent
// when the invocation started.
func TestClaudeAgent_SettingsReadAtRetryTimeKeepThePlainRetryRule(t *testing.T) {
	// Arrange
	defer withFastBackoff(t)()
	withFastProviderProbe(t)
	settings := newClaudeSettingsFixture(t)
	t.Setenv("NM_CLAUDE_STDIN_HELPER", "transient-then-succeed")
	t.Setenv("NM_CLAUDE_STDIN_COUNT_FILE", filepath.Join(t.TempDir(), "attempts"))
	t.Setenv("NM_CLAUDE_STDIN_TRANSIENT_REMAIN", "1")
	t.Setenv("NM_CLAUDE_STDIN_BANNER", `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","api_error_status":null,"result":"Request timed out"}`+"\n")
	a := newClaudeStdinHelperAgent(t)
	a.settingsHost = settings.host()
	probes := 0
	a.providerAnswers = func(context.Context, string) error {
		probes++
		return errors.New("dial tcp: i/o timeout")
	}
	attempts := 0

	// Act
	res, err := a.Run(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    settings.cwd,
		OnAttempt: func(Attempt) {
			attempts++
			if attempts == 1 {
				writeClaudeSettingsFile(t, filepath.Join(settings.home, ".claude", "settings.json"), routesToBedrock)
			}
		},
	})

	// Assert
	if err != nil || res == nil || string(res.Output) != `{"ok":true}` {
		t.Fatalf("Run = %+v, %v, want the retry's structured output", res, err)
	}
	if attempts != 2 || probes != 0 {
		t.Fatalf("attempts=%d probes=%d, want the retry on the plain rule with no probe", attempts, probes)
	}
}
