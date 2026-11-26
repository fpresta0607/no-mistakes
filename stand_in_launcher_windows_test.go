//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// buildStandInBashLauncher builds a bash.exe that stands in for the WSL
// launcher: started, it writes the returned mark file and exits 1. It returns
// the directory holding it, for a test to lead its own PATH with, so no real
// launcher can start and any start of the stand-in is recorded.
func buildStandInBashLauncher(t *testing.T) (standIns, launcherMark string) {
	t.Helper()
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go unavailable to build the stand-in launcher")
	}
	scratch := t.TempDir()
	standIns = filepath.Join(scratch, "stand-ins")
	launcherSource := filepath.Join(scratch, "launcher", "main.go")
	launcherMark = filepath.Join(scratch, "launcher-ran")
	for _, dir := range []string{standIns, filepath.Dir(launcherSource)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	launcher := "package main\n\nimport \"os\"\n\nfunc main() {\n\t_ = os.WriteFile(os.Getenv(\"STAND_IN_LAUNCHER_MARK\"), []byte(\"ran\"), 0o600)\n\tos.Exit(1)\n}\n"
	if err := os.WriteFile(launcherSource, []byte(launcher), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goPath, "build", "-o", filepath.Join(standIns, "bash.exe"), "main.go")
	build.Dir = filepath.Dir(launcherSource)
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	winproc.Harden(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stand-in launcher: %v\n%s", err, output)
	}
	return standIns, launcherMark
}
