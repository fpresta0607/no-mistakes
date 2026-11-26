// Package testwait bounds a test's wait for an executor to reach a boundary,
// park, or return once cancelled.
package testwait

import (
	"testing"
	"time"
)

// Timeout bounds each such wait. Loaded Windows and macOS runners outlasted
// fixed 3 to 15 second windows with no code fault (issues #75, #340, #355 and
// #451), so a correct test must not fail on its timer, while a real hang still
// fails well inside go test's own timeout.
const Timeout = time.Minute

// Receive returns the next value from ch, failing the test with "<what>
// within Timeout" when none arrives in time.
func Receive[T any](t testing.TB, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(Timeout):
		t.Fatalf("%s within %s", what, Timeout)
		var zero T
		return zero
	}
}
