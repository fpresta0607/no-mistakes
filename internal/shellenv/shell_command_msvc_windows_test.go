//go:build windows

package shellenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestShellCommandGroupActiveAcceptsARealMSVCLinkTelemetryHelper is the issue
// #259 reproduction with the real toolchain: a compile and link that exited
// cleanly leaves link.exe's vctip.exe running in the job, and that is not an
// unfinished check. It needs an installed MSVC toolchain whose link starts the
// telemetry helper, and skips otherwise.
func TestShellCommandGroupActiveAcceptsARealMSVCLinkTelemetryHelper(t *testing.T) {
	vcvars := installedMSVCVarsScript(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.c"), []byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "call "` + vcvars + `" >nul && cl /nologo hello.c >nul"`}
	cmd.Dir = dir
	// Keeps the environment script's own telemetry out of the job, so the
	// linker's helper is the only thing left behind.
	cmd.Env = append(os.Environ(), "VSCMD_SKIP_SENDTELEMETRY=1")
	ConfigureShellCommand(cmd)
	if _, ok := shellCommandJob(cmd); !ok {
		t.Skip("job object setup unavailable")
	}
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand() error = %v", err)
	}
	defer TerminateShellCommandGroup(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("compile and link did not exit cleanly: %v", err)
	}
	procs, err := inspectJobProcesses(mustShellCommandJob(t, cmd).handle)
	if err != nil {
		t.Fatal(err)
	}
	if !jobHoldsRunningTelemetryHelper(procs) {
		t.Skip("this toolchain's link left no telemetry helper running")
	}

	active, detail, err := ShellCommandGroupActive(cmd, 0)

	if err != nil {
		t.Fatalf("ShellCommandGroupActive() error = %v", err)
	}
	if active {
		t.Fatalf("ShellCommandGroupActive() = true with only link's telemetry helper left (%s); want false", detail)
	}
}

func installedMSVCVarsScript(t *testing.T) string {
	t.Helper()
	vswhere := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft Visual Studio", "Installer", "vswhere.exe")
	out, err := exec.Command(vswhere, "-latest", "-products", "*", "-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64", "-property", "installationPath").Output()
	installation := strings.TrimSpace(string(out))
	if err != nil || installation == "" {
		t.Skip("no MSVC toolchain installed")
	}
	vcvars := filepath.Join(installation, "VC", "Auxiliary", "Build", "vcvars64.bat")
	if _, err := os.Stat(vcvars); err != nil {
		t.Skipf("no vcvars64.bat in %s", installation)
	}
	return vcvars
}

func jobHoldsRunningTelemetryHelper(procs []jobProcess) bool {
	for _, p := range procs {
		if p.state == jobProcessRunning && strings.EqualFold(filepath.Base(p.imagePath), "vctip.exe") {
			return true
		}
	}
	return false
}
