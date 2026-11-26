package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Leave room for testing.T's named directories and Git's incoming object paths.
const testWindowsScratchMaxChars = 60

func newTestScratch(parent, goos string) (string, error) {
	if parent == "" && goos == "windows" {
		parent = os.TempDir()
		// A nested Test process inherits its caller's scratch as TEMP. Use the
		// per-user Windows temp directory when nesting exhausts room; a sandboxed
		// home's cache directory may not have one yet.
		if len(utf16.Encode([]rune(parent)))+14 > testWindowsScratchMaxChars {
			cache, err := os.UserCacheDir()
			if err != nil {
				return "", fmt.Errorf("resolve short Windows temporary root: %w", err)
			}
			parent = filepath.Join(cache, "Temp")
			if err := os.MkdirAll(parent, 0o700); err != nil {
				return "", fmt.Errorf("create short Windows temporary root: %w", err)
			}
		}
	}
	dir, err := os.MkdirTemp(parent, "nm-")
	if err != nil {
		return "", fmt.Errorf("create owned agent scratch: %w", err)
	}
	if goos == "windows" && len(utf16.Encode([]rune(dir))) > testWindowsScratchMaxChars {
		return "", errors.Join(fmt.Errorf("an agent turn needs a short temporary root: scratch %q exceeds %d Windows characters; provide a shorter TEMP directory", dir, testWindowsScratchMaxChars), os.RemoveAll(dir))
	}
	return dir, nil
}

func prepareTestHost(sctx *pipeline.StepContext, scratch string) string {
	settings := map[string]string{"TMP": scratch, "TEMP": scratch, "TMPDIR": scratch, "GOTMPDIR": scratch}
	guidance := testScratchGuidance(scratch) + "\n- TMP, TEMP, TMPDIR and GOTMPDIR already name that owned scratch directory. Keep disposable Git repositories and build trees there; keep evidence artifacts in the recorded evidence directory."
	if dirs := sctx.Run.TestOutputDirs; len(dirs) > 0 {
		guidance += fmt.Sprintf("\n- The operator authorized these directories as output destinations for the commands this step runs (logs, PID files, temporary build files, provenance): %s. Writes there are allowed; every other destination outside the worktree, the evidence directory and the scratch directory stays refused.", strings.Join(dirs, ", "))
	}
	if runtime.GOOS == "windows" {
		env := runenv.Overlay{}.Apply(mergeEnv(sctx.Env))
		bash, err := resolveNativeTestBash(nativeTestBashCandidates(sctx.WorkDir, env), os.Getenv("SystemRoot"), func(path string) error {
			ctx, cancel := context.WithTimeout(sctx.Ctx, 5*time.Second)
			defer cancel()
			cmd := stepCmdContext(sctx, ctx, path, "--noprofile", "--norc", "-c", `test -n "$BASH_VERSION" && test "$OSTYPE" = msys && printf native-git-bash`)
			cmd.Env = (runenv.Overlay{Unset: []string{"BASH_ENV", "ENV"}}).Apply(env)
			shellenv.ConfigureShellCommand(cmd)
			output, err := shellenv.CombinedOutputShellCommand(cmd)
			if err != nil {
				return err
			}
			if string(output) != "native-git-bash" {
				return fmt.Errorf("shell did not identify as native MSYS Bash")
			}
			return nil
		})
		if err != nil {
			sctx.Log("Test host prerequisite: " + err.Error())
			guidance += "\n- Native Git Bash is unavailable or could not be verified. Do not launch bash, sh, WSL or a Linux distribution. Report scenarios requiring Bash as untested with that missing prerequisite; continue checks that use available native tools."
		} else {
			settings["NO_MISTAKES_TEST_BASH"] = bash
			sctx.Log("Test host verified native Git Bash: " + bash)
			guidance += fmt.Sprintf("\n- The verified native Git Bash executable is %q (also NO_MISTAKES_TEST_BASH). Invoke that exact executable with --noprofile --norc -o igncr when running an existing Bash script, so CRLF checkout lines work without editing source. Use the prepared native Windows tool environment; do not use WSL, replace TMPDIR with /tmp, or search the host for another shell.", filepath.ToSlash(bash))
		}
	}
	sctx.Env = (runenv.Overlay{Set: settings, Unset: []string{"NO_MISTAKES_TEST_BASH"}}).Apply(sctx.Env)
	sctx.Log(fmt.Sprintf("Test host scratch: %s (owned by this Test execution; evidence: %s; source: %s)", scratch, testEvidenceDir(sctx), sctx.Run.HeadSHA))
	return guidance + testLoadRunGuidance + testCommandWrapperGuidance(sctx)
}

