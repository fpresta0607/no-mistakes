//go:build unix && !linux && !darwin

package shellenv

import (
	"errors"
	"time"
)

// listGroupMembersStartedBefore cannot place a member's start precisely on
// these platforms, so nothing is recorded.
func listGroupMembersStartedBefore(int, time.Time) (map[int]int64, error) {
	return nil, errors.New("process-group member start times cannot be read on this platform")
}

func groupMemberStart(int, int) (int64, bool) {
	return 0, false
}
