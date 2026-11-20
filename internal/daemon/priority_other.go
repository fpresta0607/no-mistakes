//go:build !windows

package daemon

// raiseInheritedPriority is Windows-only; see priority_windows.go.
func raiseInheritedPriority() (string, error) {
	return "", nil
}
