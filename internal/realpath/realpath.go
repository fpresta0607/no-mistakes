// Package realpath resolves a path to the file or directory it names on disk.
//
// It replaces filepath.EvalSymlinks, which cannot be trusted on Windows: since
// Go 1.23 (the winsymlink GODEBUG) a directory junction or volume mount point
// is an irregular file to os.Lstat, so EvalSymlinks refuses every path below
// one with ERROR_PATH_NOT_FOUND while os.Stat of the same path succeeds. A
// no-mistakes home whose repos, worktrees, logs or tmp folders are junctions
// to another drive is therefore unusable through EvalSymlinks (issue #172).
package realpath

import "path/filepath"

// Resolve returns the absolute path of the existing file or directory that
// path names, with every symbolic link resolved and, on Windows, every
// directory junction and mount point followed. It fails when path does not
// exist, exactly as filepath.EvalSymlinks does.
func Resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return resolve(abs)
}
