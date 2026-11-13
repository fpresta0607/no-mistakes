// Package testgit resolves the real system git binary for tests that spawn
// a fake CLI on PATH (see internal/pipeline/fakecli). It exists because
// exec.LookPath("git") returns the PATH winner, not necessarily real git: if
// a passthrough git wrapper (guard shim, audit wrapper, etc.) sits on the
// developer's PATH, that wrapper gets mistaken for "real git" and, once the
// fake CLI directory is prepended ahead of it, the two forward into each
// other without bound (github.com/ironerumi/no-mistakes#5).
package testgit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// RealGit resolves the system git binary by absolute path only, never
// consulting PATH: a wrapper (guard shim, audit wrapper, etc.) or the fake
// CLI itself can win PATH, and either one being mistaken for "real git" is
// what let fakecli and a wrapper forward into each other without bound
// (github.com/ironerumi/no-mistakes#5). Restricting resolution to a fixed
// list of well-known install locations means PATH is never consulted, so
// neither a wrapper nor the fake CLI can ever be selected.
func RealGit() (string, error) {
	locations := gitLocations()
	for _, p := range locations {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() &&
			(runtime.GOOS == "windows" || fi.Mode()&0111 != 0) {
			return p, nil
		}
	}
	return "", fmt.Errorf("testgit: real git not found in standard locations (%s)", strings.Join(locations, ", "))
}

func gitLocations() []string {
	if runtime.GOOS != "windows" {
		return []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"}
	}

	locations := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	appendRoot := func(root string) {
		if root == "" || !filepath.IsAbs(root) {
			return
		}
		for _, p := range []string{
			filepath.Join(root, "Git", "cmd", "git.exe"),
			filepath.Join(root, "Git", "bin", "git.exe"),
		} {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			locations = append(locations, p)
		}
	}

	appendRoot(os.Getenv("ProgramFiles"))
	appendRoot(`C:\Program Files`)
	appendRoot(os.Getenv("ProgramFiles(x86)"))
	appendRoot(`C:\Program Files (x86)`)
	return locations
}

// NoBackgroundMaintenanceConfig is the global git config line a test package
// writes so its fixture repositories never start git's automatic maintenance.
const NoBackgroundMaintenanceConfig = "[maintenance]\n\tauto = false\n"

// RequireNoBackgroundMaintenance fails t when a commit made with the calling
// test's git environment starts git's automatic maintenance. Every commit,
// fetch and push otherwise starts `git maintenance run --auto --detach`,
// which takes objects/maintenance.lock in the repository and can outlive the
// command, so a test that walks or copies that repository afterwards can list
// the lock and fail when it vanishes before the open (issue #109). Git's
// trace records every child process git starts, so it is the oracle.
func RequireNoBackgroundMaintenance(t *testing.T) {
	t.Helper()
	git, err := RealGit()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	trace := filepath.Join(t.TempDir(), "git-trace")
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=probe", "-c", "user.email=probe@example.com", "commit", "-q", "--allow-empty", "-m", "probe"},
	} {
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_TRACE="+trace)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	log, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "commit") {
		t.Fatalf("trace missed the probe commit, so it proves nothing:\n%s", log)
	}
	if strings.Contains(string(log), "maintenance run") {
		t.Fatalf("a fixture commit started git's automatic maintenance; write testgit.NoBackgroundMaintenanceConfig into the package's global git config:\n%s", log)
	}
}
