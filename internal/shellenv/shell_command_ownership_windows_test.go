//go:build windows

package shellenv

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func init() {
	if path := os.Getenv("NM_TASKKILL_CANARY"); path != "" && strings.EqualFold(filepath.Base(os.Args[0]), "taskkill.exe") {
		if os.WriteFile(path, []byte("1"), 0o600) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func TestShellCommandJobCloseSerializesBorrowers(t *testing.T) {
	for _, operation := range []string{"assignment", "query", "event", "termination"} {
		t.Run(operation, func(t *testing.T) {
			cmd, job := suspendedShellCommand(t, operation != "assignment")
			event, err := windows.CreateEvent(nil, 1, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			job.cancelEvent = event
			job.cooperative = true
			originalAssign := assignShellCommandJobFunc
			originalQuery := queryShellCommandJobFunc
			originalEvent := setShellCommandCancelEventFunc
			originalTerminate := terminateShellCommandJobFunc
			originalClose := closeShellCommandHandleFunc
			t.Cleanup(func() {
				assignShellCommandJobFunc = originalAssign
				queryShellCommandJobFunc = originalQuery
				setShellCommandCancelEventFunc = originalEvent
				terminateShellCommandJobFunc = originalTerminate
				closeShellCommandHandleFunc = originalClose
			})
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			calls := 0
			barrier := func() {
				calls++
				close(entered)
				<-release
			}
			var use func() error
			switch operation {
			case "assignment":
				assignShellCommandJobFunc = func(handle windows.Handle, pid uint32) error {
					barrier()
					return originalAssign(handle, pid)
				}
				use = func() error { return job.assign(uint32(cmd.Process.Pid)) }
			case "query":
				queryShellCommandJobFunc = func(handle windows.Handle) (bool, error) {
					barrier()
					return originalQuery(handle)
				}
				use = func() error { _, err := job.hasActiveProcesses(); return err }
			case "event":
				setShellCommandCancelEventFunc = func(handle windows.Handle) error {
					barrier()
					return originalEvent(handle)
				}
				use = func() error { return job.cancel(cmd.Process) }
			case "termination":
				terminateShellCommandJobFunc = func(handle windows.Handle, exitCode uint32) error {
					barrier()
					return originalTerminate(handle, exitCode)
				}
				use = func() error { return terminateShellCommandJob(cmd, false) }
			}
			closed := make(map[windows.Handle]int)
			closeShellCommandHandleFunc = func(handle windows.Handle) error {
				closed[handle]++
				return originalClose(handle)
			}
			used := make(chan error, 1)
			go func() { used <- use() }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("owned operation did not reach barrier")
			}
			if job.mu.TryLock() {
				job.mu.Unlock()
				t.Fatal("handle API was called without holding the close mutex")
			}
			closeStarted := make(chan struct{})
			closeDone := make(chan error, 1)
			go func() { close(closeStarted); closeDone <- closeShellCommandJob(cmd) }()
			<-closeStarted
			select {
			case err := <-closeDone:
				t.Fatalf("close passed the active handle-use barrier: %v", err)
			default:
			}
			releaseOnce.Do(func() { close(release) })
			waitForOwnershipOperation(t, used)
			waitForOwnershipOperation(t, closeDone)
			if err := closeShellCommandJob(cmd); err != nil {
				t.Error(err)
			}
			if err := job.assign(uint32(cmd.Process.Pid)); !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("stale assignment=%v", err)
			}
			if err := job.cancel(cmd.Process); !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("stale cancellation=%v", err)
			}
			if isActive, err := job.hasActiveProcesses(); isActive || err != nil {
				t.Errorf("closed query=%t, %v", isActive, err)
			}
			if err := terminateShellCommandJob(cmd, false); err != nil {
				t.Error(err)
			}
			if calls != 1 || closed[job.handle] != 1 || closed[event] != 1 || len(closed) != 2 {
				t.Fatalf("API calls=%d physical closes=%v", calls, closed)
			}
			t.Log("job releases=1 event releases=1 stale API calls=0")
		})
	}
	t.Run("close_and_cancel_while_discovery_is_blocked", func(t *testing.T) {
		restoreThreadCalls(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		shellCommandThreadCalls.capture = func(windows.Handle) (processSnapshot, error) {
			close(entered)
			<-release
			return 0, windows.ERROR_ACCESS_DENIED
		}
		cmd := exec.CommandContext(context.Background(), "cmd.exe", "/d", "/c", "exit", "0")
		ConfigureShellCommand(cmd)
		done := make(chan error, 1)
		isFinished := false
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			if !isFinished {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("startup did not finish during owned cleanup")
					return
				}
			}
			_ = terminateShellCommandJob(cmd, true)
			if cmd.Process != nil && cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		go func() { done <- StartShellCommand(cmd) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("discovery did not reach barrier")
		}
		closed := make(chan error, 1)
		go func() { closed <- closeShellCommandJob(cmd) }()
		waitForOwnershipOperation(t, closed)
		if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("late Cancel while discovery active=%v", err)
		}
		releaseOnce.Do(func() { close(release) })
		select {
		case err := <-done:
			isFinished = true
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Errorf("startup=%v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("startup did not unwind")
		}
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			t.Fatal("original child not reaped")
		}
		if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("post-Wait cancellation=%v", err)
		}
		t.Logf("owned pid=%d reaped=1 close completed during blocked PSS=1", cmd.Process.Pid)
	})
}

