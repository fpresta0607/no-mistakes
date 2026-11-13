// Package testoutputdirs owns admission of the operator-authorized Test output
// directories supplied through axi run --test-output-dir. The CLI checks them
// before any push and the daemon checks them again where the run is admitted,
// because push options and IPC are a trust boundary.
package testoutputdirs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// location is a place the Test agent must not be authorized to write into,
// so an admitted directory may neither be inside it nor hold it.
type location struct {
	name, path string
}

// Validate returns the canonical form of each requested directory, in request
// order and without duplicates, or refuses the first one that is not an
// existing absolute directory outside, and above none of, the repository
// checkout at checkout and its linked worktrees, the no-mistakes home at
// p.Root(), every configured run worktree root, and the evidence root.
func Validate(ctx context.Context, values []string, checkout string, p *paths.Paths) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	protected, err := protectedLocations(ctx, checkout, p)
	if err != nil {
		return nil, fmt.Errorf("--test-output-dir cannot be checked: %w", err)
	}
	seen := make(map[string]bool, len(values))
	dirs := make([]string, 0, len(values))
	for _, value := range values {
		dir, err := admit(value, protected)
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

// protectedLocations reads the worktree and evidence placement from the global
// config, because worktree_roots, evidence_dir and test.evidence.local_root can
// all move run worktrees or evidence outside the home.
func protectedLocations(ctx context.Context, checkout string, p *paths.Paths) ([]location, error) {
	global, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return nil, err
	}
	listed, err := git.Run(ctx, checkout, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list the repository's worktrees: %w", err)
	}
	protected := []location{{"the repository", checkout}, {"the no-mistakes home", p.Root()}}
	for _, line := range strings.Split(listed, "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok {
			protected = append(protected, location{"a worktree of the repository", filepath.Clean(path)})
		}
	}
	for _, root := range global.WorktreeRoots {
		protected = append(protected, location{"a run worktree root", root})
	}
	localRoot := config.Merge(global, &config.RepoConfig{}).Test.Evidence.LocalRoot
	evidenceRoot, _ := p.ResolveEvidenceRoot(global.EvidenceDir, localRoot)
	protected = append(protected, location{"the evidence root", evidenceRoot}, location{"the evidence root", p.EvidenceRoot(localRoot)})
	return protected, nil
}

func admit(value string, protected []location) (string, error) {
	refuse := func(rule, correction string) error {
		return fmt.Errorf("--test-output-dir %s %s; %s", value, rule, correction)
	}
	if !filepath.IsAbs(value) {
		return "", refuse("is not an absolute path", "pass the directory's absolute path")
	}
	// realpath.Resolve is filepath.EvalSymlinks that also follows Windows
	// junctions, so a link into a protected location is judged by where it
	// lands.
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
	for _, loc := range protected {
		if worktrees.Contains(loc.path, dir) {
			return "", refuse("is inside "+loc.name, "choose a directory outside it")
		}
	}
	// A directory above a protected location would authorize writes into all of it.
	for _, loc := range protected {
		if worktrees.Contains(dir, loc.path) {
			return "", refuse("holds "+loc.name, "choose a directory that does not contain it")
		}
	}
	return dir, nil
}
