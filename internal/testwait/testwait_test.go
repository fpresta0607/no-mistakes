package testwait

import (
	"errors"
	"testing"
	"time"
)

func TestReceiveOutlastsASlowExecutorTeardown(t *testing.T) {
	t.Parallel()
	// Longer than the longest earlier fixed executor wait (15 seconds),
	// standing in for a slow teardown on a loaded runner.
	const slowTeardown = 16 * time.Second
	want := errors.New("executor stopped")
	done := make(chan error, 1)
	go func() {
		time.Sleep(slowTeardown)
		done <- want
	}()

	got := Receive(t, done, "executor did not stop")

	if got != want {
		t.Fatalf("Receive() = %v, want %v", got, want)
	}
}
