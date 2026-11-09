//go:build !windows

package realpath

import "path/filepath"

func resolve(abs string) (string, error) {
	return filepath.EvalSymlinks(abs)
}
