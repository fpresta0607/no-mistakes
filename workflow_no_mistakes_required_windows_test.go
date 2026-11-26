//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The composite-action runner took the first bash on PATH, so on a Windows
// host where the WSL launcher leads PATH an ordinary go test started WSL to
// run the action's script (issue #326). A stand-in launcher leads this test's
// own PATH, which holds no system folder, so no real launcher can start. With
// no python on that PATH the action's own script reports it, which only a
// real bash running the script can print.
func TestRequiredWorkflowRunsTheActionInGitBashWhenALauncherLeadsPATH(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable to locate Git for Windows' bash")
	}
	standIns, launcherMark := buildStandInBashLauncher(t)
	t.Setenv("PATH", strings.Join([]string{standIns, filepath.Dir(gitPath)}, string(os.PathListSeparator)))
	t.Setenv("STAND_IN_LAUNCHER_MARK", launcherMark)

	conclusion, output := runRequiredWorkflowCheckJob(t, loadRequiredWorkflow(t), requiredWorkflowEvent{
		Action: "opened", Body: "signature absent", HeadSHA: requiredWorkflowTestHeadSHA, PRNumber: 549,
	})

	if _, err := os.Stat(launcherMark); err == nil {
		t.Fatalf("the required workflow started the bash leading PATH instead of Git for Windows' bash; output:\n%s", output)
	}
	if conclusion != "failure" || !strings.Contains(output, "python interpreter not found") {
		t.Fatalf("conclusion = %q, want the action's script to run in Git for Windows' bash and report the missing python; output:\n%s", conclusion, output)
	}
}
