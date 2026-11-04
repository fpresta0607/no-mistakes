package config

import (
	"fmt"
	"regexp"
	"strings"
)

// CommandWrapper puts a command behind a machine-local wrapper program: the
// wrapper's words are started with no shell in front of them, followed by
// exactly the program and arguments the command would have been started with
// (`sh -c <command>`, or `cmd.exe /c <command>` on Windows). It is the shape
// the nice override already uses, so the command string is never joined to the
// wrapper and `a && b` stays one argument behind it.
//
// The wrapper is trusted to start what follows its words and to pass that
// program's output and exit code through.
type CommandWrapper struct {
	// Command is the wrapper program and its own arguments. A word may name a
	// run fact as {repo}, {branch}, {run}, {run_short} or {test_comparison_sha}.
	Command []string `yaml:"command" json:"command,omitempty"`
	// RefusalPrefix marks the wrapper's own last output line when it exits 125
	// or 126 without having produced a result of the command. Empty means the
	// exit code is always read as the command's own.
	RefusalPrefix string `yaml:"refusal_prefix" json:"refusal_prefix,omitempty"`
}

// CommandWrapperValues are the run facts a wrapper word may name.
type CommandWrapperValues struct {
	Repo              string
	Branch            string
	Run               string
	TestComparisonSHA string
}

// commandWrapperShortRun is how many characters of a run id {run_short}
// carries: what the status views print.
const commandWrapperShortRun = 8

var (
	commandWrapperPlaceholder   = regexp.MustCompile(`\{[a-z_]+\}`)
	commandWrapperPlaceholders  = []string{"{repo}", "{branch}", "{run}", "{run_short}", "{test_comparison_sha}"}
	commandWrapperComparisonSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// Enabled reports whether a wrapper is configured.
func (w CommandWrapper) Enabled() bool { return len(w.Command) > 0 }

// Words returns the wrapper's words for one run, or nil when none is
// configured. Each value is substituted once and is never read as a template.
// A requested Test comparison must be a full nonzero SHA before expansion.
func (w CommandWrapper) Words(values CommandWrapperValues) ([]string, error) {
	if !w.Enabled() {
		return nil, nil
	}
	for _, word := range w.Command {
		if strings.Contains(word, "{test_comparison_sha}") {
			if err := ValidateTestComparisonSHA(values.TestComparisonSHA); err != nil {
				return nil, fmt.Errorf("test_command_wrapper {test_comparison_sha}: %w", err)
			}
		}
	}
	short := values.Run
	if len(short) > commandWrapperShortRun {
		short = short[:commandWrapperShortRun]
	}
	replacer := strings.NewReplacer("{repo}", values.Repo, "{branch}", values.Branch, "{run_short}", short, "{run}", values.Run, "{test_comparison_sha}", values.TestComparisonSHA)
	words := make([]string, len(w.Command))
	for i, word := range w.Command {
		words[i] = replacer.Replace(word)
	}
	return words, nil
}

// ValidateTestComparisonSHA checks that the resolved comparison is a full
// object ID, never an absent previous-push ref or an unresolved revision.
func ValidateTestComparisonSHA(sha string) error {
	if !commandWrapperComparisonSHA.MatchString(sha) || strings.Trim(sha, "0") == "" {
		return fmt.Errorf("the resolved Test comparison must be a full nonzero SHA")
	}
	return nil
}

func copyCommandWrapper(w CommandWrapper) CommandWrapper {
	return CommandWrapper{Command: append([]string(nil), w.Command...), RefusalPrefix: w.RefusalPrefix}
}

func validateCommandWrapper(key string, w CommandWrapper) error {
	if len(w.Command) == 0 {
		return fmt.Errorf("%s.command must name the wrapper program", key)
	}
	for i, word := range w.Command {
		if strings.TrimSpace(word) == "" || strings.ContainsRune(word, '\x00') {
			return fmt.Errorf("%s.command[%d] must be a nonempty word without NUL", key, i)
		}
		for _, placeholder := range commandWrapperPlaceholder.FindAllString(word, -1) {
			known := false
			for _, name := range commandWrapperPlaceholders {
				known = known || placeholder == name
			}
			if !known {
				return fmt.Errorf("%s.command[%d] names %s, which is not one of %s", key, i, placeholder, strings.Join(commandWrapperPlaceholders, ", "))
			}
		}
	}
	if w.RefusalPrefix != "" && (strings.TrimSpace(w.RefusalPrefix) == "" || strings.ContainsAny(w.RefusalPrefix, "\r\n\x00")) {
		return fmt.Errorf("%s.refusal_prefix must be the start of one output line", key)
	}
	return nil
}
