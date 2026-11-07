package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonStartupProgressKeepsSlowRecoveryAlive(t *testing.T) {
	for _, isManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%t", isManaged), func(t *testing.T) {
			p, started, elapsed := startupProgressClock(t)
			daemonHealthCheck = func(*paths.Paths) (bool, error) {
				writeStartupProgressFixture(t, p, 4242, started, 3, int(elapsed().Seconds())/20)
				return elapsed() >= 70*time.Second, nil
			}
			pid := 4242
			if isManaged {
				pid = 0
			}

			err := waitForDaemonStart(p, pid, started)

			if err != nil {
				t.Fatalf("slow recovery with completed work failed after %v: %v", elapsed(), err)
			}
			if elapsed() != 70*time.Second {
				t.Fatalf("readiness elapsed = %v, want 70s", elapsed())
			}
		})
	}
}

func startupProgressClock(t *testing.T) (*paths.Paths, time.Time, func() time.Duration) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	started := time.Now().UTC()
	now := started
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "45s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "5s")
	oldNow, oldSleep := daemonStartupNow, daemonStartupSleep
	oldHealth, oldStart := daemonHealthCheck, daemonProcessStartTime
	oldRunning, oldKill := daemonProcessRunning, daemonKillPID
	oldInspect := inspectManagedDaemonService
	t.Cleanup(func() {
		daemonStartupNow, daemonStartupSleep = oldNow, oldSleep
		daemonHealthCheck, daemonProcessStartTime = oldHealth, oldStart
		daemonProcessRunning, daemonKillPID = oldRunning, oldKill
		inspectManagedDaemonService = oldInspect
	})
	daemonStartupNow = func() time.Time { return now }
	daemonStartupSleep = func(duration time.Duration) { now = now.Add(duration) }
	daemonProcessStartTime = func(pid int) (time.Time, error) {
		if pid != 4242 {
			return time.Time{}, fmt.Errorf("unrelated pid %d", pid)
		}
		return started, nil
	}
	isRunning := true
	daemonProcessRunning = func(int) (bool, error) { return isRunning, nil }
	daemonKillPID = func(pid int) error {
		isRunning = false
		t.Errorf("unexpected kill of pid %d", pid)
		return nil
	}
	inspectManagedDaemonService = func(*paths.Paths, managedServiceLaunch) (managedServiceState, error) {
		return managedServiceRunning, nil
	}
	return p, started, func() time.Duration { return now.Sub(started) }
}

