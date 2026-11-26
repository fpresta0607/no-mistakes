package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A daemon started from Git Bash inherits the PATH Git Bash's profile built,
// with Git's MSYS folders ahead of the Windows system folders, so every step
// command resolved bash, find and sort to Git's MSYS tools instead of the ones
// the user's own Windows terminal finds (#322).
func TestPrepareDaemonEnvironment_KeepsAGitBashLaunchersMSYSFoldersBehindTheSystemFolders(t *testing.T) {
	tests := []struct {
		name       string
		msystem    string
		wantNative bool
	}{
		{name: "launched from Git Bash", msystem: "MINGW64", wantNative: true},
		{name: "launched from a Windows terminal", msystem: "", wantNative: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			p := daemonEnvironmentTestPaths(t)
			base := t.TempDir()
			gitRoot := filepath.Join(base, "Git")
			home := filepath.Join(base, "home", "bin")
			system := filepath.Join(base, "Windows", "System32")
			mingw := filepath.Join(gitRoot, "mingw64", "bin")
			usrLocal := filepath.Join(gitRoot, "usr", "local", "bin")
			usr := filepath.Join(gitRoot, "usr", "bin")
			gitCmd := filepath.Join(gitRoot, "cmd")
			for _, file := range []string{
				filepath.Join(system, "bash.exe"),
				filepath.Join(system, "find.exe"),
				filepath.Join(usr, "msys-2.0.dll"),
				filepath.Join(usr, "bash.exe"),
				filepath.Join(usr, "find.exe"),
				filepath.Join(usr, "sort.exe"),
				filepath.Join(mingw, "git.exe"),
				filepath.Join(gitCmd, "git.exe"),
				filepath.Join(home, "tool.exe"),
			} {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, nil, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(usrLocal, 0o755); err != nil {
				t.Fatal(err)
			}
			launcherPath := strings.Join([]string{home, mingw, usrLocal, usr, system, gitCmd}, string(os.PathListSeparator))
			t.Setenv("PATH", launcherPath)
			t.Setenv("MSYSTEM", test.msystem)
			oldApply := applyShellEnvToProcess
			t.Cleanup(func() { applyShellEnvToProcess = oldApply })
			applyShellEnvToProcess = func(...string) error { return nil }
			wantPath := launcherPath
			want := map[string]string{
				"bash": filepath.Join(usr, "bash.exe"),
				"find": filepath.Join(usr, "find.exe"),
				"git":  filepath.Join(mingw, "git.exe"),
			}
			if test.wantNative {
				wantPath = strings.Join([]string{home, system, gitCmd, mingw, usrLocal, usr}, string(os.PathListSeparator))
				want = map[string]string{
					"bash": filepath.Join(system, "bash.exe"),
					"find": filepath.Join(system, "find.exe"),
					"sort": filepath.Join(usr, "sort.exe"),
					"git":  filepath.Join(gitCmd, "git.exe"),
					"tool": filepath.Join(home, "tool.exe"),
				}
			}

			// Act
			if err := prepareDaemonEnvironment(p); err != nil {
				t.Fatal(err)
			}

			// Assert
			if got := os.Getenv("PATH"); got != wantPath {
				t.Errorf("PATH = %q, want %q", got, wantPath)
			}
			for name, wantResolved := range want {
				got, err := exec.LookPath(name)
				if err != nil {
					t.Errorf("look up %s: %v", name, err)
					continue
				}
				if !strings.EqualFold(got, wantResolved) {
					t.Errorf("%s resolves to %q, want %q", name, got, wantResolved)
				}
			}
		})
	}
}
