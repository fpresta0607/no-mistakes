//go:build !windows

package testlink

import "testing"

// Junction skips the test: directory junctions exist only on Windows.
func Junction(t *testing.T, link, target string) {
	t.Helper()
	t.Skip("directory junctions exist only on Windows")
}

// OtherVolumeDir skips the test: only Windows paths name a volume.
func OtherVolumeDir(t *testing.T, dir string) string {
	t.Helper()
	t.Skip("cross-volume junctions exist only on Windows")
	return ""
}
