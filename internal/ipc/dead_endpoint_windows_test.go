//go:build windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func deadOwnerPID(t *testing.T, exitCode int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/c", "set /p ready= & exit /b "+strconv.Itoa(exitCode))
	winproc.Harden(cmd)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("retain helper process %d: %v", pid, err)
	}
	t.Cleanup(func() { windows.CloseHandle(handle) })
	if _, err := input.Write([]byte("ready\n")); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	err = cmd.Wait()
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || cmd.ProcessState.ExitCode() != exitCode {
		t.Fatalf("helper exit = %v, state = %v, want exit code %d", err, cmd.ProcessState, exitCode)
	}
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("retained helper wait = %d, %v, want signaled", state, err)
	}
	reopened, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("exited helper %d is not openable: %v", pid, err)
	}
	windows.CloseHandle(reopened)
	t.Logf("retained exited helper pid=%d exit=%d remains openable", pid, exitCode)
	return pid
}

func TestDialClassifiesDeadEndpointOwner(t *testing.T) {
	for _, exitCode := range []int{0, 1, 259} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			endpoint := filepath.Join(t.TempDir(), "socket")
			pid := deadOwnerPID(t, exitCode)
			content := fmt.Sprintf("127.0.0.1:1\n0123456789abcdef0123456789abcdef\n%d", pid)
			if err := os.WriteFile(endpoint, []byte(content), 0o600); err != nil {
				t.Fatalf("write endpoint file: %v", err)
			}

			client, err := Dial(endpoint)
			if client != nil {
				client.Close()
				t.Fatal("Dial returned a client for a dead endpoint owner")
			}
			if !errors.Is(err, ErrEndpointProcessDead) {
				t.Fatalf("Dial error = %v, want ErrEndpointProcessDead", err)
			}
			var deadOwner *DeadEndpointError
			if !errors.As(err, &deadOwner) || deadOwner.PID != pid {
				t.Fatalf("Dial error = %v, want dead owner pid %d", err, pid)
			}
			if want := fmt.Sprintf("dial ipc: daemon process %d is no longer running", pid); err.Error() != want {
				t.Fatalf("Dial error = %q, want %q", err, want)
			}
			if processAlive(pid) {
				t.Fatalf("retained exited owner %d classified alive", pid)
			}
		})
	}
}

func TestProcessAliveCurrentOwner(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("current endpoint owner classified dead")
	}
}

func TestProcessAliveRejectsInvalidPID(t *testing.T) {
	nonexistentPID := uint32(math.MaxUint32)
	pids := []int{-1, 0, int(nonexistentPID)}
	if strconv.IntSize > 32 {
		// Truncating this PID to a DWORD would alias the live test process.
		overflowPID := int64(math.MaxUint32) + 1 + int64(os.Getpid())
		pids = append(pids, int(overflowPID))
	}
	for _, pid := range pids {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			if processAlive(pid) {
				t.Fatalf("invalid pid %d classified alive", pid)
			}
		})
	}
}

func TestDialProbeFailureDoesNotClassifyOwnerDead(t *testing.T) {
	tests := []struct {
		name           string
		openErr        error
		shouldDenyWait bool
	}{
		{name: "access denied", openErr: windows.ERROR_ACCESS_DENIED},
		{name: "other open failure", openErr: windows.ERROR_NOT_ENOUGH_MEMORY},
		{name: "wait failure", shouldDenyWait: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalOpen := openEndpointProcess
			originalDial := dialNetworkWithTimeout
			t.Cleanup(func() {
				openEndpointProcess = originalOpen
				dialNetworkWithTimeout = originalDial
			})
			var opened windows.Handle
			openEndpointProcess = func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
				if test.openErr != nil {
					return 0, test.openErr
				}
				if test.shouldDenyWait {
					access = windows.PROCESS_QUERY_LIMITED_INFORMATION
				}
				var err error
				opened, err = originalOpen(access, inherit, pid)
				if err == nil && test.shouldDenyWait {
					state, waitErr := windows.WaitForSingleObject(opened, 0)
					if state != windows.WAIT_FAILED || !errors.Is(waitErr, windows.ERROR_ACCESS_DENIED) {
						windows.CloseHandle(opened)
						t.Fatalf("wait without SYNCHRONIZE = %d, %v, want access denied", state, waitErr)
					}
				}
				return opened, err
			}
			connectErr := errors.New("connection unavailable")
			dialNetworkWithTimeout = func(string, string, time.Duration) (net.Conn, error) {
				return nil, connectErr
			}
			endpoint := filepath.Join(t.TempDir(), "socket")
			content := fmt.Sprintf("127.0.0.1:1\n0123456789abcdef0123456789abcdef\n%d", os.Getpid())
			if err := os.WriteFile(endpoint, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			client, err := Dial(endpoint)

			if client != nil {
				client.Close()
				t.Fatal("Dial returned a client after connection failure")
			}
			if errors.Is(err, ErrEndpointProcessDead) || !errors.Is(err, connectErr) {
				t.Fatalf("Dial error = %v, want connection failure without dead-owner classification", err)
			}
			if opened != 0 {
				if _, err := windows.WaitForSingleObject(opened, 0); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
					t.Fatalf("probe handle remains open: %v", err)
				}
			}
			data, err := os.ReadFile(endpoint)
			if err != nil || string(data) != content {
				t.Fatalf("probe failure changed endpoint: %q, %v", data, err)
			}
		})
	}
}