func writeStartupProgressFixture(t *testing.T, p *paths.Paths, pid int, started time.Time, total, completed int) {
	t.Helper()
	data, err := json.Marshal(daemonPIDFile{
		PID: pid, StartedAt: started,
		Startup: &startupWork{Snapshot: "fixture", Total: total, Completed: completed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonStartupProgressWithoutCompletedWorkTimesOut(t *testing.T) {
	p, started, elapsed := startupProgressClock(t)
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		writeStartupProgressFixture(t, p, 4242, started, 3, 0)
		return false, nil
	}
	killed := 0
	daemonKillPID = func(pid int) error { killed = pid; return nil }
	daemonProcessRunning = func(int) (bool, error) { return false, nil }

	err := waitForDaemonStart(p, 4242, started)

	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("stuck startup error = %v", err)
	}
	if elapsed() != 45*time.Second || killed != 4242 {
		t.Fatalf("stuck startup elapsed = %v killed = %d; want 45s and exact child", elapsed(), killed)
	}
}

func TestDaemonStartupProgressRejectsInvalidOrRepeatedEvidence(t *testing.T) {
	for _, test := range []struct {
		name         string
		pid          int
		startOffset  time.Duration
		total        int
		completed    int
		isUnreadable bool
		wantElapsed  time.Duration
	}{
		{name: "other pid", pid: 5151, total: 3, completed: 1, wantElapsed: 45 * time.Second},
		{name: "stale generation", pid: 4242, startOffset: -time.Hour, total: 3, completed: 1, wantElapsed: 45 * time.Second},
		{name: "changed snapshot", pid: 4242, total: 4, completed: 1, wantElapsed: 45 * time.Second},
		{name: "beyond snapshot", pid: 4242, total: 3, completed: 4, wantElapsed: 45 * time.Second},
		{name: "negative completion", pid: 4242, total: 3, completed: -1, wantElapsed: 45 * time.Second},
		{name: "empty snapshot", pid: 4242, total: 0, completed: 0, wantElapsed: 45 * time.Second},
		{name: "unreadable", isUnreadable: true, wantElapsed: 45 * time.Second},
		{name: "repeated completion", pid: 4242, total: 3, completed: 1, wantElapsed: 55 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, started, elapsed := startupProgressClock(t)
			daemonHealthCheck = func(*paths.Paths) (bool, error) {
				if elapsed() < 10*time.Second {
					writeStartupProgressFixture(t, p, 4242, started, 3, 0)
				} else if test.isUnreadable {
					if err := os.WriteFile(p.PIDFile(), []byte("incomplete publication"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					writeStartupProgressFixture(t, p, test.pid, started.Add(test.startOffset), test.total, test.completed)
				}
				return false, nil
			}
			killed := 0
			daemonKillPID = func(pid int) error { killed = pid; return nil }
			daemonProcessRunning = func(int) (bool, error) { return false, nil }

			err := waitForDaemonStart(p, 4242, started)

			if err == nil || elapsed() != test.wantElapsed || killed != 4242 {
				t.Fatalf("startup elapsed=%v killed=%d error=%v; want %v and only the owned child", elapsed(), killed, err, test.wantElapsed)
			}
		})
	}
}

func TestDaemonStartupProgressExhaustsItsFiniteSnapshot(t *testing.T) {
	p, started, elapsed := startupProgressClock(t)
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		writeStartupProgressFixture(t, p, 4242, started, 2, int(elapsed().Seconds())/10)
		return false, nil
	}
	daemonKillPID = func(int) error { return nil }
	daemonProcessRunning = func(int) (bool, error) { return false, nil }

	err := waitForDaemonStart(p, 4242, started)

	if err == nil || elapsed() != 65*time.Second {
		t.Fatalf("exhausted snapshot elapsed=%v error=%v; want timeout at 65s", elapsed(), err)
	}
}

func TestDaemonStartupProgressHasAnAbsoluteElapsedLimit(t *testing.T) {
	p, started, elapsed := startupProgressClock(t)
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		writeStartupProgressFixture(t, p, 4242, started, 1000000, int(elapsed().Seconds())/10)
		return false, nil
	}
	daemonKillPID = func(int) error { return nil }
	daemonProcessRunning = func(int) (bool, error) { return false, nil }

	err := waitForDaemonStart(p, 4242, started)

	if err == nil || elapsed() != 10*time.Minute {
		t.Fatalf("continuous progress elapsed=%v error=%v; want timeout at 10m", elapsed(), err)
	}
}

func TestDaemonStartupProgressDoesNotHideAnEarlyChildExit(t *testing.T) {
	for _, isManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%t", isManaged), func(t *testing.T) {
			p, started, elapsed := startupProgressClock(t)
			exited := make(chan error, 1)
			daemonHealthCheck = func(*paths.Paths) (bool, error) {
				writeStartupProgressFixture(t, p, 4242, started, 10, int(elapsed().Seconds())/5)
				if elapsed() == 20*time.Second && exited != nil {
					exited <- fmt.Errorf("exit status 23")
				}
				return false, nil
			}
			daemonProcessRunning = func(int) (bool, error) { return elapsed() < 20*time.Second, nil }
			pid := 4242
			if isManaged {
				pid = 0
				exited = nil
			}

			err := waitForDaemonStartWithProcess(p, nil, exited, pid, started, managedServiceLaunch{})

			if err == nil || !strings.Contains(err.Error(), "exited before readiness") || elapsed() != 20*time.Second {
				t.Fatalf("early exit elapsed=%v error=%v; want failure at 20s", elapsed(), err)
			}
		})
	}
}

func TestDaemonStartupProgressPreservesAReusedPID(t *testing.T) {
	p, started, elapsed := startupProgressClock(t)
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		writeStartupProgressFixture(t, p, 4242, started, 3, 0)
		return false, nil
	}
	daemonProcessStartTime = func(int) (time.Time, error) {
		if elapsed() >= 45*time.Second {
			return started.Add(time.Second), nil
		}
		return started, nil
	}

	err := waitForDaemonStart(p, 4242, started)

	if err == nil || !strings.Contains(err.Error(), "no longer matches original process") {
		t.Fatalf("PID reuse error=%v; want refusal without killing the replacement", err)
	}
}

func TestStartupProgressCreditsOnlyDistinctSnapshotWork(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	record := daemonPIDFile{PID: 4242, StartedAt: time.Now().UTC()}
	progress := newStartupProgress(p.PIDFile(), record, []*db.Run{{ID: "first"}, {ID: "second"}})

	for _, work := range []string{"parked_runs", "parked_runs", "run:first", "run:first", "run:later", "unknown"} {
		progress.complete(work)
	}

	actual, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		t.Fatal(err)
	}
	if actual.PID != record.PID || !actual.StartedAt.Equal(record.StartedAt) || actual.Startup.Total != 11 || actual.Startup.Completed != 2 {
		t.Fatalf("startup snapshot=%+v identity=%+v; want 2 of 11 distinct completed items in the original generation", actual.Startup, actual)
	}
}

func TestStartupProgressCreditsEachCompletedRecoveryCheck(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repository, err := database.InsertRepoWithID("fixture", t.TempDir(), "https://example.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"first", "second"} {
		if _, err := database.InsertRun(repository.ID, branch, strings.Repeat("a", 40), strings.Repeat("a", 40)); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := database.GetActiveRuns()
	if err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, nil)
	manager.startup = newStartupProgress(p.PIDFile(), daemonPIDFile{PID: 4242, StartedAt: time.Now().UTC()}, runs)

	plans, _ := manager.recoverableParkedRuns(t.Context())
	actual, err := readDaemonPIDFile(p.PIDFile())

	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 || actual.Startup.Completed != 2 || actual.Startup.Total != 11 {
		t.Fatalf("completed checks=%+v plans=%d; want both completed refusals credited once", actual.Startup, len(plans))
	}
}

