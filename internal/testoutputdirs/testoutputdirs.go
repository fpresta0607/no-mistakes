// Package testoutputdirs owns admission of the operator-authorized Test output
// directories supplied through axi run --test-output-dir. The CLI checks them
// before any push and the daemon checks them again where the run is admitted,
// because push options and IPC are a trust boundary.
package testoutputdirs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// Validate returns the canonical form of each requested directory, in request
// order and without duplicates, or refuses the first one that is not an
// existing absolute directory outside both the repository checkout at repoRoot
// and the no-mistakes home, and above neither. The home holds run worktrees and the default
// evidence directory, so it covers both.
func Validate(values []string, repoRoot, home string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(values))
	dirs := make([]string, 0, len(values))
	for _, value := range values {
		dir, err := admit(value, repoRoot, home)
		if err != nil {
			return nil, err
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func admit(value, repoRoot, home string) (string, error) {
	refuse := func(rule, correction string) error {
		return fmt.Errorf("--test-output-dir %s %s; %s", value, rule, correction)
	}
	if !filepath.IsAbs(value) {
		return "", refuse("is not an absolute path", "pass the directory's absolute path")
	}
	// realpath.Resolve is filepath.EvalSymlinks that also follows Windows
	// junctions, so a link into the repository or the home is judged by where
	// it lands.
	dir, err := realpath.Resolve(filepath.Clean(value))
	if errors.Is(err, fs.ErrNotExist) {
		return "", refuse("does not exist", "create it first or pass an existing directory")
	}
	if err != nil {
		return "", refuse(fmt.Sprintf("cannot be resolved (%v)", err), "pass a directory this user can open")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", refuse("is not a directory", "pass a directory")
	}
	if filepath.Dir(dir) == dir {
		return "", refuse("is a filesystem root", "choose a directory below it")
	}
	if worktrees.Contains(repoRoot, dir) {
		return "", refuse("is inside the repository", "choose a directory outside it")
	}
	if worktrees.Contains(home, dir) {
		return "", refuse("is inside the no-mistakes home", "choose a directory outside it")
	}
	// A directory above either one would authorize writes into all of it.
	if worktrees.Contains(dir, repoRoot) {
		return "", refuse("holds the repository", "choose a directory that does not contain it")
	}
	if worktrees.Contains(dir, home) {
		return "", refuse("holds the no-mistakes home", "choose a directory that does not contain it")
	}
	return dir, nil
}
