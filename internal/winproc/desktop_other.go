//go:build !windows

package winproc

// HiddenDesktop exists only on Windows, where a process can be started on a
// desktop nobody sees. Elsewhere NewHiddenDesktop returns nil, whose methods
// do nothing and name no desktop.
type HiddenDesktop struct{}

// NewHiddenDesktop returns nil on non-Windows platforms.
func NewHiddenDesktop() (*HiddenDesktop, error) { return nil, nil }

// Name is empty on non-Windows platforms.
func (d *HiddenDesktop) Name() string { return "" }

// Windows reports nothing on non-Windows platforms.
func (d *HiddenDesktop) Windows() ([]Window, int) { return nil, 0 }

// Close does nothing on non-Windows platforms.
func (d *HiddenDesktop) Close() error { return nil }
