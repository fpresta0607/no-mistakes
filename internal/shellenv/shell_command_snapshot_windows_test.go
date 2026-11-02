//go:build windows

package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func suspendedShellCommand(t *testing.T, isAssigned bool) (*exec.Cmd, *shellCommandJobState) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "cmd.exe", "/d", "/c", "exit", "0")
	ConfigureShellCommand(cmd)
	job, ok := shellCommandJob(cmd)
	if !ok {
		t.Fatal("disposable job setup failed")
	}
	t.Cleanup(func() {
		if err := terminateShellCommandJob(cmd, true); err != nil {
			t.Error(err)
		}
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if cmd.Process != nil {
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Error("owned suspended child was not reaped")
			}
			t.Logf("owned pid=%d reaped=1", cmd.Process.Pid)
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if isAssigned {
		if err := job.assign(uint32(cmd.Process.Pid)); err != nil {
			t.Fatal(err)
		}
	}
	return cmd, job
}

func restoreThreadCalls(t *testing.T) processThreadCalls {
	t.Helper()
	original := shellCommandThreadCalls
	t.Cleanup(func() { shellCommandThreadCalls = original })
	return original
}

func TestStartShellCommandPinsProcessThroughDiscovery(t *testing.T) {
	for _, isCanceled := range []bool{false, true} {
		name := "live_gc"
		if isCanceled {
			name = "canceled_before_open_gc"
		}
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "cmd.exe", "/d", "/c", "exit", "0")
			ConfigureShellCommand(cmd)
			t.Cleanup(func() {
				_ = terminateShellCommandJob(cmd, true)
				if cmd.Process != nil && cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			original := restoreThreadCalls(t)
			openCount := 0
			shellCommandThreadCalls.openProcess = func(mask uint32, isInherited bool, pid uint32) (windows.Handle, error) {
				openCount++
				if mask != windows.PROCESS_QUERY_INFORMATION|windows.SYNCHRONIZE || isInherited || pid != uint32(cmd.Process.Pid) {
					t.Errorf("acquisition mask=%x inherited=%t pid=%d", mask, isInherited, pid)
				}
				if isCanceled {
					if err := cmd.Cancel(); err != nil {
						return 0, err
					}
				}
				runtime.GC()
				handle, err := original.openProcess(mask, isInherited, pid)
				if err != nil {
					return 0, err
				}
				owner, err := windows.GetProcessId(handle)
				if err != nil || owner != pid {
					t.Errorf("acquired owner=%d error=%v want=%d", owner, err, pid)
				}
				if isCanceled {
					status, err := windows.WaitForSingleObject(handle, 2_000)
					if err != nil || status != windows.WAIT_OBJECT_0 {
						t.Errorf("canceled owned process exit=%d error=%v", status, err)
					}
				}
				runtime.GC()
				return handle, nil
			}
			err := StartShellCommand(cmd)
			if isCanceled {
				if !errors.Is(err, os.ErrProcessDone) || cmd.ProcessState == nil {
					t.Fatalf("canceled startup = %v, state=%v", err, cmd.ProcessState)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			if openCount != 1 {
				t.Errorf("process acquisitions=%d want=1", openCount)
			}
			t.Logf("owned pid=%d canceled=%t reaped=1", cmd.Process.Pid, isCanceled)
		})
	}
}

func TestStartShellCommandFailsClosedOnDiscoveryDenial(t *testing.T) {
	for _, phase := range []string{"open", "capture", "walk", "cancel_during_capture", "job_close_error"} {
		t.Run(phase, func(t *testing.T) {
			original := restoreThreadCalls(t)
			resumeCount := 0
			shellCommandThreadCalls.resume = func(thread windows.Handle) (uint32, error) {
				resumeCount++
				return original.resume(thread)
			}
			cmd := exec.CommandContext(context.Background(), "cmd.exe", "/d", "/c", "exit", "0")
			ConfigureShellCommand(cmd)
			closeErr := errors.New("owned job close error")
			if phase == "job_close_error" {
				originalClose := closeShellCommandHandleFunc
				closeShellCommandHandleFunc = func(handle windows.Handle) error {
					return errors.Join(originalClose(handle), closeErr)
				}
				t.Cleanup(func() { closeShellCommandHandleFunc = originalClose })
			}
			t.Cleanup(func() {
				_ = terminateShellCommandJob(cmd, true)
				if cmd.Process != nil && cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			switch phase {
			case "open":
				shellCommandThreadCalls.openProcess = func(uint32, bool, uint32) (windows.Handle, error) {
					return 0, windows.ERROR_ACCESS_DENIED
				}
			case "capture", "cancel_during_capture":
				shellCommandThreadCalls.capture = func(windows.Handle) (processSnapshot, error) {
					if phase == "cancel_during_capture" {
						if err := cmd.Cancel(); err != nil {
							t.Error(err)
						}
					}
					return 0, windows.ERROR_ACCESS_DENIED
				}
			case "walk", "job_close_error":
				shellCommandThreadCalls.walk = func(processSnapshot, processSnapshotMarker, *processSnapshotThread) error {
					return windows.ERROR_ACCESS_DENIED
				}
			}
			err := StartShellCommand(cmd)
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || resumeCount != 0 {
				t.Fatalf("startup=%v resumes=%d want denial and no resume", err, resumeCount)
			}
			if phase == "job_close_error" && !errors.Is(err, closeErr) {
				t.Errorf("owned cleanup error missing: %v", err)
			}
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatal("failed startup did not reap the original child")
			}
			if _, ok := shellCommandJob(cmd); ok {
				t.Fatal("failed startup retained the job")
			}
			if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("late cancellation=%v want process done", err)
			}
			t.Logf("owned pid=%d reaped=1 resumes=0", cmd.Process.Pid)
		})
	}
}

func TestResumeProcessThreadsValidatesOwnerAndPreservesCounts(t *testing.T) {
	for _, phase := range []string{"count_0", "count_1", "count_2", "wrong_owner", "wrong_entry", "vanished", "empty"} {
		t.Run(phase, func(t *testing.T) {
			cmd, _ := suspendedShellCommand(t, true)
			original := restoreThreadCalls(t)
			resumeCount := 0
			shellCommandThreadCalls.resume = func(thread windows.Handle) (uint32, error) {
				resumeCount++
				count, err := original.resume(thread)
				if strings.HasPrefix(phase, "count_") {
					count = uint32(phase[len(phase)-1] - '0')
				}
				return count, err
			}
			shellCommandThreadCalls.openThread = func(mask uint32, isInherited bool, tid uint32) (windows.Handle, error) {
				if mask != windows.THREAD_SUSPEND_RESUME|windows.THREAD_QUERY_LIMITED_INFORMATION || isInherited {
					t.Errorf("thread acquisition mask=%x inherited=%t", mask, isInherited)
				}
				if phase == "vanished" {
					return 0, windows.ERROR_INVALID_PARAMETER
				}
				return original.openThread(mask, isInherited, tid)
			}
			if phase == "wrong_owner" {
				shellCommandThreadCalls.threadOwner = func(windows.Handle) (uint32, error) {
					return uint32(cmd.Process.Pid) + 1, nil
				}
			}
			shellCommandThreadCalls.walk = func(snapshot processSnapshot, marker processSnapshotMarker, entry *processSnapshotThread) error {
				if phase == "empty" {
					return windows.ERROR_NO_MORE_ITEMS
				}
				err := original.walk(snapshot, marker, entry)
				if err == nil && phase == "wrong_entry" {
					entry.ProcessID++
				}
				return err
			}
			err := resumeProcessThreads(cmd.Process)
			if strings.HasPrefix(phase, "count_") {
				if err != nil || resumeCount == 0 {
					t.Fatalf("resume=%v count=%d", err, resumeCount)
				}
			} else if err == nil || resumeCount != 0 {
				t.Fatalf("invalid discovery error=%v resumes=%d", err, resumeCount)
			}
		})
	}
}

func TestResumeProcessThreadsReleasesPSSResources(t *testing.T) {
	for _, phase := range []string{"wait", "capture", "marker", "walk", "open_thread", "owner", "resume", "thread_close_error", "cleanup_errors", "success"} {
		t.Run(phase, func(t *testing.T) {
			cmd, _ := suspendedShellCommand(t, true)
			original := restoreThreadCalls(t)
			owned := make(map[windows.Handle]int)
			markerCount, snapshotCount := 0, 0
			acquisitions, threadCount := 0, 0
			primary := windows.ERROR_ACCESS_DENIED
			markerErr := errors.New("marker release error")
			snapshotErr := errors.New("snapshot release error")
			closeErr := errors.New("handle release error")
			shellCommandThreadCalls.openProcess = func(mask uint32, isInherited bool, pid uint32) (windows.Handle, error) {
				handle, err := original.openProcess(mask, isInherited, pid)
				if err == nil {
					owned[handle] = 0
					acquisitions++
				}
				return handle, err
			}
			shellCommandThreadCalls.capture = func(handle windows.Handle) (processSnapshot, error) {
				if phase == "capture" {
					return 0, primary
				}
				return original.capture(handle)
			}
			if phase == "wait" {
				shellCommandThreadCalls.waitProcess = func(windows.Handle, uint32) (uint32, error) {
					return 0, primary
				}
			}
			shellCommandThreadCalls.createMarker = func() (processSnapshotMarker, error) {
				if phase == "marker" {
					return 0, primary
				}
				return original.createMarker()
			}
			shellCommandThreadCalls.walk = func(snapshot processSnapshot, marker processSnapshotMarker, entry *processSnapshotThread) error {
				if phase == "walk" || phase == "cleanup_errors" {
					return primary
				}
				return original.walk(snapshot, marker, entry)
			}
			shellCommandThreadCalls.openThread = func(mask uint32, isInherited bool, tid uint32) (windows.Handle, error) {
				if phase == "open_thread" {
					return 0, primary
				}
				handle, err := original.openThread(mask, isInherited, tid)
				if err == nil {
					owned[handle] = 0
					threadCount++
				}
				return handle, err
			}
			shellCommandThreadCalls.threadOwner = func(handle windows.Handle) (uint32, error) {
				if phase == "owner" || phase == "thread_close_error" {
					return 0, primary
				}
				return original.threadOwner(handle)
			}
			shellCommandThreadCalls.resume = func(handle windows.Handle) (uint32, error) {
				if phase == "resume" {
					return 0, primary
				}
				return original.resume(handle)
			}
			shellCommandThreadCalls.closeHandle = func(handle windows.Handle) error {
				owned[handle]++
				err := original.closeHandle(handle)
				if phase == "cleanup_errors" || phase == "thread_close_error" {
					return errors.Join(err, closeErr)
				}
				return err
			}
			shellCommandThreadCalls.freeMarker = func(marker processSnapshotMarker) error {
				markerCount++
				err := original.freeMarker(marker)
				if phase == "cleanup_errors" {
					return errors.Join(err, markerErr)
				}
				return err
			}
			shellCommandThreadCalls.freeSnapshot = func(snapshot processSnapshot) error {
				snapshotCount++
				err := original.freeSnapshot(snapshot)
				if phase == "cleanup_errors" {
					return errors.Join(err, snapshotErr)
				}
				return err
			}
			err := resumeProcessThreads(cmd.Process)
			if phase == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, primary) {
				t.Fatalf("primary error missing: %v", err)
			}
			if phase == "cleanup_errors" && (!errors.Is(err, markerErr) || !errors.Is(err, snapshotErr) || !errors.Is(err, closeErr)) {
				t.Errorf("cleanup errors missing: %v", err)
			}
			if phase == "thread_close_error" && !errors.Is(err, closeErr) {
				t.Errorf("thread close error missing: %v", err)
			}
			wantSnapshot, wantMarker := 1, 1
			if phase == "wait" || phase == "capture" {
				wantSnapshot, wantMarker = 0, 0
			} else if phase == "marker" {
				wantMarker = 0
			}
			if acquisitions != 1 || snapshotCount != wantSnapshot || markerCount != wantMarker {
				t.Errorf("acquired=%d snapshot frees=%d marker frees=%d", acquisitions, snapshotCount, markerCount)
			}
			for handle, count := range owned {
				if count != 1 {
					t.Errorf("owned handle %d release attempts=%d", handle, count)
				}
			}
			t.Logf("owned process acquisitions=%d thread acquisitions=%d snapshot releases=%d marker releases=%d", acquisitions, threadCount, snapshotCount, markerCount)
		})
	}
}
