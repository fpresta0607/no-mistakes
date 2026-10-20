package config

import (
	"strings"
	"testing"
)

func TestLoadGlobal_TestCommandWrapper(t *testing.T) {
	data := `test_command_wrapper:
  command: ["cfo", "gate", "turn", "--as", "{repo} {branch}, run {run_short}", "--"]
  refusal_prefix: "cfo gate turn: "
`
	cfg, err := LoadGlobalFromBytes([]byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"cfo", "gate", "turn", "--as", "{repo} {branch}, run {run_short}", "--"}
	if strings.Join(cfg.TestCommandWrapper.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("TestCommandWrapper.Command = %q, want %q", cfg.TestCommandWrapper.Command, want)
	}
	if cfg.TestCommandWrapper.RefusalPrefix != "cfo gate turn: " {
		t.Fatalf("TestCommandWrapper.RefusalPrefix = %q, want the prefix with its trailing space kept", cfg.TestCommandWrapper.RefusalPrefix)
	}

	merged := Merge(cfg, &RepoConfig{})
	if strings.Join(merged.TestCommandWrapper.Command, "\x00") != strings.Join(want, "\x00") || merged.TestCommandWrapper.RefusalPrefix != "cfo gate turn: " {
		t.Fatalf("merged TestCommandWrapper = %#v, want the machine's wrapper", merged.TestCommandWrapper)
	}
	// One run's config must not be able to rewrite the wrapper another reads.
	merged.TestCommandWrapper.Command[0] = "changed"
	if cfg.TestCommandWrapper.Command[0] != "cfo" {
		t.Fatal("the global config's wrapper was mutated through a merged copy")
	}
}

// Absent means off: the baseline test command starts exactly as it always has.
func TestLoadGlobal_TestCommandWrapperDefaultsOff(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.TestCommandWrapper.Command) != 0 || cfg.TestCommandWrapper.RefusalPrefix != "" {
		t.Fatalf("TestCommandWrapper = %#v, want none when unset", cfg.TestCommandWrapper)
	}
	if got := DefaultGlobalConfig().TestCommandWrapper; len(got.Command) != 0 || got.RefusalPrefix != "" {
		t.Fatalf("the default global config sets a wrapper: %#v", got)
	}
	if merged := Merge(cfg, &RepoConfig{}); len(merged.TestCommandWrapper.Command) != 0 {
		t.Fatalf("merged TestCommandWrapper = %#v, want none when unset", merged.TestCommandWrapper)
	}
}

// A wrapper that cannot be started, or a refusal rule with no wrapper to
// apply it to, is a mistake the config must name rather than run with.
func TestLoadGlobal_TestCommandWrapperRefusesUnusableValues(t *testing.T) {
	cases := map[string]string{
		"no command":          "test_command_wrapper:\n  refusal_prefix: \"cfo gate turn: \"\n",
		"empty command":       "test_command_wrapper:\n  command: []\n",
		"empty program":       "test_command_wrapper:\n  command: [\"\", \"--\"]\n",
		"blank program":       "test_command_wrapper:\n  command: [\"  \", \"--\"]\n",
		"empty word":          "test_command_wrapper:\n  command: [\"cfo\", \"\", \"--\"]\n",
		"multiline refusal":   "test_command_wrapper:\n  command: [\"cfo\"]\n  refusal_prefix: \"a\\nb\"\n",
		"blank refusal":       "test_command_wrapper:\n  command: [\"cfo\"]\n  refusal_prefix: \"   \"\n",
		"unknown placeholder": "test_command_wrapper:\n  command: [\"cfo\", \"--as\", \"{repository}\"]\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "test_command_wrapper") {
				t.Fatalf("LoadGlobalFromBytes() error = %v, want test_command_wrapper refused by name", err)
			}
		})
	}

	// The strict decoder refuses these in its own words.
	shapes := map[string]string{
		"unknown key":             "test_command_wrapper:\n  command: [\"cfo\"]\n  timeout: 5m\n",
		"a string, not a mapping": "test_command_wrapper: \"cfo gate turn --\"\n",
	}
	for name, data := range shapes {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(data)); err == nil {
				t.Fatal("LoadGlobalFromBytes() accepted a wrapper of the wrong shape")
			}
		})
	}
}

// The wrapper decides how a command starts on this machine, so no repository
// file can name one: neither a pushed branch nor the trusted copy.
func TestLoadRepo_TestCommandWrapperIsNotARepoSetting(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("test_command_wrapper:\n  command: [\"evil\", \"--\"]\ncommands:\n  test: go test ./...\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	merged := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(repo, repo, true))
	if len(merged.TestCommandWrapper.Command) != 0 {
		t.Fatalf("a repository file set the machine's test command wrapper: %#v", merged.TestCommandWrapper)
	}
}

func TestCommandWrapper_Words(t *testing.T) {
	wrapper := CommandWrapper{Command: []string{"cfo", "gate", "turn", "--as", "{repo} {branch}, run {run_short} ({run})", "--"}}
	got := wrapper.Words(CommandWrapperValues{Repo: "PrecisionDocs", Branch: "feat/x", Run: "01M4FG7FABCDEFGHJKMNPQRSTV"})
	want := []string{"cfo", "gate", "turn", "--as", "PrecisionDocs feat/x, run 01M4FG7F (01M4FG7FABCDEFGHJKMNPQRSTV)", "--"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Words() = %q, want %q", got, want)
	}
	if wrapper.Command[4] != "{repo} {branch}, run {run_short} ({run})" {
		t.Fatal("Words() rewrote the configured wrapper")
	}

	// A value is data, never a template: braces inside a branch name are not
	// expanded a second time.
	got = wrapper.Words(CommandWrapperValues{Repo: "{run}", Branch: "{repo}", Run: "short"})
	if got[4] != "{run} {repo}, run short (short)" {
		t.Fatalf("Words() = %q, want values substituted once", got[4])
	}

	if words := (CommandWrapper{}).Words(CommandWrapperValues{Repo: "r"}); words != nil {
		t.Fatalf("Words() of no wrapper = %q, want nil", words)
	}
}
