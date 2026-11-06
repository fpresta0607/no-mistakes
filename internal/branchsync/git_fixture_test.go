package branchsync

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gitFixtureRoot holds the one built copy of each cached fixture's
// repositories. TestMain places it in its temporary directory and removes it.
var gitFixtureRoot string

var gitFixtures sync.Map

type gitFixture[T any] struct {
	once  sync.Once
	dir   string
	heads T
	built bool
}

// copyGitFixture runs build once per test process for key and copies the
// repositories it left in its root into a fresh temporary root. Building a
// fixture's repositories costs ~20 git.exe spawns, and those spawns set this
// package's Windows wall time; a copy costs one spawn per worktree. Every copy
// shares what build leaves, so build must remove any clone or linked worktree
// it made: their Git metadata names the build root as a path.
func copyGitFixture[T any](t *testing.T, key string, build func(t *testing.T, root string) T) (string, T) {
	t.Helper()
	cached, _ := gitFixtures.LoadOrStore(key, &gitFixture[T]{})
	fixture := cached.(*gitFixture[T])
	fixture.once.Do(func() {
		fixture.dir = filepath.Join(gitFixtureRoot, key)
		if err := os.MkdirAll(fixture.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		fixture.heads = build(t, fixture.dir)
		fixture.built = true
	})
	if !fixture.built {
		t.Fatalf("git fixture %s failed to build", key)
	}
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(fixture.dir)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			// A copy changes every file's stat data; refresh the index now
			// so the first status a test runs does not rewrite it.
			mustRun(t, dir, "update-index", "-q", "--refresh")
		}
	}
	return root, fixture.heads
}

// copyGitFixture reads a cached fixture while other tests copy it, so nothing
// may still write there once build returns. Git's automatic maintenance
// detaches and holds objects/maintenance.lock past the push that started it,
// and a copy that listed the lock failed when it vanished before the open.
// Git's trace is the oracle: it records every child process git starts.
func TestFixtureBuildStartsNoBackgroundMaintenance(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "git-trace")
	t.Setenv("GIT_TRACE", trace)

	buildRecoverRepositoriesAtSubmittedHead(t, t.TempDir())

	log, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "receive-pack") {
		t.Fatalf("trace missed the fixture's push, so it proves nothing:\n%s", log)
	}
	if strings.Contains(string(log), "git maintenance run") {
		t.Fatalf("fixture build started automatic maintenance:\n%s", log)
	}
}

func removeScratchClone(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
}
