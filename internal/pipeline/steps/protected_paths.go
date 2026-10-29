package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// githubMaxFileBytes is GitHub's file size limit: it refuses a push carrying
// any file larger than 100 MiB.
const githubMaxFileBytes = 100 << 20

// stagePipelineChanges guards every pipeline-owned catch-all staging path,
// including Push's leftover commit. Refusal preserves the index and worktree.
//
// before lists the untracked files the worktree held when the work being
// committed began, as untrackedFiles read them. Those files are left out,
// unstaged, and named in the step log: a correction commits the files its own
// turn wrote, never what an earlier step left behind. A Test evidence turn
// once left a Home Assistant runtime, a downloaded tool and copied third-party
// source in the worktree, a test command left its scratch beside them, and
// the next fix round's `git add -A` committed 636 of those files to the branch
// (fpresta0607/code-goblins#1316). A nil before leaves no file out for that
// reason.
//
// An untracked file larger than githubMaxFileBytes is left out too, unstaged
// and unhashed, and named in the step log. Such a file is an agent's disposable
// artifact rather than a correction: a Test agent whose cleanup was refused
// left a whole Chrome build in the worktree, `git add -A` committed its 302 MB
// chrome.dll, and the push could never succeed (#1076).
func stagePipelineChanges(sctx *pipeline.StepContext, before []string) error {
	leftBehind := make(map[string]bool, len(before))
	for _, file := range before {
		leftBehind[file] = true
	}
	if len(sctx.Config.ProtectedPaths) > 0 {
		// Disable renames so both source and destination are checked, and list
		// individual untracked files so a protected path inside a new directory
		// cannot hide behind the directory entry. NULs preserve unusual names.
		status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
		if err != nil {
			return fmt.Errorf("check protected_paths: %w", err)
		}
		for _, entry := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			if len(entry) < 4 || entry[2] != ' ' {
				return fmt.Errorf("check protected_paths: invalid git status entry %q", entry)
			}
			file := entry[3:]
			if entry[:2] == "??" && leftBehind[file] {
				continue
			}
			for _, pattern := range sctx.Config.ProtectedPaths {
				if matchIgnorePattern(file, pattern) {
					return &pipeline.ProtectedPathError{Path: file, Rule: pattern}
				}
			}
		}
	}
	untracked, err := untrackedFiles(sctx)
	if err != nil {
		return err
	}
	var notMade, oversized []string
	for _, file := range untracked {
		if leftBehind[file] {
			notMade = append(notMade, file)
			continue
		}
		info, err := os.Lstat(filepath.Join(sctx.WorkDir, filepath.FromSlash(file)))
		if err != nil {
			return fmt.Errorf("check untracked file size: %w", err)
		}
		if info.Mode().IsRegular() && info.Size() > githubMaxFileBytes {
			oversized = append(oversized, file)
		}
	}
	if len(notMade) > 0 {
		sctx.Log(fmt.Sprintf("left the untracked files the worktree held before this change out of the commit (%d): %s", len(notMade), namedPaths(notMade)))
	}
	if len(oversized) > 0 {
		sctx.Log(fmt.Sprintf("left untracked files larger than GitHub's 100 MiB limit out of the commit: %s", strings.Join(oversized, ", ")))
	}
	if len(notMade) == 0 && len(oversized) == 0 {
		if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
			return err
		}
		return unstageSubmodulePointerMoves(sctx)
	}
	// The exclusions go on standard input: a step can leave hundreds of files
	// behind, and as arguments their paths would overrun Windows' command line.
	pathspecs := []string{"."}
	for _, file := range append(notMade, oversized...) {
		pathspecs = append(pathspecs, ":(exclude,literal)"+file)
	}
	if _, err := stepGitRunInput(sctx, strings.Join(pathspecs, "\x00"), "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return err
	}
	return unstageSubmodulePointerMoves(sctx)
}

// untrackedFiles lists the files `git add -A` would stage that git does not
// track yet.
func untrackedFiles(sctx *pipeline.StepContext) ([]string, error) {
	out, err := stepGitRunRaw(sctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list untracked files: %w", err)
	}
	var files []string
	for _, file := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// namedPaths names the first ten paths and counts the rest.
func namedPaths(paths []string) string {
	const maxNamed = 10
	named := strings.Join(paths[:min(len(paths), maxNamed)], ", ")
	if len(paths) > maxNamed {
		named += fmt.Sprintf(" and %d more", len(paths)-maxNamed)
	}
	return named
}

// unstageSubmodulePointerMoves keeps catch-all staging from recording a
// submodule checkout as a new pointer. A rebase moves the recorded pointer but
// not the populated checkout, so `git add -A` would commit the stale checkout
// as a silent revert of the base's submodule bump, and a populated checkout
// whose gitlink the base removed would be re-added as an embedded repository;
// pipeline corrections never move a submodule pointer. A gitlink addition the
// staged .gitmodules registers is a deliberately added submodule and stays.
func unstageSubmodulePointerMoves(sctx *pipeline.StepContext) error {
	raw, err := stepGitRunRaw(sctx, "diff", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("check staged submodule pointers: %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	var moved []string
	var registered map[string]bool
	for i := 0; i+1 < len(fields); i += 2 {
		modes := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(modes) < 2 || modes[1] != "160000" {
			continue
		}
		if modes[0] == "160000" {
			moved = append(moved, fields[i+1])
			continue
		}
		if modes[0] != "000000" {
			continue
		}
		if registered == nil {
			if registered, err = stagedRegisteredSubmodules(sctx); err != nil {
				return fmt.Errorf("check staged submodule pointers: %w", err)
			}
		}
		if !registered[fields[i+1]] {
			moved = append(moved, fields[i+1])
		}
	}
	if len(moved) == 0 {
		return nil
	}
	if _, err := stepGitRun(sctx, append([]string{"--literal-pathspecs", "reset", "-q", "--"}, moved...)...); err != nil {
		return fmt.Errorf("unstage submodule pointers: %w", err)
	}
	sctx.Log(fmt.Sprintf("left submodule pointers as recorded: %s", strings.Join(moved, ", ")))
	return nil
}

// stagedRegisteredSubmodules returns the submodule paths the staged .gitmodules
// registers.
func stagedRegisteredSubmodules(sctx *pipeline.StepContext) (map[string]bool, error) {
	registered := map[string]bool{}
	if _, err := stepGitRun(sctx, "cat-file", "-e", ":.gitmodules"); err != nil {
		return registered, nil
	}
	out, err := stepGitRunRaw(sctx, "config", "--null", "--blob", ":.gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return registered, nil
		}
		return nil, fmt.Errorf("list staged submodules: %w", err)
	}
	paths, err := parseRegisteredSubmodulePaths([]byte(out))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		registered[filepath.ToSlash(path)] = true
	}
	return registered, nil
}
