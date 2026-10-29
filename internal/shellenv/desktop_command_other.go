//go:build !windows

package shellenv

import (
	"errors"
	"os/exec"
)

// PlaceOnDesktop refuses a desktop on non-Windows platforms, which have no
// hidden desktop to start a command on. An empty desktop leaves cmd as it is.
func PlaceOnDesktop(cmd *exec.Cmd, desktop string) error {
	if desktop != "" {
		return errors.New("a hidden desktop exists only on Windows")
	}
	return nil
}

// RunWindowsDesktopCommandHelper is a no-op on non-Windows platforms.
func RunWindowsDesktopCommandHelper(args []string) (bool, int, error) {
	return false, 0, nil
}
