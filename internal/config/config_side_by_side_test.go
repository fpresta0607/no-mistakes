package config

import (
	"strings"
	"testing"
)

func TestLoadGlobal_SideBySide(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("side_by_side: true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.SideBySide {
		t.Fatal("SideBySide = false, want it on")
	}
	if merged := Merge(cfg, &RepoConfig{}); !merged.SideBySide {
		t.Fatal("merged SideBySide = false, want the machine's setting")
	}
}

// Absent means off: the steps run one after another, as they always have.
func TestLoadGlobal_SideBySideDefaultsOff(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SideBySide || DefaultGlobalConfig().SideBySide || Merge(cfg, &RepoConfig{}).SideBySide {
		t.Fatal("side_by_side must be off unless the machine's config sets it")
	}
}

// How many agents run at once is the machine's to decide, never a
// repository's: neither a pushed branch nor the trusted copy can switch it on.
func TestLoadRepo_SideBySideIsNotARepoSetting(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("side_by_side: true\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(repo, repo, true)); merged.SideBySide {
		t.Fatal("a repository file switched side_by_side on")
	}
}

// test.documentation_paths decides which commits after the tested one may be
// pushed without the baseline running again, so a pushed branch that could
// set it could declare its own code documentation.
func TestEffectiveRepoConfig_DocumentationPathsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{DocumentationPaths: []string{"**", "*.go"}}}
	trusted := &RepoConfig{Test: TestRaw{DocumentationPaths: []string{"docs/**"}}}

	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		if got := strings.Join(effective.Test.DocumentationPaths, ","); got != "docs/**" {
			t.Fatalf("allow_repo_commands=%v: Test.DocumentationPaths = %q, want the trusted list", allowRepoCommands, got)
		}
	}
	if effective := EffectiveRepoConfig(pushed, nil, false); effective.Test.DocumentationPaths != nil {
		t.Fatalf("without a trusted copy the pushed list must be dropped, got %q", effective.Test.DocumentationPaths)
	}
	if effective := EffectiveRepoConfig(pushed, &RepoConfig{}, false); effective.Test.DocumentationPaths != nil {
		t.Fatalf("a trusted copy that sets no list must leave it unset, got %q", effective.Test.DocumentationPaths)
	}

	effective := EffectiveRepoConfig(pushed, trusted, false)
	effective.Test.DocumentationPaths[0] = "changed"
	if trusted.Test.DocumentationPaths[0] != "docs/**" {
		t.Fatal("trusted config was mutated through the effective copy")
	}
}

// An explicit empty list is a statement, not an absence: nothing in this
// repository is documentation, so every commit after the tested one runs the
// baseline again. It must survive the trusted copy and the merge as empty and
// must not fall back to the built-in list.
func TestDocumentationPaths_EmptyListIsNotTheBuiltInList(t *testing.T) {
	unset, err := LoadRepoFromBytes([]byte("commands:\n  test: go test ./...\n"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := LoadRepoFromBytes([]byte("test:\n  documentation_paths: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := LoadRepoFromBytes([]byte("test:\n  documentation_paths:\n    - \"docs/**\"\n    - \" *.rst \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	patterns := func(repo *RepoConfig) []string {
		return Merge(DefaultGlobalConfig(), EffectiveRepoConfig(repo, repo, false)).Test.DocumentationPatterns()
	}

	if got := strings.Join(patterns(unset), ","); got != strings.Join(DefaultDocumentationPaths, ",") || got == "" {
		t.Fatalf("unset documentation_paths = %q, want the built-in list %q", got, DefaultDocumentationPaths)
	}
	if got := patterns(empty); len(got) != 0 {
		t.Fatalf("an explicit empty documentation_paths resolved to %q, want no path counted as documentation", got)
	}
	if got := strings.Join(patterns(listed), ","); got != "docs/**,*.rst" {
		t.Fatalf("documentation_paths = %q, want the repository's list, trimmed, in place of the built-in one", got)
	}
}

func TestLoadRepo_DocumentationPathsRefusesUnusableEntries(t *testing.T) {
	cases := map[string]string{
		"empty entry":    "test:\n  documentation_paths:\n    - \"\"\n",
		"malformed glob": "test:\n  documentation_paths:\n    - \"docs/[\"\n",
		"backslash path": "test:\n  documentation_paths:\n    - \"docs\\\\**\"\n",
		"rooted path":    "test:\n  documentation_paths:\n    - \"/docs/**\"\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "test.documentation_paths") {
				t.Fatalf("LoadRepoFromBytes() error = %v, want test.documentation_paths refused", err)
			}
		})
	}
}

// The built-in list names prose formats only. A lockfile, a build file or a
// test fixture that happens to end in .txt is not documentation.
func TestDefaultDocumentationPathsNameProseOnly(t *testing.T) {
	for _, pattern := range DefaultDocumentationPaths {
		for _, risky := range []string{"txt", "json", "yaml", "yml", "toml", "lock", "**"} {
			if strings.HasSuffix(pattern, risky) {
				t.Fatalf("built-in documentation pattern %q would count %s files as documentation", pattern, risky)
			}
		}
	}
}
