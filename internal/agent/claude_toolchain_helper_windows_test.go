//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

// Issue #259: a Test agent that built with MSVC exits with success while the
// telemetry helper link.exe started is still running. The helper is a
// grandchild whose parent already exited, as with a real link, and the turn
// runs on a hidden desktop as Test turns do. The same leftover outside an MSVC
// toolchain directory is still refused.
func TestClaudeAgent_SuccessWithOnlyAToolchainTelemetryHelperLeftCompletes(t *testing.T) {
	root := t.TempDir()
	// One copy of the test binary, hard-linked into each layout, so the
	// helper's image is scanned once.
	copied := filepath.Join(root, "helper.exe")
	copyTestExecutable(t, copied)
	for _, tc := range []struct {
		name       string
		helperDir  []string
		wantResult bool
	}{
		{name: "msvc telemetry helper is not unfinished work", helperDir: []string{"VC", "Tools", "MSVC", "14.44.35207", "bin", "Hostx64", "x64"}, wantResult: true},
		{name: "the same name outside a toolchain is still a child", helperDir: []string{"target", "debug"}, wantResult: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := filepath.Join(append([]string{root}, append(tc.helperDir, "vctip.exe")...)...)
			if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(copied, helper); err != nil {
				t.Fatal(err)
			}
			desktop, err := winproc.NewHiddenDesktop()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = desktop.Close() })
			ready := filepath.Join(dir, "helper-ready")
			t.Setenv("NM_CLAUDE_STDIN_HELPER", "result-after-toolchain-link")
			t.Setenv("NM_CLAUDE_STDIN_TOOLCHAIN_HELPER", helper)
			t.Setenv("NM_CLAUDE_STDIN_READY", ready)

			res, err := newClaudeStdinHelperAgent(t).runOnce(context.Background(), RunOpts{
				Prompt:                   "test",
				CWD:                      t.TempDir(),
				Desktop:                  desktop.Name(),
				JSONSchema:               json.RawMessage(`{"type":"object","required":["ok"]}`),
				RequireCompletedChildren: true,
			})

			if tc.wantResult {
				if err != nil {
					t.Fatalf("err = %v, want the finished result", err)
				}
				if string(res.Output) != `{"ok":true}` {
					t.Fatalf("output = %s, want the result", res.Output)
				}
				return
			}
			helperPID, readErr := os.ReadFile(ready)
			if readErr != nil {
				t.Fatalf("the telemetry helper never reported ready: %v", readErr)
			}
			if err == nil || !strings.Contains(err.Error(), "agent exited with active owned children") || !strings.Contains(err.Error(), "pid "+string(helperPID)+" ") {
				t.Fatalf("err = %v, want the refusal naming the helper, pid %s", err, helperPID)
			}
		})
	}
}

// TestClaudeToolchainLinkHelper stands in for link.exe and the telemetry
// helper it leaves behind. Like link.exe, the linker starts the helper
// detached, so it has no console, and exits once it runs.
func TestClaudeToolchainLinkHelper(t *testing.T) {
	switch os.Getenv("NM_CLAUDE_STDIN_HELPER") {
	case "toolchain-link":
		helper := exec.Command(os.Getenv("NM_CLAUDE_STDIN_TOOLCHAIN_HELPER"), "-test.run=^TestClaudeToolchainLinkHelper$")
		helper.Env = append(os.Environ(), "NM_CLAUDE_STDIN_HELPER=toolchain-helper")
		helper.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if err := helper.Start(); err != nil {
			os.Exit(2)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv("NM_CLAUDE_STDIN_READY")); err == nil {
				return
			}
			if time.Now().After(deadline) {
				os.Exit(3)
			}
			time.Sleep(25 * time.Millisecond)
		}
	case "toolchain-helper":
		_ = os.WriteFile(os.Getenv("NM_CLAUDE_STDIN_READY"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(30 * time.Second)
	}
}

func copyTestExecutable(t *testing.T, destination string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
}
