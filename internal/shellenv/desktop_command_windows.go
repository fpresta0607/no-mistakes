//go:build windows

package shellenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsDesktopCommandArg = "--internal-windows-desktop-command="

// desktopReadObjects is DESKTOP_READOBJECTS, the least access that proves a
// desktop exists in this window station.
const desktopReadObjects = 0x0001

var (
	desktopUser32    = windows.NewLazySystemDLL("user32.dll")
	procOpenDesktopW = desktopUser32.NewProc("OpenDesktopW")
	procCloseDesktop = desktopUser32.NewProc("CloseDesktop")
)

var windowsDesktopHelperCommandFunc = windowsDesktopHelperCommand

// PlaceOnDesktop makes cmd start on desktop, the name of a
// winproc.HiddenDesktop, so the program and every process it starts run
// there and none of their windows shows on the user's screen. Go cannot name
// a desktop for a process it starts, so cmd starts this binary's private
// helper instead, which starts the program on desktop with cmd's standard
// handles, environment and directory and exits with its code. The helper
// refuses to start the program when desktop does not exist, so a placed
// command never starts on the user's desktop instead. Call it after the
// configurator and before StartShellCommand; an empty desktop leaves cmd as
// it is.
func PlaceOnDesktop(cmd *exec.Cmd, desktop string) error {
	if desktop == "" {
		return nil
	}
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return errors.New("invalid command to place on a desktop")
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		return errors.New("a command placed on a desktop cannot set SysProcAttr.CmdLine")
	}
	path, args, err := windowsDesktopHelperCommandFunc(desktop, cmd.Path, cmd.Args)
	if err != nil {
		return fmt.Errorf("prepare Windows desktop command helper: %w", err)
	}
	cmd.Path = path
	cmd.Args = args
	return nil
}

func windowsDesktopHelperCommand(desktop, targetPath string, targetArgs []string) (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	args := []string{exe, windowsDesktopCommandArg + desktop, targetPath}
	args = append(args, targetArgs...)
	return exe, args, nil
}

// RunWindowsDesktopCommandHelper handles the private process PlaceOnDesktop
// starts: it starts the program named after the desktop on that desktop and
// returns the program's exit code.
func RunWindowsDesktopCommandHelper(args []string) (bool, int, error) {
	if len(args) == 0 || !strings.HasPrefix(args[0], windowsDesktopCommandArg) {
		return false, 0, nil
	}
	if len(args) < 3 {
		return true, 1, errors.New("missing Windows desktop command arguments")
	}
	desktop := strings.TrimPrefix(args[0], windowsDesktopCommandArg)
	if desktop == "" {
		return true, 1, errors.New("empty Windows desktop name")
	}
	exitCode, err := runOnWindowsDesktop(desktop, args[1], args[2:])
	return true, exitCode, err
}

// runOnWindowsDesktop starts targetPath on desktop as os/exec would start it,
// with the same application name and command line, so a batch file runs as it
// would there, and with this process's standard handles, environment and
// directory, then waits for it.
func runOnWindowsDesktop(desktop, targetPath string, targetArgs []string) (int, error) {
	desktopName, err := windows.UTF16PtrFromString(desktop)
	if err != nil {
		return 1, err
	}
	opened, _, openErr := procOpenDesktopW.Call(uintptr(unsafe.Pointer(desktopName)), 0, 0, desktopReadObjects)
	if opened == 0 {
		return 1, fmt.Errorf("refusing to start %s: desktop %s is not open in this window station, and the program must not start on the user's desktop instead: %w", targetPath, desktop, openErr)
	}
	defer procCloseDesktop.Call(opened)
	appName, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return 1, err
	}
	commandLine, err := windows.UTF16PtrFromString(windowsCommandLine(targetArgs))
	if err != nil {
		return 1, err
	}
	startup := windows.StartupInfo{
		Desktop:   desktopName,
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  windows.Handle(os.Stdin.Fd()),
		StdOutput: windows.Handle(os.Stdout.Fd()),
		StdErr:    windows.Handle(os.Stderr.Fd()),
	}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	var process windows.ProcessInformation
	if err := windows.CreateProcess(appName, commandLine, nil, nil, true, windows.CREATE_NO_WINDOW, nil, nil, &startup, &process); err != nil {
		return 1, fmt.Errorf("start %s on desktop %s: %w", targetPath, desktop, err)
	}
	defer windows.CloseHandle(process.Process)
	_ = windows.CloseHandle(process.Thread)
	if _, err := windows.WaitForSingleObject(process.Process, windows.INFINITE); err != nil {
		return 1, fmt.Errorf("wait for %s on desktop %s: %w", targetPath, desktop, err)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return 1, fmt.Errorf("read the exit code of %s on desktop %s: %w", targetPath, desktop, err)
	}
	return int(exitCode), nil
}

// windowsCommandLine joins args into one command line exactly as os/exec
// does on Windows: each argument escaped, separated by spaces.
func windowsCommandLine(args []string) string {
	escaped := make([]string, len(args))
	for i, arg := range args {
		escaped[i] = syscall.EscapeArg(arg)
	}
	return strings.Join(escaped, " ")
}
