package config

import (
	"strings"
	"testing"
	"time"
)

// TestEffectiveRepoConfig_LiveCheckRulesTrustedOnly proves the two repository
// settings that decide whether and how the live check runs are honored only
// from the trusted default-branch copy.
//
// test.surface_paths switches the evidence agent off for a change that touches
// none of the listed paths, so a pushed branch that could set it could list a
// path it does not touch and skip its own live check. test.environment is
// injected into the evidence agent's prompt, exactly like test.instructions.
func TestEffectiveRepoConfig_LiveCheckRulesTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{
		SurfacePaths: []string{"never/touched/**"},
		Environment:  "there is nothing to start",
	}}
	trusted := &RepoConfig{Test: TestRaw{
		SurfacePaths: []string{"src/**", "*.html"},
		Environment:  "scripts/live-env.ps1 brings up the app on a seeded database",
	}}

	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		if got := strings.Join(effective.Test.SurfacePaths, ","); got != "src/**,*.html" {
			t.Fatalf("allow_repo_commands=%v: Test.SurfacePaths = %q, want the trusted list", allowRepoCommands, got)
		}
		if effective.Test.Environment != trusted.Test.Environment {
			t.Fatalf("allow_repo_commands=%v: Test.Environment = %q, want the trusted value", allowRepoCommands, effective.Test.Environment)
		}
	}

	// Without a trusted copy the pushed values are dropped, never used as a
	// fallback: an unset list is today's behavior, the live check on every change.
	effective := EffectiveRepoConfig(pushed, nil, false)
	if len(effective.Test.SurfacePaths) != 0 {
		t.Fatalf("without a trusted copy the pushed surface list must be dropped, got %q", effective.Test.SurfacePaths)
	}
	if effective.Test.Environment != "" {
		t.Fatalf("without a trusted copy the pushed environment must be dropped, got %q", effective.Test.Environment)
	}

	// The trusted list is copied, so a later edit of the effective config
	// cannot reach the trusted one another run reads.
	effective = EffectiveRepoConfig(pushed, trusted, false)
	effective.Test.SurfacePaths[0] = "changed"
	if trusted.Test.SurfacePaths[0] != "src/**" {
		t.Fatal("trusted config was mutated through the effective copy")
	}
	if pushed.Test.SurfacePaths[0] != "never/touched/**" {
		t.Fatal("pushed config was mutated")
	}
}