func waitForOwnershipOperation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned handle operation did not finish")
	}
}

func TestConfiguredShellCommandTeardownAfterWaitKeepsIdentity(t *testing.T) {
	directory := t.TempDir()
	executable, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	canary, err := os.Create(filepath.Join(directory, "taskkill.exe"))
	if err != nil {
		_ = executable.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(canary, executable)
	if err := errors.Join(copyErr, canary.Close(), executable.Close()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, phase := range []string{"after_wait", "startup_failure_then_cancel"} {
		t.Run(phase, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "pid-request")
			t.Setenv("NM_TASKKILL_CANARY", marker)
			cmd := exec.CommandContext(context.Background(), "cmd.exe", "/d", "/c", "exit", "0")
			ConfigureShellCommand(cmd)
			t.Cleanup(func() {
				if cmd.Process != nil && cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				closeShellCommandJob(cmd)
			})
			var pinned windows.Handle
			assignment := assignShellCommandJobFunc
			assignShellCommandJobFunc = func(job windows.Handle, pid uint32) error {
				var err error
				pinned, err = windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
				if err != nil {
					return err
				}
				if phase == "startup_failure_then_cancel" {
					return windows.ERROR_ACCESS_DENIED
				}
				return assignment(job, pid)
			}
			t.Cleanup(func() {
				assignShellCommandJobFunc = assignment
				if pinned != 0 {
					if err := windows.CloseHandle(pinned); err != nil {
						t.Error(err)
					}
				}
			})
			err := StartShellCommand(cmd)
			if phase == "startup_failure_then_cancel" {
				if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Fatalf("startup error = %v, want assignment denial", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				TerminateShellCommandGroup(cmd)
				TerminateShellCommandGroup(cmd)
			}
			if err := cmd.Cancel(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("late Cancel = %v", err)
			}
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatal("original child was not reaped")
			}
			if _, ok := shellCommandJob(cmd); ok {
				t.Error("closed command retained a job-map entry")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("configured cleanup issued an unpinned PID request after Wait: %v", err)
			}
			status, err := windows.WaitForSingleObject(pinned, 0)
			if err != nil || status != windows.WAIT_OBJECT_0 {
				t.Fatalf("owned child exit = %d, %v", status, err)
			}
			t.Logf("owned pid=%d reaped=1 exit_observed=1", cmd.Process.Pid)
		})
	}
	t.Run("pipe_error_after_wait_closed_job", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "pid-request")
		t.Setenv("NM_TASKKILL_CANARY", marker)
		event, eventName, err := newWindowsCooperativeCancelEvent()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = windows.SetEvent(event); _ = windows.CloseHandle(event) })
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsOwnedPipeHelper$")
		cmd.Env = append(os.Environ(), "NM_OWNED_PIPE_EVENT="+eventName)
		ConfigureShellCommand(cmd)
		job, ok := shellCommandJob(cmd)
		if !ok {
			t.Fatal("disposable job setup failed")
		}
		entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		var pinned windows.Handle
		copyErr := errors.New("owned output copy failure")
		cmd.Stdout = ownershipPipeWriter(func([]byte) (int, error) {
			var err error
			pinned, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
			close(entered)
			<-release
			return 0, errors.Join(copyErr, err)
		})
		originalClose := closeShellCommandHandleFunc
		closeShellCommandHandleFunc = func(handle windows.Handle) error {
			err := originalClose(handle)
			if handle == job.handle {
				close(closed)
			}
			return err
		}
		done := make(chan error, 1)
		isFinished := false
		t.Cleanup(func() {
			cancel()
			releaseOnce.Do(func() { close(release) })
			if !isFinished {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("pipe command did not finish during owned cleanup")
				}
			}
			closeShellCommandHandleFunc = originalClose
			if pinned != 0 {
				if err := windows.CloseHandle(pinned); err != nil {
					t.Error(err)
				}
			}
		})
		go func() { done <- RunShellCommand(cmd) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("owned writer did not reach barrier")
		}
		cancel()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("Wait path did not close job while output copy was blocked")
		}
		releaseOnce.Do(func() { close(release) })
		select {
		case err := <-done:
			isFinished = true
			if err == nil {
				t.Error("canceled command with failed output unexpectedly passed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("pipe-error path did not finish")
		}
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			t.Fatal("pipe command not reaped")
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pipe-error cleanup issued a PID request after Wait: %v", err)
		}
		status, err := windows.WaitForSingleObject(pinned, 0)
		if err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatalf("owned pipe child exit=%d error=%v", status, err)
		}
		t.Logf("owned pid=%d reaped=1 pipe error after job close=1", cmd.Process.Pid)
	})
}

type ownershipPipeWriter func([]byte) (int, error)

func (write ownershipPipeWriter) Write(data []byte) (int, error) { return write(data) }

func TestWindowsOwnedPipeHelper(t *testing.T) {
	name := os.Getenv("NM_OWNED_PIPE_EVENT")
	if name == "" {
		return
	}
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, namePointer)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	if _, err := os.Stdout.WriteString("1"); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.WaitForSingleObject(event, 10_000); err != nil {
		t.Fatal(err)
	}
}
