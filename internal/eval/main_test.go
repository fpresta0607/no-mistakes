package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	globalConfigDir, err := os.MkdirTemp("", "no-mistakes-eval-tests-")
	if err != nil {
		panic(err)
	}
	globalConfig := filepath.Join(globalConfigDir, "gitconfig")
	if err := os.WriteFile(globalConfig, []byte(testgit.NoBackgroundMaintenanceConfig), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	code := m.Run()
	os.RemoveAll(globalConfigDir)
	os.Exit(code)
}

// Issue #109: this package walks or copies repositories its tests just ran git
// in, so a detached automatic maintenance holding objects/maintenance.lock
// there could make that walk fail when the lock vanished before the open.
func TestFixtureGitStartsNoBackgroundMaintenance(t *testing.T) {
	testgit.RequireNoBackgroundMaintenance(t)
}
