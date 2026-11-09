//go:build windows

package realpath

import (
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// normalizedDOSName asks GetFinalPathNameByHandle for FILE_NAME_NORMALIZED |
// VOLUME_NAME_DOS, a drive-letter or UNC path; x/sys does not name the flags.
const normalizedDOSName = 0

// resolve opens abs and asks Windows for the final path of the open handle,
// which the kernel has already walked through every symlink, junction and
// mount point, whatever volume each one targets.
func resolve(abs string) (string, error) {
	file, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer file.Close()
	buffer := make([]uint16, windows.MAX_PATH)
	for {
		length, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), normalizedDOSName)
		if err != nil {
			return "", &fs.PathError{Op: "GetFinalPathNameByHandle", Path: abs, Err: err}
		}
		if length < uint32(len(buffer)) {
			return trimExtendedPrefix(windows.UTF16ToString(buffer[:length])), nil
		}
		buffer = make([]uint16, length)
	}
}

// trimExtendedPrefix turns the \\?\ form GetFinalPathNameByHandle returns
// back into the ordinary drive-letter or UNC spelling.
func trimExtendedPrefix(path string) string {
	if rest, ok := strings.CutPrefix(path, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(path, `\\?\`)
}
