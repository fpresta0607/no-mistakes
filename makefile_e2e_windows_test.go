//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// make e2e started scripts/e2e.sh with a bare bash, and on Windows the WSL
// launcher can lead PATH, so the suite ran in a Linux shell with another
// toolchain or none (issue #153). A stand-in launcher leads this test's own
// PATH, which holds no system folder, so no real launcher can start.
func TestMakeE2ERunsTheWrapperInGitBashWhenALauncherLeadsPATH(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make unavailable to run the e2e target")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable to locate Git for Windows' bash")
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go unavailable to build the stand-in launcher")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	standIns := filepath.Join(scratch, "stand-ins")
	launcherSource := filepath.Join(scratch, "launcher", "main.go")
	launcherMark := filepath.Join(scratch, "launcher-ran")
	goLog := filepath.ToSlash(filepath.Join(scratch, "go.log"))
	temp := filepath.Join(scratch, "temp")
	for _, dir := range []string{standIns, filepath.Dir(launcherSource), temp} {
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
	// The stand-in for go answers this platform and logs the suite it is asked to run.
	fakeGo := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>'" + goLog + "'\n[ \"$1 $2\" = 'env GOOS' ] && printf 'windows\\n'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(standIns, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	target := exec.CommandContext(ctx, makePath, "e2e")
	target.Dir = root
	target.Env = append(environmentWithout(os.Environ(), "PATH", "TEMP", "TMP", "NM_E2E_DAEMON_INVENTORY", "NM_E2E_DAEMON_INVENTORY_PARENT"),
		"PATH="+strings.Join([]string{standIns, filepath.Dir(makePath), filepath.Dir(gitPath)}, string(os.PathListSeparator)),
		"TEMP="+temp,
		"TMP="+temp,
		"STAND_IN_LAUNCHER_MARK="+launcherMark,
	)
	winproc.Harden(target)
	output, runErr := target.CombinedOutput()

	if _, err := os.Stat(launcherMark); err == nil {
		t.Fatalf("make e2e started the bash leading PATH instead of Git for Windows' bash; output:\n%s", output)
	}
	if runErr != nil {
		t.Fatalf("make e2e: %v\n%s", runErr, output)
	}
	calls, err := os.ReadFile(goLog)
	if err != nil {
		t.Fatalf("make e2e never reached go: %v\n%s", err, output)
	}
	if !strings.Contains("\n"+string(calls), "\ntest -tags=e2e ") {
		t.Fatalf("make e2e did not run the suite through scripts/e2e.sh; go calls:\n%s\noutput:\n%s", calls, output)
	}
}

func environmentWithout(environment []string, names ...string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		isDropped := false
		for _, dropped := range names {
			if strings.EqualFold(name, dropped) {
				isDropped = true
				break
			}
		}
		if !isDropped {
			kept = append(kept, entry)
		}
	}
	return kept
}
