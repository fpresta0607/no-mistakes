//go:build !unix && !windows

package shellenv

import (
	"errors"
	"os/exec"
)

// ShellCommandMembers is never recorded on platforms without process groups.
type ShellCommandMembers struct{}

func RecordShellCommandMembers(cmd *exec.Cmd) (*ShellCommandMembers, error) {
	return nil, errors.New("process-group members cannot be recorded on this platform")
}

func (m *ShellCommandMembers) Kill() {}

func (m *ShellCommandMembers) Release() {}
