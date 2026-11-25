package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ownedChildrenSettleWindow bounds how long the owned-children probe re-checks
// an exited agent's process group or job before refusing its result (issue
// #1614). A member that was merely finishing when the leader exited - a test
// runner's last worker, a toolchain helper mid-shutdown - leaves inside this
// window, while a genuinely surviving child is still refused and reaped by the
// deferred group terminate.
const ownedChildrenSettleWindow = 2 * time.Second

// ErrCompletionUnverified marks a RequireCompletedChildren turn whose result
// does not count: work the agent started was still running when it ended, or
// the probe could not tell. The group is stopped after the probe either way.
var ErrCompletionUnverified = errors.New("native check completion is unverified")

type nativeAgentCommand struct {
	cmd            *exec.Cmd
	stdout         *nativeAgentPipe
	stderr         *nativeAgentPipe
	waitCh         chan error
	terminateOnce  sync.Once
	closePipesOnce sync.Once
	waitOnce       sync.Once
	waitErr        error
	pipeMu         sync.Mutex
	remainingPipes int
	pipesDone      chan struct{}
	// isLeaderStopped records that stopLeader ended the agent process itself,
	// so its exit is judged by what it left running, like a clean exit.
	isLeaderStopped atomic.Bool
	// cliProcessesMu guards cliProcesses and isRecordClosed.
	cliProcessesMu sync.Mutex
	// cliProcesses are the agent CLI's own processes beside the leader, from
	// recordCLIProcesses; stopLeader stops them with the leader.
	cliProcesses *shellenv.ShellCommandMembers
	// isRecordClosed records that a record was attempted, whether or not it
	// succeeded, or that the command was reaped, so nothing is recorded any
	// more.
	isRecordClosed bool
}

func writeNativeAgentStdin(stdin io.WriteCloser, prompt string) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(stdin, prompt)
		closeErr := stdin.Close()
		errCh <- errors.Join(writeErr, closeErr)
	}()
	return errCh
}

type nativeAgentPipe struct {
	file     *os.File
	done     func()
	doneOnce sync.Once
	// activity, when set, is called for every non-empty read. It is the only
	// evidence no-mistakes has that a native agent is still alive during the
	// long tool-using stretches that produce no assistant prose.
	activity func()
}

func (p *nativeAgentPipe) Read(b []byte) (int, error) {
	n, err := p.file.Read(b)
	if n > 0 && p.activity != nil {
		p.activity()
	}
	if err != nil {
		p.markDone()
	}
	return n, err
}

func (p *nativeAgentPipe) Close() error {
	err := p.file.Close()
	p.markDone()
	return err
}

func (p *nativeAgentPipe) markDone() {
	p.doneOnce.Do(p.done)
}

// startNativeAgentCommand starts cmd with dedicated stdout/stderr pipes, on
// desktop when it names one (RunOpts.Desktop).
// activity, when non-nil, is invoked on every non-empty read from either pipe;
// see LifecyclePhaseActivity for why subprocess byte liveness - not assistant
// prose - is the signal that distinguishes a working agent from a wedged one.
func startNativeAgentCommand(cmd *exec.Cmd, desktop string, activity func(), requireCompletedChildren bool) (*nativeAgentCommand, error) {
	if err := shellenv.PlaceOnDesktop(cmd, desktop); err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	oomBase, oomOK := shellenv.OOMKillBaseline()
	if err := shellenv.StartShellCommand(cmd); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	started := &nativeAgentCommand{
		cmd:            cmd,
		waitCh:         make(chan error, 1),
		remainingPipes: 2,
		pipesDone:      make(chan struct{}),
	}
	started.stdout = &nativeAgentPipe{file: stdoutR, done: started.markPipeDone, activity: activity}
	started.stderr = &nativeAgentPipe{file: stderrR, done: started.markPipeDone, activity: activity}
	go func() {
		err := shellenv.AttributeOOMKill(oomBase, oomOK, cmd.Wait())
		if requireCompletedChildren && (err == nil || started.isLeaderStopped.Load()) {
			err = nil
			active, members, probeErr := shellenv.ShellCommandGroupActive(cmd, ownedChildrenSettleWindow)
			if probeErr != nil {
				err = fmt.Errorf("%w: %w", ErrCompletionUnverified, probeErr)
				if members != "" {
					err = fmt.Errorf("%w; last observed %s", err, members)
				}
			} else if active {
				msg := "agent exited with active owned children"
				if members != "" {
					msg += ": " + members
				}
				err = fmt.Errorf("%w: %s", ErrCompletionUnverified, msg)
			}
		}
		started.terminate()
		started.releaseCLIProcesses()
		started.waitCh <- started.waitForPipes(err)
	}()
	return started, nil
}