func TestRunWithOptionsFinishesStartupProgressBeforeIPCReadiness(t *testing.T) {
	p, _ := startTestDaemon(t)

	record, err := readDaemonPIDFile(p.PIDFile())

	if err != nil {
		t.Fatal(err)
	}
	if record.Startup == nil || record.Startup.Total != 9 || record.Startup.Completed != record.Startup.Total {
		t.Fatalf("startup at real IPC readiness=%+v; want all nine phases completed", record.Startup)
	}
}

func TestDaemonStartupProgressRejectsAnOldManagedChild(t *testing.T) {
	p, started, elapsed := startupProgressClock(t)
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		writeStartupProgressFixture(t, p, 4242, started, 10, int(elapsed().Seconds())/5)
		return false, nil
	}

	err := waitForDaemonStartWithProcess(p, nil, nil, 0, time.Time{}, managedServiceLaunch{notBefore: started.Add(time.Second)})

	if err == nil || elapsed() != 45*time.Second {
		t.Fatalf("old managed generation elapsed=%v error=%v; want no extension", elapsed(), err)
	}
}

func TestDaemonStartupProgressManagedLaunchRespectsProcessTimePrecision(t *testing.T) {
	for _, test := range []struct {
		name         string
		platform     string
		startOffset  time.Duration
		recordOffset time.Duration
		wantElapsed  time.Duration
	}{
		{name: "linux same second", platform: "linux", wantElapsed: 70 * time.Second},
		{name: "darwin same second", platform: "darwin", wantElapsed: 70 * time.Second},
		{name: "linux previous second", platform: "linux", startOffset: -time.Second, wantElapsed: 45 * time.Second},
		{name: "darwin previous second", platform: "darwin", startOffset: -time.Second, wantElapsed: 45 * time.Second},
		{name: "linux mismatched identity", platform: "linux", recordOffset: time.Second, wantElapsed: 45 * time.Second},
		{name: "windows earlier same second", platform: "windows", startOffset: 100 * time.Millisecond, wantElapsed: 45 * time.Second},
		{name: "windows later same second", platform: "windows", startOffset: 400 * time.Millisecond, wantElapsed: 70 * time.Second},
		{name: "windows mismatched identity", platform: "windows", startOffset: 400 * time.Millisecond, recordOffset: 100 * time.Nanosecond, wantElapsed: 45 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, started, elapsed := startupProgressClock(t)
			oldPlatform := runtimeGOOS
			runtimeGOOS = test.platform
			t.Cleanup(func() { runtimeGOOS = oldPlatform })
			launchStarted := started.Truncate(time.Second).Add(250 * time.Millisecond)
			processStarted := started.Truncate(time.Second).Add(test.startOffset)
			daemonProcessStartTime = func(int) (time.Time, error) { return processStarted, nil }
			daemonHealthCheck = func(*paths.Paths) (bool, error) {
				writeStartupProgressFixture(t, p, 4242, processStarted.Add(test.recordOffset), 3, int(elapsed().Seconds())/20)
				return elapsed() >= 70*time.Second, nil
			}

			err := waitForDaemonStartWithProcess(p, nil, nil, 0, time.Time{}, managedServiceLaunch{notBefore: launchStarted})

			if elapsed() != test.wantElapsed {
				t.Fatalf("managed readiness elapsed=%v error=%v; want %v", elapsed(), err, test.wantElapsed)
			}
			if test.wantElapsed == 70*time.Second {
				if err != nil {
					t.Fatalf("valid managed progress failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "did not become ready") {
				t.Fatalf("invalid managed progress error=%v; want readiness timeout", err)
			}
		})
	}
}

func TestDaemonStartupProgressRejectsAReplacementSnapshotOfTheSameSize(t *testing.T) {
	p, started, _ := startupProgressClock(t)
	wait := startupWait{timeout: 45 * time.Second, deadline: started.Add(45 * time.Second), hardDeadline: started.Add(10 * time.Minute), pid: 4242, startedAt: started}
	writeStartupProgressFixture(t, p, 4242, started, 3, 0)
	wait.observe(p, started)
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		t.Fatal(err)
	}
	record.Startup.Snapshot = "replacement"
	record.Startup.Completed = 1
	if err := writeDaemonPIDFile(p.PIDFile(), record); err != nil {
		t.Fatal(err)
	}

	wait.observe(p, started.Add(20*time.Second))

	if wait.completed != 0 || !wait.deadline.Equal(started.Add(45*time.Second)) {
		t.Fatalf("replacement snapshot changed the wait: %+v", wait)
	}
}
