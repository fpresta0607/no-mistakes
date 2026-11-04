//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDaemonTemporaryDirectorySurvivesLauncherCleanup(t *testing.T) {
	nmBinary, fakeBinary := buildBinaries(t)
	root, err := os.MkdirTemp("", "nm-e2e-temp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	h := &Harness{
		t: t, NMBin: nmBinary, FakeAgent: fakeBinary,
		BinDir: filepath.Join(root, "bin"), NMHome: filepath.Join(root, "nmhome"),
		HomeDir: filepath.Join(root, "home"), WorkDir: filepath.Join(root, "work"),
		UpstreamDir: filepath.Join(root, "upstream.git"), AgentLog: filepath.Join(root, "fakeagent.log"),
		agentName: "claude", globalConfigExtra: "intent:\n  enabled: false\n",
	}
	for _, dir := range []string{h.BinDir, h.NMHome, h.HomeDir, h.WorkDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Copies keep this lifecycle regression runnable without Windows symlink privileges.
	fake, err := os.ReadFile(fakeBinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "gh", "tea"} {
		if err := os.WriteFile(filepath.Join(h.BinDir, executableName(name)), fake, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", h.BinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", h.HomeDir)
	t.Setenv("USERPROFILE", h.HomeDir)
	t.Setenv("NM_HOME", h.NMHome)
	t.Setenv("FAKEAGENT_LOG", h.AgentLog)
	t.Setenv("FAKEAGENT_SCENARIO", "")
	t.Setenv("FAKEAGENT_FIXTURE", "")
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", e2eDaemonStartTimeout)
	t.Setenv("NO_MISTAKES_TELEMETRY", "off")
	t.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "absent-gitconfig"))
	if runtime.GOOS != "windows" {
		t.Setenv("SHELL", "/bin/sh")
	}
	h.writeGlobalConfig()
	h.initGitRepos()
	h.daemonOwn, err = e2edaemon.Acquire(h.NMHome, h.NMBin, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var daemonPID int
	t.Cleanup(func() {
		h.shutdown()
		if daemonPID > 0 {
			if isAlive, err := e2edaemon.ProcessAlive(daemonPID); err != nil || isAlive {
				t.Errorf("isolated daemon pid=%d remains alive=%t after cleanup: %v", daemonPID, isAlive, err)
			}
			t.Logf("released isolated daemon pid=%d root=%s", daemonPID, h.NMHome)
		}
	})
	h.CommitChange("main", "check-temp.go", `package main

import (
    "encoding/json"
    "os"
    "path/filepath"
)

func main() {
    created := make(map[string]string)
    for _, key := range []string{"", "TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
        file, err := os.CreateTemp(os.Getenv(key), "configured-command-*")
        if err != nil { panic(err) }
        if err := file.Close(); err != nil { panic(err) }
        created[key] = file.Name()
        if err := os.Remove(file.Name()); err != nil { panic(err) }
    }
    data, err := json.Marshal(created)
    if err != nil { panic(err) }
    if err := os.WriteFile(filepath.Join(os.Getenv("NM_HOME"), "created-temp.json"), data, 0600); err != nil { panic(err) }
}
`, "add configured temporary-file check")
	h.CommitChange("main", ".no-mistakes.yaml", `gates:
  - name: temporary-files
    after: rebase
    command: go run check-temp.go
`, "configure temporary-file gate")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted gate config: %v\n%s", err, out)
	}
	scratch := t.TempDir()
	launcherEnv := map[string]string{}
	for _, key := range []string{"TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
		launcherEnv[key] = scratch
	}
	if out, err := h.RunInDirWithEnv(h.WorkDir, launcherEnv, "daemon", "start"); err != nil {
		t.Fatalf("start isolated daemon: %v\n%s", err, out)
	}
	daemonPID, err = daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("owned daemon pid=%d root=%s launcher_scratch=%s binary=%s", daemonPID, h.NMHome, scratch, h.NMBin)
	if err := os.RemoveAll(scratch); err != nil {
		t.Fatalf("remove test-owned launcher scratch: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init after launcher cleanup: %v\n%s", err, out)
	}
	const branch = "feature/daemon-temp"
	h.CommitChange(branch, "feature.txt", "temporary files survive\n", "exercise configured command")
	h.PushToGate(branch)

	const gate = types.StepName("gate.rebase.temporary-files")
	deadline := time.Now().Add(120 * time.Second)
	isCompleted := false
	for time.Now().Before(deadline) && !isCompleted {
		for _, run := range h.Runs() {
			if run.Branch != branch {
				continue
			}
			for _, step := range run.Steps {
				if step.StepName != gate {
					continue
				}
				if step.Status == types.StepStatusCompleted {
					isCompleted = true
					break
				}
				if step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFailed {
					h.dumpDebugState()
					t.Fatalf("configured temporary-file command status = %s", step.Status)
				}
			}
			if !isCompleted && run.Status.Terminal() {
				h.dumpDebugState()
				t.Fatalf("run ended before configured command passed: %s", run.Status)
			}
		}
		if !isCompleted {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !isCompleted {
		h.dumpDebugState()
		t.Fatal("configured temporary-file command did not complete")
	}
	data, err := os.ReadFile(filepath.Join(h.NMHome, "created-temp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]string
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h.NMHome, "tmp")
	for _, key := range []string{"", "TEMP", "TMP", "TMPDIR", "GOTMPDIR"} {
		if got := created[key]; !filepath.IsAbs(got) || filepath.Dir(got) != want {
			t.Errorf("temporary file for %q = %q, want absolute file under %q outside %q", key, got, want, h.WorkDir)
		}
	}
	t.Logf("configured command created temporary files: %s", data)
}