func (c *nativeAgentCommand) markPipeDone() {
	c.pipeMu.Lock()
	defer c.pipeMu.Unlock()
	c.remainingPipes--
	if c.remainingPipes == 0 {
		close(c.pipesDone)
	}
}

func (c *nativeAgentCommand) pid() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *nativeAgentCommand) waitForPipes(waitErr error) error {
	if c.cmd.WaitDelay <= 0 {
		<-c.pipesDone
		return waitErr
	}
	timer := time.NewTimer(c.cmd.WaitDelay)
	defer timer.Stop()
	select {
	case <-c.pipesDone:
		return waitErr
	case <-timer.C:
		c.closePipes()
		if waitErr == nil {
			return exec.ErrWaitDelay
		}
		return waitErr
	}
}

// recordCLIProcesses records every process the command's group or job held
// besides the leader before reportedAt as the agent CLI's own (issue #277): the
// real CLI under an npm shim or the Windows desktop helper, and the stdio MCP
// servers it started. reportedAt is when the agent's report that it has
// started was read, before it can have started any work, so nothing recorded
// can be that work however long the record itself takes. Only the first call
// records; after a failed one the leader is stopped alone.
func (c *nativeAgentCommand) recordCLIProcesses(reportedAt time.Time) error {
	c.cliProcessesMu.Lock()
	defer c.cliProcessesMu.Unlock()
	if c.isRecordClosed {
		return nil
	}
	c.isRecordClosed = true
	members, err := shellenv.RecordShellCommandMembers(c.cmd, reportedAt)
	if err != nil {
		return err
	}
	c.cliProcesses = members
	return nil
}

func (c *nativeAgentCommand) releaseCLIProcesses() {
	c.cliProcessesMu.Lock()
	defer c.cliProcessesMu.Unlock()
	c.isRecordClosed = true
	if c.cliProcesses != nil {
		c.cliProcesses.Release()
		c.cliProcesses = nil
	}
}

// stopLeader kills the agent process and the CLI processes recorded beside it,
// and leaves the work the agent started in its process group or job, so the
// owned-children probe decides whether that work is still running, as it does
// for an agent that exited by itself.
func (c *nativeAgentCommand) stopLeader() {
	c.isLeaderStopped.Store(true)
	c.cliProcessesMu.Lock()
	if c.cliProcesses != nil {
		c.cliProcesses.Kill()
	}
	c.cliProcessesMu.Unlock()
	_ = c.cmd.Process.Kill()
}

func (c *nativeAgentCommand) terminate() {
	c.terminateOnce.Do(func() {
		shellenv.TerminateShellCommandGroup(c.cmd)
	})
}

// waitAfterParseError reaps the agent after its output failed to parse and
// returns the parse error. A WaitDelay expiry is attached to it rather than
// substituted for it, so the real cause is never hidden. Only stdout, which the
// parser abandoned, is closed: stderr stays open until its reader drains what
// the agent wrote before it was stopped, such as a safety refusal (issue #184),
// and WaitDelay still bounds a descendant that holds it open.
func (c *nativeAgentCommand) waitAfterParseError(parseErr error) error {
	c.terminate()
	_ = c.stdout.Close()
	if waitErr := c.wait(); errors.Is(waitErr, exec.ErrWaitDelay) {
		return fmt.Errorf("%w (%v)", parseErr, waitErr)
	}
	return parseErr
}

// closedByWaitDelay reports whether parseErr is only the stdout read failing
// because WaitDelay force-closed a pipe that an escaped descendant of a cleanly
// exited agent kept open. Call it after waitAfterParseError.
func (c *nativeAgentCommand) closedByWaitDelay(parseErr error) bool {
	return errors.Is(parseErr, os.ErrClosed) && errors.Is(c.wait(), exec.ErrWaitDelay)
}

// waitKeepingResult returns the agent's wait error, except that a bare
// WaitDelay expiry is not an error once the caller holds a finished result:
// the agent exited cleanly and only a lingering descendant held a pipe open.
func (c *nativeAgentCommand) waitKeepingResult(finished bool) error {
	err := c.wait()
	if finished && errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

func (c *nativeAgentCommand) wait() error {
	c.waitOnce.Do(func() { c.waitErr = <-c.waitCh })
	return c.waitErr
}

func (c *nativeAgentCommand) closePipes() {
	c.closePipesOnce.Do(func() {
		_ = c.stdout.Close()
		_ = c.stderr.Close()
	})
}
