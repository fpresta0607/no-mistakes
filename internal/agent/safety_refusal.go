package agent

import (
	"errors"
	"fmt"
	"strings"
)

var ErrSafetyRefusal = errors.New("native agent safety refusal")

// IsSafetyRefusal recognizes the explicit native refusal reported by Codex.
// Keep this specific to the provider's refusal, rather than ordinary errors
// such as a refused network connection or a tool permission failure.
func IsSafetyRefusal(err error) bool {
	return errors.Is(err, ErrSafetyRefusal)
}

// refusedSessionError carries the native session a safety refusal ended on the
// error itself, because every wrapper between the adapter and the session
// manager passes the error through while most drop a failed call's Result.
type refusedSessionError struct {
	err     error
	session SessionRef
}

func (e *refusedSessionError) Error() string { return e.err.Error() }

func (e *refusedSessionError) Unwrap() error { return e.err }

// NewSafetyRefusal marks err as a native safety refusal and, when the provider
// reported the session it ended, carries that session with it.
func NewSafetyRefusal(err error, session SessionRef) error {
	err = fmt.Errorf("%w: %w", ErrSafetyRefusal, err)
	if session.ID == "" {
		return err
	}
	return &refusedSessionError{err: err, session: session}
}

// RefusedSession returns the native session a safety refusal ended, so a
// fresh session's refusal is recorded like a resumed one's.
func RefusedSession(err error) (SessionRef, bool) {
	var refused *refusedSessionError
	if !errors.As(err, &refused) {
		return SessionRef{}, false
	}
	return refused.session, true
}

func isNativeSafetyRefusal(message string) bool {
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "this content was flagged for possible cybersecurity risk") {
			return true
		}
	}
	return false
}
