//go:build windows

package testgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// The Unix test step guard took the first bash on PATH and then ran it to ask
// whether it was Git Bash, so a WSL launcher leading PATH was started (issue
// #165). A stand-in launcher leads this test's own PATH, which holds no system
// folder, so no real launcher can start, and the stand-in records any start.
func TestBash_SelectsGitBashWithoutStartingALauncherLeadingPATH(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable to locate Git for Windows' bash")
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go unavailable to build the stand-in launcher")
	}
	scratch := t.TempDir()
	standIns := filepath.Join(scratch, "stand-ins")
	launcherSource := filepath.Join(scratch, "launcher", "main.go")
	launcherMark := filepath.Join(scratch, "launcher-ran")
	for _, dir := range []string{standIns, filepath.Dir(launcherSource)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	launcher := "package main\n\nimport \"os\"\n\nfunc main() {\n\t_ = os.WriteFile(os.Getenv(\"STAND_IN_LAUNCHER_MARK\"), []byte(\"ran\"), 0o600)\n\tos.Exit(1)\n}\n"
	if err := os.WriteFile(launcherSource, []byte(launcher), 0o644); err != nil {
		t.Fatal(err)
	}
	standIn := filepath.Join(standIns, "bash.exe")
	build := exec.Command(goPath, "build", "-o", standIn, "main.go")
	build.Dir = filepath.Dir(launcherSource)
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	winproc.Harden(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stand-in launcher: %v\n%s", err, output)
	}
	t.Setenv("PATH", strings.Join([]string{standIns, filepath.Dir(gitPath)}, string(os.PathListSeparator)))
	t.Setenv("STAND_IN_LAUNCHER_MARK", launcherMark)

	bash, err := Bash()

	if _, statErr := os.Stat(launcherMark); statErr == nil {
		t.Fatal("Bash started the bash leading PATH to ask what it is")
	}
	if err != nil {
		t.Fatalf("Bash() error = %v, want Git for Windows' bash", err)
	}
	if strings.EqualFold(filepath.Clean(bash), standIn) {
		t.Fatalf("Bash() = %q, the stand-in launcher leading PATH, want Git for Windows' bash", bash)
	}
	if !strings.EqualFold(filepath.Base(bash), "bash.exe") {
		t.Fatalf("Bash() = %q, want a bash.exe", bash)
	}
	if _, err := os.Stat(bash); err != nil {
		t.Fatalf("Bash() = %q, which does not exist: %v", bash, err)
	}
}
