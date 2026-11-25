//go:build !unix && !windows

package shellenv

import (
	"errors"
	"os/exec"
	"time"
)

// ShellCommandMembers is never recorded on platforms without process groups.
type ShellCommandMembers struct{}

func RecordShellCommandMembers(*exec.Cmd, time.Time) (*ShellCommandMembers, error) {
	return nil, errors.New("process-group members cannot be recorded on this platform")
}

func (m *ShellCommandMembers) Kill() {}

func (m *ShellCommandMembers) Release() {}
