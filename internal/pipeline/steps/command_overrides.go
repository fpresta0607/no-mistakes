package steps

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func runRepositoryCommand(sctx *pipeline.StepContext, name, command string) (string, int, error) {
	declareCommandOverrides(sctx, name)
	return executeRepositoryCommand(sctx, name, command)
}

func declareCommandOverrides(sctx *pipeline.StepContext, name string) {
	if declaration := commandOverrideDeclaration(name, sctx.Config.CommandOverrides[name]); declaration != "" {
		sctx.Log(declaration)
	}
	if words := commandWrapperWords(sctx, name); len(words) > 0 {
		quoted := make([]string, len(words))
		for i, word := range words {
			quoted[i] = fmt.Sprintf("%q", word)
		}
		sctx.Log(fmt.Sprintf("machine-local wrapper applied to commands.%s: %s", name, strings.Join(quoted, " ")))
	}
}

// declareStepCommandOverrides declares a Test or Lint step's overrides once per
// step: a fix round re-executes the step into the same step log.
func declareStepCommandOverrides(sctx *pipeline.StepContext, name string) {
	if !sctx.Fixing {
		declareCommandOverrides(sctx, name)
	}
}

func executeRepositoryCommand(sctx *pipeline.StepContext, name, command string) (string, int, error) {
	return runShellCommandBehind(sctx.Ctx, sctx.WorkDir, stepEnvironment(sctx), command, sctx.Config.CommandOverrides[name].Nice, commandWrapperWords(sctx, name))
}

// commandWrapperWords returns the words of the machine-local wrapper that
// name's command starts behind, or nil. Only the Test baseline has one
// (global test_command_wrapper): prepare, lint, format and repository gates
// start as they always have.
func commandWrapperWords(sctx *pipeline.StepContext, name string) []string {
	if name != "test" || !sctx.Config.TestCommandWrapper.Enabled() {
		return nil
	}
	words := sctx.Config.TestCommandWrapper.Words(config.CommandWrapperValues{
		Repo:   wrapperRepoName(sctx),
		Branch: strings.TrimPrefix(sctx.Run.Branch, "refs/heads/"),
		Run:    sctx.Run.ID,
	})
	// Like stepCmd, resolve a bare program name on the step's own PATH.
	if len(sctx.Env) > 0 && !hasExecutablePathSeparator(words[0]) {
		if candidate := findInCustomPath(sctx.WorkDir, sctx.Env, words[0]); candidate != "" {
			words[0] = candidate
		}
	}
	return words
}

// wrapperRepoName is the short name a wrapper shows for the run's repository:
// the last path element of its upstream URL, else of its checkout.
func wrapperRepoName(sctx *pipeline.StepContext) string {
	if sctx.Repo == nil {
		return ""
	}
	upstream := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(sctx.Repo.UpstreamURL), "/"), ".git")
	if cut := strings.LastIndexAny(upstream, "/:"); cut >= 0 {
		upstream = upstream[cut+1:]
	}
	if name := path.Base(upstream); upstream != "" && name != "." && name != "/" {
		return name
	}
	return filepath.Base(sctx.Repo.WorkingPath)
}

// commandWrapperRefusal returns the wrapper's own last line when a wrapped
// command's exit says the wrapper never produced a result of the command, or
// "". The wrapper convention is the one env, nice and timeout share: 125 when
// the wrapper itself gave up, 126 when the command could not be started. A
// command can exit with those codes too, so the reading needs the configured
// refusal_prefix on the last output line; without one configured every exit
// code is the command's own.
func commandWrapperRefusal(sctx *pipeline.StepContext, name, output string, exitCode int) string {
	prefix := sctx.Config.TestCommandWrapper.RefusalPrefix
	if (exitCode != 125 && exitCode != 126) || prefix == "" || len(commandWrapperWords(sctx, name)) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(output, "\r\n\t "), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(last, strings.TrimLeft(prefix, " ")) {
		return ""
	}
	return last
}

// commandOverrideDeclaration states every machine-local override applied to a
// command so a result produced under one is never presented as a plain run.
func commandOverrideDeclaration(name string, override config.CommandOverride) string {
	var parts []string
	if override.Nice != 0 {
		parts = append(parts, fmt.Sprintf("nice %d", override.Nice))
	}
	if len(override.Additional) != 0 {
		checks := make([]string, len(override.Additional))
		for i, command := range override.Additional {
			checks[i] = fmt.Sprintf("%q", command)
		}
		parts = append(parts, "additional checks "+strings.Join(checks, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("machine-local overrides applied to commands.%s: %s", name, strings.Join(parts, "; "))
}

// checkResult is one configured check's own outcome, so a machine-local
// check's failure is never attributed to the repository's command.
type checkResult struct {
	Command  string
	Local    bool
	ExitCode int
	Output   string
	// NoTurn is why the machine-local wrapper produced no result of this
	// check: its own refusal line, or that it could not be started. A check
	// with a NoTurn never ran, so its ExitCode is not a test result.
	NoTurn string
}

func (r checkResult) description(name string) string {
	if r.Local {
		return fmt.Sprintf("machine-local %s check failed with exit code %d: %s", name, r.ExitCode, r.Command)
	}
	return fmt.Sprintf("configured %s command failed with exit code %d", name, r.ExitCode)
}

func failedChecks(results []checkResult) []checkResult {
	var failed []checkResult
	for _, result := range results {
		if result.ExitCode != 0 {
			failed = append(failed, result)
		}
	}
	return failed
}

// runConfiguredChecks runs the repository command (when nonempty) followed by
// the operator's machine-local additional checks for name.
func runConfiguredChecks(sctx *pipeline.StepContext, name, command string) (string, []checkResult, error) {
	override := sctx.Config.CommandOverrides[name]
	checks := []checkResult{}
	if command != "" {
		checks = append(checks, checkResult{Command: command})
	}
	for _, additional := range override.Additional {
		checks = append(checks, checkResult{Command: additional, Local: true})
	}
	var output strings.Builder
	for i := range checks {
		if err := sctx.Ctx.Err(); err != nil {
			return output.String(), checks[:i], err
		}
		if checks[i].Local {
			sctx.Log(fmt.Sprintf("running machine-local %s check: %s", name, checks[i].Command))
			fmt.Fprintf(&output, "\nmachine-local %s check: %s\n", name, checks[i].Command)
		} else if len(override.Additional) > 0 {
			fmt.Fprintf(&output, "\nconfigured %s command: %s\n", name, checks[i].Command)
		}
		out, code, err := executeRepositoryCommand(sctx, name, checks[i].Command)
		output.WriteString(out)
		if err != nil && sctx.Ctx.Err() == nil && len(commandWrapperWords(sctx, name)) > 0 {
			// A wrapper that cannot be started gave this check no turn. That is
			// the machine's state, not the run's, so it parks like a refusal
			// instead of failing the run.
			checks[i].ExitCode = 126
			checks[i].NoTurn = fmt.Sprintf("the wrapper could not be started: %v", err)
			return output.String(), checks[:i+1], nil
		}
		if err != nil {
			return output.String(), checks[:i], err
		}
		checks[i].ExitCode = code
		checks[i].Output = out
		if refusal := commandWrapperRefusal(sctx, name, out, code); refusal != "" {
			// Nothing after a check that was given no turn would get one.
			checks[i].NoTurn = refusal
			return output.String(), checks[:i+1], nil
		}
		if len(override.Additional) > 0 {
			fmt.Fprintf(&output, "\nexit code: %d\n", code)
		}
	}
	return output.String(), checks, nil
}
