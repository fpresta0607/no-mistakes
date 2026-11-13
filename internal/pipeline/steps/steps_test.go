package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestMain(m *testing.M) {
	if handled, exitCode, err := shellenv.RunWindowsCooperativeCommandHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode)
	}

	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit)
	// via GIT_CONFIG_COUNT/KEY_n/VALUE_n; tests that need it re-set it with
	// t.Setenv (issue #362).
	os.Unsetenv("GIT_CONFIG_COUNT")
	// Fixtures must not depend on the machine's system git config: some
	// distributions ship safe.bareRepository=explicit there, which refuses
	// the in-directory use of the bare fixture repositories these tests
	// create. Mirrors internal/git's TestMain.
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Fixtures must not inherit the developer's global git config: a
	// commit.gpgsign=true there makes every fixture commit ask gpg to sign.
	globalConfigDir, err := os.MkdirTemp("", "no-mistakes-steps-tests-")
	if err != nil {
		panic(err)
	}
	globalConfig := filepath.Join(globalConfigDir, "gitconfig")
	if err := os.WriteFile(globalConfig, []byte(testgit.NoBackgroundMaintenanceConfig), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	cleanup, err := stepstest.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init fake CLI helper: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup fake CLI helper: %v\n", err)
		code = 1
	}
	os.RemoveAll(globalConfigDir)
	os.Exit(code)
}

// Issue #109: this package walks or copies repositories its tests just ran git
// in, so a detached automatic maintenance holding objects/maintenance.lock
// there could make that walk fail when the lock vanished before the open.
func TestFixtureGitStartsNoBackgroundMaintenance(t *testing.T) {
	testgit.RequireNoBackgroundMaintenance(t)
}
