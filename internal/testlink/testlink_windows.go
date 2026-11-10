//go:build windows

package testlink

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// Junction makes link a directory junction to the existing directory target.
func Junction(t *testing.T, link, target string) {
	t.Helper()
	command := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	winproc.Harden(command)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v\n%s", link, target, err, output)
	}
	t.Cleanup(func() { os.Remove(link) })
}

// OtherVolumeDir returns a new directory, removed when the test ends, on a
// different volume from dir. It skips the test, saying why, when no
// candidate location sits on another volume.
func OtherVolumeDir(t *testing.T, dir string) string {
	t.Helper()
	volume := filepath.VolumeName(dir)
	var tried []string
	for _, key := range []string{"RUNNER_TEMP", "LOCALAPPDATA", "USERPROFILE", "TEMP", "TMP"} {
		candidate := os.Getenv(key)
		if candidate == "" || strings.EqualFold(filepath.VolumeName(candidate), volume) {
			continue
		}
		tried = append(tried, key+"="+candidate)
		created, err := os.MkdirTemp(candidate, "nm-other-volume-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { os.RemoveAll(created) })
		return created
	}
	t.Skipf("no writable directory on a volume other than %s (tried %v): a cross-volume junction needs a second volume, which the Windows CI runner provides", volume, tried)
	return ""
}
