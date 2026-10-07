//go:build !windows

package gatecontext

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// processInfo reports no creation time: an orphan is reparented to init or a
// subreaper here, so a recorded parent pid never outlives its process.
func processInfo(pid int) (ProcessInfo, error) {
	if pid <= 1 {
		return ProcessInfo{}, nil
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		// The authenticated client can disappear only after sending its request;
		// treat an already-gone process as the end of the chain.
		if _, ok := err.(*exec.ExitError); ok {
			return ProcessInfo{}, nil
		}
		return ProcessInfo{}, err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return ProcessInfo{}, nil
	}
	ppid, err := strconv.Atoi(value)
	if err != nil {
		return ProcessInfo{}, fmt.Errorf("parse parent pid: %w", err)
	}
	return ProcessInfo{ParentPID: ppid}, nil
}
