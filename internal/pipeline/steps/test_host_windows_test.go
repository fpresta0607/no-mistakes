//go:build windows

package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestTestHostNativeBashRunsCRLFWithNativeGoWhenWSLWinsPATH(t *testing.T) {
	t.Parallel()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	wslDirectory := t.TempDir()
	writeTestFile(t, filepath.Join(wslDirectory, "bash.exe"), "WSL launcher must never execute")
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		bash, _ := envValue(opts.Env, "NO_MISTAKES_TEST_BASH")
		if bash == "" {
			t.Skip("native Git Bash prerequisite unavailable on this host")
		}
		if !strings.Contains(opts.Prompt, filepath.ToSlash(bash)) || !strings.Contains(opts.Prompt, "-o igncr") {
			t.Fatal("agent did not receive the exact native shell and CRLF instructions")
		}
		if got := findInCustomPath(dir, opts.Env, "bash"); !strings.EqualFold(got, filepath.Join(wslDirectory, "bash.exe")) {
			t.Fatalf("fixture does not reproduce WSL-first PATH: %q", got)
		}
		scratch, _ := envValue(opts.Env, "TMPDIR")
		script := filepath.Join(scratch, "existing-journey.sh")
		writeTestFile(t, script, "set -eu\r\ntest \"$OSTYPE\" = msys\r\n\"$NATIVE_GO\" version\r\nprintf 'native-journey-passed\\n'\r\n")
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-o", "igncr", filepath.ToSlash(script))
		cmd.Dir = dir
		cmd.Env = (runenv.Overlay{Set: map[string]string{"NATIVE_GO": filepath.ToSlash(goBinary)}, Unset: []string{"BASH_ENV", "ENV"}}).Apply(mergeEnv(opts.Env))
		shellenv.ConfigureShellCommand(cmd)
		output, err := shellenv.CombinedOutputShellCommand(cmd)
		if err != nil || !strings.Contains(string(output), "native-journey-passed") || !strings.Contains(string(output), "windows/") {
			t.Fatalf("native Bash CRLF journey: %v\n%s", err, output)
		}
		t.Logf("source=%s bash=%s native_go=%s result=%s", head, bash, goBinary, output)
		return &agent.Result{Output: json.RawMessage(passingTestTurnOutput)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Env = []string{"PATH=" + wslDirectory + string(os.PathListSeparator) + os.Getenv("PATH")}
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
}

func TestTestHostNestedWindowsTempStillGetsAShortOwnedDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), strings.Repeat("nested-", 8))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMP", parent)
	t.Setenv("TMP", parent)
	scratch, err := newTestScratch("", "windows")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	if len(scratch) > testWindowsScratchMaxChars || strings.HasPrefix(scratch, parent) {
		t.Fatalf("nested temp was not prepared: %s", scratch)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("preparation changed the caller's temp: %v", err)
	}
}
