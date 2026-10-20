//go:build windows

package shellenv

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

// desktopProbeTitle names the window the probe opens, only ever on a hidden
// desktop.
const desktopProbeTitle = "no-mistakes desktop probe"

var (
	probeUser32                   = windows.NewLazySystemDLL("user32.dll")
	procGetThreadDesktop          = probeUser32.NewProc("GetThreadDesktop")
	procGetUserObjectInformationW = probeUser32.NewProc("GetUserObjectInformationW")
	procCreateWindowExW           = probeUser32.NewProc("CreateWindowExW")
	procDestroyWindow             = probeUser32.NewProc("DestroyWindow")
)

func TestPlaceOnDesktopRunsAProgramAndEveryProgramItStartsOnTheHiddenDesktop(t *testing.T) {
	installWindowsDesktopCommandTestHelper(t)
	desktop, err := winproc.NewHiddenDesktop()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = desktop.Close() })
	reports := t.TempDir()
	cmd := desktopProbeCommand(reports)
	ConfigureShellCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := PlaceOnDesktop(cmd, desktop.Name()); err != nil {
		t.Fatal(err)
	}
	if err := RunShellCommand(cmd); err != nil {
		t.Fatalf("the probe failed: %v\n%s", err, output.String())
	}
	if err := desktop.Close(); err != nil {
		t.Fatal(err)
	}

	for _, who := range []string{"program", "child"} {
		if got := readDesktopProbeReport(t, reports, who); got != desktop.Name() {
			t.Errorf("the %s ran on desktop %q, want the hidden desktop %q", who, got, desktop.Name())
		}
	}
	opened, more := desktop.Windows()
	image := filepath.Base(os.Args[0])
	found := false
	for _, window := range opened {
		if window.Title == desktopProbeTitle && strings.EqualFold(window.Image, image) && window.PID > 0 {
			found = true
		}
	}
	if !found || more != 0 {
		t.Errorf("the hidden desktop recorded %v and %d more, want the child's window %q of %s", opened, more, desktopProbeTitle, image)
	}
}

func TestPlaceOnDesktopRefusesToStartWhenTheDesktopIsNotOpen(t *testing.T) {
	installWindowsDesktopCommandTestHelper(t)
	reports := t.TempDir()
	cmd := desktopProbeCommand(reports)
	ConfigureShellCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := PlaceOnDesktop(cmd, "no-mistakes-never-created"); err != nil {
		t.Fatal(err)
	}
	err := RunShellCommand(cmd)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("RunShellCommand() error = %v, want exit code 1\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "refusing to start") || !strings.Contains(output.String(), "must not start on the user's desktop") {
		t.Errorf("the helper did not say it refused the start:\n%s", output.String())
	}
	if _, err := os.Stat(filepath.Join(reports, "program")); !os.IsNotExist(err) {
		t.Errorf("the program ran although its desktop was not open: %v", err)
	}
}

func TestPlaceOnDesktopLeavesACommandAloneWithoutADesktop(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	path, args := cmd.Path, append([]string(nil), cmd.Args...)

	if err := PlaceOnDesktop(cmd, ""); err != nil {
		t.Fatal(err)
	}

	if cmd.Path != path || strings.Join(cmd.Args, " ") != strings.Join(args, " ") {
		t.Errorf("PlaceOnDesktop with no desktop changed the command to %s %v", cmd.Path, cmd.Args)
	}
}

// TestWindowsDesktopProbe is the program a placed command starts. It records
// the desktop it runs on, then starts a child that does the same and opens a
// window there. The child opens its window only on a hidden desktop this
// package created, so a broken placement can never show it on the user's
// screen.
func TestWindowsDesktopProbe(t *testing.T) {
	reports := os.Getenv("NM_DESKTOP_PROBE_REPORTS")
	if reports == "" {
		return
	}
	desktop := threadDesktopName()
	switch os.Getenv("NM_DESKTOP_PROBE") {
	case "program":
		writeDesktopProbeReport(t, reports, "program", desktop)
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsDesktopProbe$")
		child.Env = append(os.Environ(), "NM_DESKTOP_PROBE=child")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("the child failed: %v\n%s", err, output)
		}
	case "child":
		writeDesktopProbeReport(t, reports, "child", desktop)
		if !strings.HasPrefix(desktop, "no-mistakes-") {
			t.Fatalf("refusing to open a window on desktop %q, which is not a hidden one", desktop)
		}
		openDesktopProbeWindow(t)
	}
}

func TestWindowsDesktopCommandLauncherHelper(t *testing.T) {
	args := argsAfterSeparator(os.Args)
	if len(args) == 0 {
		return
	}
	handled, exitCode, err := RunWindowsDesktopCommandHelper(args)
	if !handled {
		t.Fatal("Windows desktop command helper arguments were not recognized")
	}
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
	}
	os.Exit(exitCode)
}

func installWindowsDesktopCommandTestHelper(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := windowsDesktopHelperCommandFunc
	windowsDesktopHelperCommandFunc = func(desktop, targetPath string, targetArgs []string) (string, []string, error) {
		args := []string{exe, "-test.run=^TestWindowsDesktopCommandLauncherHelper$", "--", windowsDesktopCommandArg + desktop, targetPath}
		return exe, append(args, targetArgs...), nil
	}
	t.Cleanup(func() { windowsDesktopHelperCommandFunc = old })
}

func desktopProbeCommand(reports string) *exec.Cmd {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestWindowsDesktopProbe$")
	cmd.Env = append(os.Environ(), "NM_DESKTOP_PROBE=program", "NM_DESKTOP_PROBE_REPORTS="+reports)
	return cmd
}

func argsAfterSeparator(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// threadDesktopName is the name of the desktop this goroutine's thread runs
// on, which is the process's own until it calls SetThreadDesktop.
func threadDesktopName() string {
	desktop, _, _ := procGetThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	name := make([]uint16, 256)
	var needed uint32
	const userObjectName = 2
	procGetUserObjectInformationW.Call(desktop, userObjectName, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)*2), uintptr(unsafe.Pointer(&needed)))
	return windows.UTF16ToString(name)
}

func openDesktopProbeWindow(t *testing.T) {
	const visibleOverlappedWindow = 0x10CF0000
	className, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString(desktopProbeTitle)
	window, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)), visibleOverlappedWindow, 0, 0, 320, 200, 0, 0, 0, 0)
	if window == 0 {
		t.Fatalf("CreateWindowEx: %v", err)
	}
	// Long enough for the desktop's watch to see it more than once.
	time.Sleep(time.Second)
	procDestroyWindow.Call(window)
}

func writeDesktopProbeReport(t *testing.T, reports, who, desktop string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(reports, who), []byte(desktop), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readDesktopProbeReport(t *testing.T, reports, who string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(reports, who))
	if err != nil {
		t.Fatalf("the %s left no report: %v", who, err)
	}
	return string(data)
}
