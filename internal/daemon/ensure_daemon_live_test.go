package daemon

import "testing"

// TestEnsureDaemonLeavesLiveDaemonRunning guards the other side of the
// classification: a live endpoint still reports running, so EnsureDaemon
// returns without replacing it. The second daemon would carry a different
// pid and start identity, so both are compared across the call.
func TestEnsureDaemonLeavesLiveDaemonRunning(t *testing.T) {
	p, _ := startTestDaemon(t)

	beforePID, err := ReadPID(p)
	if err != nil {
		t.Fatalf("read live daemon pid before EnsureDaemon: %v", err)
	}
	beforeStart, err := daemonProcessStartTime(beforePID)
	if err != nil {
		t.Fatalf("read live daemon start time before EnsureDaemon: %v", err)
	}

	if err := EnsureDaemon(p); err != nil {
		t.Fatalf("EnsureDaemon on a live daemon: %v", err)
	}

	afterPID, err := ReadPID(p)
	if err != nil {
		t.Fatalf("read live daemon pid after EnsureDaemon: %v", err)
	}
	afterStart, err := daemonProcessStartTime(afterPID)
	if err != nil {
		t.Fatalf("read live daemon start time after EnsureDaemon: %v", err)
	}
	if afterPID != beforePID {
		t.Fatalf("daemon pid changed across EnsureDaemon: %d -> %d", beforePID, afterPID)
	}
	if !afterStart.Equal(beforeStart) {
		t.Fatalf("daemon start identity changed across EnsureDaemon: %v -> %v", beforeStart, afterStart)
	}
	if alive, err := daemonHealthCheck(p); err != nil || !alive {
		t.Fatalf("live daemon became unhealthy: alive=%v err=%v", alive, err)
	}
}
