package testgit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Bash returns the bash a test runs the repository's shell scripts in. On
// Windows a bare bash can resolve to the WSL launcher, which would hand a
// checkout path to a Linux shell, so the only candidate is Git for Windows'
// own bash, found beside the git on PATH as make e2e finds it. PATH's bash is
// never consulted there and no candidate is started to ask what it is, since
// starting the launcher is what starts WSL.
func Bash() (string, error) {
	if runtime.GOOS != "windows" {
		return exec.LookPath("bash")
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", fmt.Errorf("testgit: git unavailable to locate Git for Windows' bash: %w", err)
	}
	bash := filepath.Join(strings.TrimSpace(string(execPath)), "..", "..", "..", "bin", "bash.exe")
	if _, err := os.Stat(bash); err != nil {
		return "", fmt.Errorf("testgit: Git for Windows' bash unavailable: %w", err)
	}
	return bash, nil
}
