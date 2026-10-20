//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

// TestMain lets this test binary serve as the private desktop helper an
// agent placed on a hidden desktop starts, as cmd/no-mistakes does.
func TestMain(m *testing.M) {
	if handled, exitCode, err := shellenv.RunWindowsDesktopCommandHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode)
	}
	os.Exit(m.Run())
}

func TestClaudeAgentRunsOnTheInvocationsHiddenDesktop(t *testing.T) {
	desktop, err := winproc.NewHiddenDesktop()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = desktop.Close() })
	report := filepath.Join(t.TempDir(), "desktop")
	t.Setenv("NM_AGENT_DESKTOP_REPORT", report)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := &claudeAgent{bin: exe, extraArgs: []string{"-test.run=^TestAgentDesktopHelper$", "--"}, disableProjectSettings: true}

	_, err = a.Run(context.Background(), RunOpts{Prompt: "check", CWD: t.TempDir(), Desktop: desktop.Name(), JSONSchema: json.RawMessage(`{"type":"object","required":["ok"]}`)})
	if err != nil {
		t.Fatalf("Run on a hidden desktop: %v", err)
	}

	got, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the agent left no report of its desktop: %v", err)
	}
	if string(got) != desktop.Name() {
		t.Errorf("the agent ran on desktop %q, want the invocation's hidden desktop %q", got, desktop.Name())
	}
}

// TestAgentDesktopHelper stands in for the claude binary: it records the
// desktop it runs on and answers as claude does.
func TestAgentDesktopHelper(t *testing.T) {
	report := os.Getenv("NM_AGENT_DESKTOP_REPORT")
	if report == "" || len(argsAfterDoubleDash(os.Args)) == 0 {
		return
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	desktop, _, _ := user32.NewProc("GetThreadDesktop").Call(uintptr(windows.GetCurrentThreadId()))
	name := make([]uint16, 256)
	var needed uint32
	const userObjectName = 2
	user32.NewProc("GetUserObjectInformationW").Call(desktop, userObjectName, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)*2), uintptr(unsafe.Pointer(&needed)))
	if err := os.WriteFile(report, []byte(windows.UTF16ToString(name)), 0o600); err != nil {
		t.Fatal(err)
	}
	emitClaudeHelperResult()
}