// testLoadRunGuidance keeps load runs the operator's decision. A Test agent
// once saturated every core of a shared machine with busy loops beside other
// runs' tests on its own initiative and turned the result into a no-go finding
// (issue #270).
const testLoadRunGuidance = "\n- Start no load, stress or saturation run: no busy loops or extra processes to occupy processors or memory, no reduced or pinned processor count set against competing load, and no repeated or concurrent batches of runs meant to put pressure on the machine. Other work shares this machine, and such a run disturbs it. When you judge one necessary, do not start it: report it for the operator's decision, naming the run and why it is needed. In a live check, report the scenario that needs it as untested and add one warning finding with action \"ask-user\"; in a repair, add a \"needs_decision\" entry."

// testCommandWrapperGuidance gives a Test agent the machine-local test wrapper's
// words for the heavy work it starts by itself. A build an agent chose on its
// own once held 3 GB beside a test line that counted only configured test
// commands, while other runs' tests waited there for memory (issue #252).
// The words also carry whatever environment the wrapper binds for what it
// starts, which the step never sees. An agent once probed `uv run --no-sync
// python -c "import pytest"` outside them, resolved a host interpreter instead
// of the project environment the wrapper had bound for the baseline, and
// reported the suite's tools missing (issue #390).
func testCommandWrapperGuidance(sctx *pipeline.StepContext) string {
	words, err := commandWrapperWords(sctx, "test")
	if err != nil {
		sctx.Log("Test agent gets no test wrapper: " + err.Error())
		return "\n- This machine runs builds and tests in a test line, and its wrapper could not be prepared for this run (" + err.Error() + "). Start no build, compile or test run; report every scenario that needs one as untested with that reason."
	}
	if len(words) == 0 {
		return ""
	}
	var list bytes.Buffer
	encoder := json.NewEncoder(&list)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(words); err != nil {
		panic(err) // a []string always encodes
	}
	sctx.Log("machine-local wrapper given to the Test agent for the builds, test runs and project interpreter commands it starts")
	guidance := fmt.Sprintf("\n- This machine runs builds and tests in a test line. Start every command that installs dependencies, builds, compiles or runs tests (a whole suite or one named test), and every command that runs or inspects the project's interpreter, package manager or test tools (such as `uv run`, an import check or a tool's version), behind these words, as one argument each and in this order, followed by the command's program and its own arguments: %s. The words may wait for this machine's turn before they start the command; wait inside that tool call. They may also start the command in the environment the configured test command ran in, such as the project's bound interpreter and tools, so a command started without them can resolve a different one. Never start such a command without them, and never report a tool or dependency missing from a command started without them.", bytes.TrimSpace(list.Bytes()))
	if prefix := sctx.Config.TestCommandWrapper.RefusalPrefix; prefix != "" {
		guidance += fmt.Sprintf(" An exit code of 125 or 126 whose last output line starts with %q means the machine gave the command no turn and it never ran: report every scenario that needed it as untested with that line, and never run it another way.", prefix)
	}
	return guidance
}

func nativeTestBashCandidates(workDir string, env []string) []string {
	var candidates []string
	if git := findInCustomPath(workDir, env, "git"); git != "" && strings.EqualFold(filepath.Ext(git), ".exe") {
		root := filepath.Dir(filepath.Dir(git))
		if strings.EqualFold(filepath.Base(root), "mingw64") || strings.EqualFold(filepath.Base(root), "mingw32") {
			root = filepath.Dir(root)
		}
		candidates = append(candidates, filepath.Join(root, "bin", "bash.exe"))
	}
	for _, key := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if root, ok := envValue(env, key); ok && filepath.IsAbs(root) {
			candidates = append(candidates, filepath.Join(root, "Git", "bin", "bash.exe"))
		}
	}
	return candidates
}

func resolveNativeTestBash(candidates []string, systemRoot string, verify func(string) error) (string, error) {
	var failures []error
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		path, err := realpath.Resolve(candidate)
		if err != nil || !filepath.IsAbs(path) || seen[strings.ToLower(path)] {
			continue
		}
		seen[strings.ToLower(path)] = true
		if systemRoot != "" {
			relative, err := filepath.Rel(systemRoot, path)
			if err == nil && (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
				continue
			}
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := verify(path); err != nil {
			failures = append(failures, fmt.Errorf("verify %s: %w", path, err))
			continue
		}
		return path, nil
	}
	return "", errors.Join(append([]error{errors.New("native Git Bash prerequisite unavailable; WSL is never a fallback")}, failures...)...)
}
