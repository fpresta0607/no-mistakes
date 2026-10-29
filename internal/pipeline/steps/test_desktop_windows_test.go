//go:build windows

package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"golang.org/x/sys/windows"
)

const testStepWindowTitle = "no-mistakes test step window"

func TestTestStep_ReportNamesAWindowItsAgentOpenedAfterItClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := openWindowOnDesktop(opts.Desktop, time.Second); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(passingTestEvidence)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logged []string
	sctx.Log = func(line string) { logged = append(logged, line) }

	outcome, err := (&TestStep{}).Execute(sctx)

	if err != nil {
		t.Fatalf("test step failed: %v", err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	// The window is this test process's own, so it is named by its program
	// and process: GetWindowText cannot read the title of a window of its
	// own process on another desktop. A separate program's title is read in
	// shellenv's TestPlaceOnDesktopRunsAProgramAndEveryProgramItStartsOnTheHiddenDesktop.
	want := fmt.Sprintf("%s (pid %d)", filepath.Base(os.Args[0]), os.Getpid())
	var named []types.Finding
	for _, item := range findings.Items {
		if strings.Contains(item.Description, want) {
			named = append(named, item)
		}
	}
	if len(named) != 1 || named[0].Severity != types.FindingSeverityInfo || named[0].Action != types.ActionNoOp || outcome.NeedsApproval {
		t.Fatalf("the report names the window as %+v, needs approval %v; want one informational finding\nfindings: %s", named, outcome.NeedsApproval, outcome.Findings)
	}
	if !strings.Contains(strings.Join(logged, "\n"), want) {
		t.Errorf("the step log does not name the window:\n%s", strings.Join(logged, "\n"))
	}
}

// openWindowOnDesktop opens a visible window on the hidden desktop named
// desktop, as a program an agent started there would, keeps it open for
// open, and closes it. The window's thread is the only one ever moved to the
// desktop, and it ends with the window.
func openWindowOnDesktop(desktop string, open time.Duration) error {
	if !strings.HasPrefix(desktop, "no-mistakes-") {
		return fmt.Errorf("refusing to open a window on desktop %q, which is not a hidden one", desktop)
	}
	done := make(chan error, 1)
	go func() {
		// Never unlocked, so the thread ends with this goroutine.
		runtime.LockOSThread()
		done <- openWindowOnThisThread(desktop, open)
	}()
	return <-done
}

func openWindowOnThisThread(desktop string, open time.Duration) error {
	const desktopAllAccess = 0x01ff
	const visibleOverlappedWindow = 0x10CF0000
	user32 := windows.NewLazySystemDLL("user32.dll")
	name, err := windows.UTF16PtrFromString(desktop)
	if err != nil {
		return err
	}
	handle, _, err := user32.NewProc("OpenDesktopW").Call(uintptr(unsafe.Pointer(name)), 0, 0, desktopAllAccess)
	if handle == 0 {
		return fmt.Errorf("OpenDesktop %s: %w", desktop, err)
	}
	defer user32.NewProc("CloseDesktop").Call(handle)
	if moved, _, err := user32.NewProc("SetThreadDesktop").Call(handle); moved == 0 {
		return fmt.Errorf("SetThreadDesktop %s: %w", desktop, err)
	}
	className, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString(testStepWindowTitle)
	window, _, err := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)), visibleOverlappedWindow, 0, 0, 320, 200, 0, 0, 0, 0)
	if window == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	time.Sleep(open)
	user32.NewProc("DestroyWindow").Call(window)
	return nil
}
