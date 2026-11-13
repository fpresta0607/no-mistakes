package e2edaemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A sibling wrapper's pre-reap stops every inventory whose owner it reads as
// dead, so a running wrapper has to record a PID the reaper can see alive. Git
// Bash's $$ is its own numbering rather than a Windows PID, and recording it
// made concurrent suites stop each other's live daemons.
func TestE2EWrapperRecordsAnOwnerTheReaperSeesAlive(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable to run the suite wrapper")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	ownerCopy := filepath.ToSlash(filepath.Join(scratch, "owner"))
	release := filepath.ToSlash(filepath.Join(scratch, "release"))
	// The stand-in for go holds the wrapper open exactly as a running suite does.
	fakeGo := "#!/bin/sh\n[ \"$1\" = test ] || exit 0\ncp \"$NM_E2E_DAEMON_INVENTORY/owner.pid\" '" + ownerCopy + ".tmp'\nmv '" + ownerCopy + ".tmp' '" + ownerCopy + "'\nwhile [ ! -e '" + release + "' ]; do sleep 0.2; done\n"
	if err := os.WriteFile(filepath.Join(scratch, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	wrapper := exec.CommandContext(ctx, bash, "scripts/e2e.sh", "test")
	wrapper.Dir = root
	wrapper.Env = append(filterInventoryEnv(os.Environ()), "PATH="+scratch+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- wrapper.Wait() }()

	var owner []byte
	for owner == nil {
		select {
		case err := <-finished:
			t.Fatalf("wrapper exited before its suite started: %v", err)
		case <-ctx.Done():
			t.Fatal("wrapper never recorded its owner")
		case <-time.After(20 * time.Millisecond):
		}
		owner, _ = os.ReadFile(ownerCopy)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(owner)))
	isAlive, aliveErr := processAlive(pid)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatalf("wrapper: %v", err)
	}

	if parseErr != nil || aliveErr != nil || !isAlive {
		t.Fatalf("running wrapper's owner %q reads as alive=%t (%v, %v); a sibling's pre-reap would stop its daemons", owner, isAlive, parseErr, aliveErr)
	}
}

func filterInventoryEnv(environment []string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, "PATH") && name != EnvInventory && name != "NM_E2E_DAEMON_INVENTORY_PARENT" {
			kept = append(kept, entry)
		}
	}
	return kept
}