func TestLoadRepo_TestSurfacePaths(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("test:\n  surface_paths:\n    - \"src/**\"\n    - \"*.html\"\n    - cmd/app/main.go\n  environment: |\n    scripts/live-env.ps1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(cfg.Test.SurfacePaths, ","); got != "src/**,*.html,cmd/app/main.go" {
		t.Fatalf("Test.SurfacePaths = %q", got)
	}
	if !strings.Contains(cfg.Test.Environment, "scripts/live-env.ps1") {
		t.Fatalf("Test.Environment = %q", cfg.Test.Environment)
	}
}

// A list that cannot be matched must fail the config rather than silently
// match nothing, because matching nothing is exactly what skips the live check.
func TestLoadRepo_TestSurfacePathsRefusesUnusableEntries(t *testing.T) {
	cases := map[string]string{
		"empty entry":      "test:\n  surface_paths:\n    - \"src/**\"\n    - \"\"\n",
		"blank entry":      "test:\n  surface_paths:\n    - \"   \"\n",
		"malformed glob":   "test:\n  surface_paths:\n    - \"src/[\"\n",
		"bare subtree":     "test:\n  surface_paths:\n    - \"/**\"\n",
		"backslash path":   "test:\n  surface_paths:\n    - \"src\\\\app\\\\**\"\n",
		"leading dot path": "test:\n  surface_paths:\n    - \"./src/**\"\n",
		"rooted path":      "test:\n  surface_paths:\n    - \"/src/**\"\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "test.surface_paths") {
				t.Fatalf("LoadRepoFromBytes() error = %v, want test.surface_paths refused", err)
			}
		})
	}
}

// The resolved list and environment come from the repository alone. Global
// config shares the test block's shape but describes no repository.
func TestMerge_LiveCheckRulesComeFromTheRepositoryOnly(t *testing.T) {
	global := DefaultGlobalConfig()
	global.Test.SurfacePaths = []string{"global/**"}
	global.Test.Environment = "global environment"

	cfg := Merge(global, &RepoConfig{})
	if len(cfg.Test.SurfacePaths) != 0 || cfg.Test.Environment != "" {
		t.Fatalf("with no repository setting the live check rules must be off, got surface=%q environment=%q", cfg.Test.SurfacePaths, cfg.Test.Environment)
	}

	cfg = Merge(global, &RepoConfig{Test: TestRaw{
		SurfacePaths: []string{" src/** ", "*.html"},
		Environment:  "  scripts/live-env.ps1  \n",
	}})
	if got := strings.Join(cfg.Test.SurfacePaths, ","); got != "src/**,*.html" {
		t.Fatalf("Test.SurfacePaths = %q, want the repository list trimmed", got)
	}
	if cfg.Test.Environment != "scripts/live-env.ps1" {
		t.Fatalf("Test.Environment = %q, want the repository value trimmed", cfg.Test.Environment)
	}
}

func TestLoadGlobal_TestLiveCheckBudget(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("test_live_check_budget: \"15m\"\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.TestLiveCheckBudget != 15*time.Minute {
		t.Fatalf("TestLiveCheckBudget = %s, want 15m", cfg.TestLiveCheckBudget)
	}
	if merged := Merge(cfg, &RepoConfig{}); merged.TestLiveCheckBudget != 15*time.Minute {
		t.Fatalf("merged TestLiveCheckBudget = %s, want 15m", merged.TestLiveCheckBudget)
	}
}

// Absent means off: the live check keeps today's only bound, test_agent_timeout.
func TestLoadGlobal_TestLiveCheckBudgetDefaultsOff(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.TestLiveCheckBudget != 0 {
		t.Fatalf("TestLiveCheckBudget = %s, want 0 when unset", cfg.TestLiveCheckBudget)
	}
	if DefaultGlobalConfig().TestLiveCheckBudget != 0 {
		t.Fatal("the default global config must leave the live check budget off")
	}
	if merged := Merge(cfg, &RepoConfig{}); merged.TestLiveCheckBudget != 0 {
		t.Fatalf("merged TestLiveCheckBudget = %s, want 0 when unset", merged.TestLiveCheckBudget)
	}
}

// The budget is a cap below the agent's own limit. A value above it could only
// lengthen a live check, which is what the setting exists to prevent.
func TestLoadGlobal_TestLiveCheckBudgetNeverAboveTheAgentLimit(t *testing.T) {
	cases := map[string]string{
		"above the default limit":    "test_live_check_budget: \"31m\"\n",
		"above a configured limit":   "test_agent_timeout: \"10m\"\ntest_live_check_budget: \"15m\"\n",
		"above a limit set after it": "test_live_check_budget: \"15m\"\ntest_agent_timeout: \"10m\"\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "test_live_check_budget") || !strings.Contains(err.Error(), "test_agent_timeout") {
				t.Fatalf("LoadGlobalFromBytes() error = %v, want a budget above test_agent_timeout refused by name", err)
			}
		})
	}

	// Equal to the limit is the boundary and is accepted.
	cfg, err := LoadGlobalFromBytes([]byte("test_agent_timeout: \"20m\"\ntest_live_check_budget: \"20m\"\n"))
	if err != nil {
		t.Fatalf("a budget equal to test_agent_timeout must load: %v", err)
	}
	if cfg.TestLiveCheckBudget != 20*time.Minute {
		t.Fatalf("TestLiveCheckBudget = %s, want 20m", cfg.TestLiveCheckBudget)
	}
}

func TestLoadGlobal_TestLiveCheckBudgetRefusesUnusableValues(t *testing.T) {
	for _, value := range []string{"0", "-5m", "soon"} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte("test_live_check_budget: \"" + value + "\"\n"))
			if err == nil || !strings.Contains(err.Error(), "test_live_check_budget") {
				t.Fatalf("LoadGlobalFromBytes(%q) error = %v, want the value refused by name", value, err)
			}
		})
	}
}

// A repository names no machine budget: the key is not a repository setting.
func TestLoadRepo_TestLiveCheckBudgetIsNotARepoSetting(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("test_live_check_budget: \"1m\"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	global := DefaultGlobalConfig()
	if merged := Merge(global, cfg); merged.TestLiveCheckBudget != 0 {
		t.Fatalf("a repository file set the machine's live check budget to %s", merged.TestLiveCheckBudget)
	}
}
