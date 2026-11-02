//go:build windows

package shellenv

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

const windowsTerminateGrace = 3 * time.Second

// defaultWaitDelay bounds Wait when failed cleanup leaves inherited handles
// open for commands that do not opt into cooperative cancellation.
const defaultWaitDelay = 5 * time.Second

type shellCommandJobState struct {
	mu                  sync.Mutex
	handle              windows.Handle
	cancelEvent         windows.Handle
	cancelEventName     string
	cooperative         bool
	isAssigned          bool
	isClosed            bool
	cooperativeDeadline time.Time
}

type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

var shellCommandJobs sync.Map
var shellCommandJobSetupErrors sync.Map

var newShellCommandJobFunc = newShellCommandJob
var assignShellCommandJobFunc = assignShellCommandJob
var resumeProcessThreadsFunc = resumeProcessThreads
var windowsCooperativeHelperCommandFunc = windowsCooperativeHelperCommand
var setShellCommandCancelEventFunc = windows.SetEvent
var terminateShellCommandJobFunc = windows.TerminateJobObject
var closeShellCommandHandleFunc = windows.CloseHandle
var queryShellCommandJobFunc = queryShellCommandJob

// ConfigureShellCommand prepares a Windows command for whole-tree cleanup on
// cancellation and normal exit. StartShellCommand assigns a kill-on-close job
// and fails if that guarantee is unavailable.
//
// Use RunShellCommand, OutputShellCommand, or CombinedOutputShellCommand for
// one-shot commands, or use StartShellCommand and defer
// TerminateShellCommandGroup immediately after a successful start when the
// caller needs manual pipe handling. If a parser reads stdout/stderr until EOF,
// the goroutine that owns Wait should terminate the group when the leader exits
// so inherited pipe holders cannot wedge the parser.
func ConfigureShellCommand(cmd *exec.Cmd) {
	configureWindowsShellCommand(cmd, false)
}

// ConfigureCooperativeShellCommand prepares a repository command that receives
// CTRL+BREAK and a bounded cleanup window before the usual forced tree kill.
func ConfigureCooperativeShellCommand(cmd *exec.Cmd) {
	configureWindowsShellCommand(cmd, true)
}

func configureWindowsShellCommand(cmd *exec.Cmd, cooperative bool) {
	// Suppress the visible console window Windows would otherwise allocate for
	// this console child (agents, cmd.exe shell steps) when spawned from the
	// console-less daemon. See issue #287. Harden allocates SysProcAttr if
	// needed; the creation flags are then adjusted for the requested topology.
	winproc.Harden(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cooperative {
		cmd.SysProcAttr.CreationFlags &^= windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_CONSOLE
	} else {
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	}
	var state *shellCommandJobState
	if job, err := newShellCommandJobFunc(); err == nil {
		state = &shellCommandJobState{handle: job, cooperative: cooperative}
		if cooperative {
			cancelEvent, cancelEventName, eventErr := newWindowsCooperativeCancelEvent()
			if eventErr != nil {
				state.isClosed = true
				shellCommandJobSetupErrors.Store(cmd, errors.Join(eventErr, closeShellCommandHandleFunc(job)))
			} else {
				state.cancelEvent = cancelEvent
				state.cancelEventName = cancelEventName
				shellCommandJobs.Store(cmd, state)
				cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
			}
		} else {
			shellCommandJobs.Store(cmd, state)
			cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
		}
	} else {
		shellCommandJobSetupErrors.Store(cmd, err)
	}

	// Install a WaitDelay backstop unless the caller has chosen one
	// explicitly (the short login-shell probe, for example, uses a tighter
	// bound of its own).
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = defaultWaitDelay
		if cooperative {
			cmd.WaitDelay = windowsTerminateGrace
		}
	}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if state == nil {
			return cmd.Process.Kill()
		}
		return state.cancel(cmd.Process)
	}
}

func (job *shellCommandJobState) cancel(process *os.Process) error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.isClosed {
		return os.ErrProcessDone
	}
	if !job.isAssigned {
		return process.Kill()
	}
	if job.cooperative && job.cancelEvent != 0 {
		if err := setShellCommandCancelEventFunc(job.cancelEvent); err == nil {
			job.cooperativeDeadline = time.Now().Add(windowsTerminateGrace)
			return nil
		}
	}
	return terminateShellCommandJobFunc(job.handle, 1)
}

// StartShellCommand starts cmd and, when configured, assigns its suspended
// process to the job before resuming threads from a process-local PSS snapshot.
// Job setup, assignment, or thread discovery failure aborts startup.
// If cmd.Start succeeded, failure kills and waits for the original child.
// The returned error preserves the startup cause alongside cleanup errors.
func StartShellCommand(cmd *exec.Cmd) error {
	if err, ok := takeShellCommandJobSetupError(cmd); ok {
		return fmt.Errorf("windows job object setup: %w", err)
	}
	job, isConfigured := shellCommandJob(cmd)
	if isConfigured && job.cooperative {
		if err := wrapWindowsCooperativeCommand(cmd, job.cancelEventName); err != nil {
			return errors.Join(err, closeShellCommandJob(cmd))
		}
	}
	if err := cmd.Start(); err != nil {
		return errors.Join(err, closeShellCommandJob(cmd))
	}
	if !isConfigured {
		return nil
	}
	if err := job.assign(uint32(cmd.Process.Pid)); err != nil {
		return failStartedShellCommand(cmd, fmt.Errorf("assign process to job object: %w", err))
	}
	if err := resumeProcessThreadsFunc(cmd.Process); err != nil {
		return failStartedShellCommand(cmd, err)
	}
	return nil
}

