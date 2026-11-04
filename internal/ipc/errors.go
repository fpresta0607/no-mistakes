package ipc

import (
	"errors"
	"fmt"
)

// ErrEndpointProcessDead classifies an endpoint file whose owning process is no
// longer running. On Windows the endpoint file records the owner's pid, so this
// is definitive proof that no live daemon owns the endpoint (after a reboot or
// an unclean exit), unlike a connect timeout that may belong to a wedged but
// live daemon.
var ErrEndpointProcessDead = errors.New("daemon endpoint owner is no longer running")

// DeadEndpointError names the dead owner pid while matching
// ErrEndpointProcessDead, so callers can classify the condition with errors.Is.
type DeadEndpointError struct {
	PID int
}

func (e *DeadEndpointError) Error() string {
	return fmt.Sprintf("daemon process %d is no longer running", e.PID)
}

func (e *DeadEndpointError) Is(target error) bool {
	return target == ErrEndpointProcessDead
}