func (job *shellCommandJobState) assign(pid uint32) error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.isClosed {
		return os.ErrProcessDone
	}
	if err := assignShellCommandJobFunc(job.handle, pid); err != nil {
		return err
	}
	job.isAssigned = true
	return nil
}

// TerminateShellCommandGroup terminates the Windows job object for cmd. Callers
// defer it after a successful StartShellCommand so clean exits and ordinary
// errors get the same process-tree cleanup as context cancellation. After a
// cooperative cancellation it preserves the remaining grace window before
// forcing the job down, including when the console leader exits before a child
// finishes cleanup. Repeated calls after Wait or job removal are safe and never
// fall back to a PID-based kill. A nil, never-started, unconfigured, or already
// closed command is a no-op.
func TerminateShellCommandGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, ok := shellCommandJob(cmd)
	if !ok {
		return
	}
	job.mu.Lock()
	deadline := job.cooperativeDeadline
	job.mu.Unlock()
	if !deadline.IsZero() {
		waitForWindowsJobExit(job, time.Until(deadline))
	}
	_ = terminateShellCommandJob(cmd, true)
}

func waitForWindowsJobExit(job *shellCommandJobState, window time.Duration) {
	if window <= 0 {
		return
	}
	deadline := time.Now().Add(window)
	for {
		isActive, err := job.hasActiveProcesses()
		if err != nil || !isActive || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (job *shellCommandJobState) hasActiveProcesses() (bool, error) {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.isClosed {
		return false, nil
	}
	return queryShellCommandJobFunc(job.handle)
}

func queryShellCommandJob(job windows.Handle) (bool, error) {
	var info jobObjectBasicAccountingInformation
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return info.ActiveProcesses != 0, err
}

func newShellCommandJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ret, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if ret == 0 {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func newWindowsCooperativeCancelEvent() (windows.Handle, string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return 0, "", err
	}
	name := `Local\no-mistakes-cooperative-` + hex.EncodeToString(id[:])
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, "", err
	}
	event, err := windows.CreateEvent(nil, 1, 0, namePtr)
	if err != nil {
		return 0, "", err
	}
	return event, name, nil
}

func wrapWindowsCooperativeCommand(cmd *exec.Cmd, eventName string) error {
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return errors.New("invalid Windows cooperative command")
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		return errors.New("Windows cooperative commands do not support SysProcAttr.CmdLine")
	}
	path, args, err := windowsCooperativeHelperCommandFunc(eventName, cmd.Path, cmd.Args)
	if err != nil {
		return fmt.Errorf("prepare Windows cooperative command helper: %w", err)
	}
	cmd.Path = path
	cmd.Args = args
	return nil
}

func windowsCooperativeHelperCommand(eventName, targetPath string, targetArgs []string) (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	args := []string{exe, windowsCooperativeCommandArg + eventName, targetPath}
	args = append(args, targetArgs...)
	return exe, args, nil
}

func shellCommandJob(cmd *exec.Cmd) (*shellCommandJobState, bool) {
	if cmd == nil {
		return nil, false
	}
	value, ok := shellCommandJobs.Load(cmd)
	if !ok {
		return nil, false
	}
	job, ok := value.(*shellCommandJobState)
	return job, ok
}

func takeShellCommandJobSetupError(cmd *exec.Cmd) (error, bool) {
	if cmd == nil {
		return nil, false
	}
	value, ok := shellCommandJobSetupErrors.LoadAndDelete(cmd)
	if !ok {
		return nil, false
	}
	err, ok := value.(error)
	return err, ok
}

func closeShellCommandJob(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	value, ok := shellCommandJobs.LoadAndDelete(cmd)
	if !ok {
		return nil
	}
	job, ok := value.(*shellCommandJobState)
	if !ok {
		return nil
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.closeLocked()
}

func (job *shellCommandJobState) closeLocked() error {
	if job.isClosed {
		return nil
	}
	job.isClosed = true
	var eventErr error
	if job.cancelEvent != 0 {
		eventErr = closeShellCommandHandleFunc(job.cancelEvent)
	}
	return errors.Join(eventErr, closeShellCommandHandleFunc(job.handle))
}

func terminateShellCommandJob(cmd *exec.Cmd, closeJob bool) error {
	job, ok := shellCommandJob(cmd)
	if !ok {
		return nil
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.isClosed {
		return nil
	}
	var err error
	if job.isAssigned {
		err = terminateShellCommandJobFunc(job.handle, 1)
	}
	if closeJob {
		shellCommandJobs.CompareAndDelete(cmd, job)
		err = errors.Join(err, job.closeLocked())
	}
	return err
}

func assignShellCommandJob(job windows.Handle, pid uint32) (result error) {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, windows.CloseHandle(process)) }()
	return windows.AssignProcessToJobObject(job, process)
}

func failStartedShellCommand(cmd *exec.Cmd, cause error) error {
	cause = errors.Join(cause, terminateShellCommandJob(cmd, true))
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			cause = errors.Join(cause, err)
		}
		var exitErr *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exitErr) {
			cause = errors.Join(cause, err)
		}
	}
	return cause
}

// detachFromTerminal is a no-op where there is no POSIX session or controlling
// terminal for an interactive shell to contend for.
func detachFromTerminal(cmd *exec.Cmd) {}
