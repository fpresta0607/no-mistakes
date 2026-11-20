package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"golang.org/x/sys/windows"
)

type processPriorities struct {
	class uint32
	io    uint32
	page  uint32
}

func (p processPriorities) String() string {
	return fmt.Sprintf("class=%#x io=%d page=%d", p.class, p.io, p.page)
}

func readProcessPriorities(t *testing.T, process windows.Handle) processPriorities {
	t.Helper()
	class, err := windows.GetPriorityClass(process)
	if err != nil {
		t.Fatalf("read priority class: %v", err)
	}
	read := func(class int32) uint32 {
		var value uint32
		if err := windows.NtQueryInformationProcess(process, class, unsafe.Pointer(&value), uint32(unsafe.Sizeof(value)), nil); err != nil {
			t.Fatalf("read process information class %d: %v", class, err)
		}
		return value
	}
	return processPriorities{class: class, io: read(windows.ProcessIoPriority), page: read(windows.ProcessPagePriority)}
}

func setProcessPriorities(t *testing.T, want processPriorities) {
	t.Helper()
	process := windows.CurrentProcess()
	if err := windows.SetPriorityClass(process, want.class); err != nil {
		t.Fatalf("set priority class %#x: %v", want.class, err)
	}
	for _, setting := range []struct {
		class int32
		value uint32
	}{{windows.ProcessIoPriority, want.io}, {windows.ProcessPagePriority, want.page}} {
		value := setting.value
		if err := windows.NtSetInformationProcess(process, setting.class, unsafe.Pointer(&value), uint32(unsafe.Sizeof(value))); err != nil {
			t.Fatalf("set process information class %d to %d: %v", setting.class, value, err)
		}
	}
}

// livePriorities reads a process's priorities, and reports false for one that
// already exited.
func livePriorities(t *testing.T, pid uint32) (processPriorities, bool) {
	t.Helper()
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return processPriorities{}, false
	}
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	defer windows.CloseHandle(process)
	return readProcessPriorities(t, process), true
}

func childPIDs(t *testing.T, parent uint32) []uint32 {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatalf("snapshot processes: %v", err)
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var children []uint32
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if entry.ParentProcessID == parent {
			children = append(children, entry.ProcessID)
		}
	}
	return children
}

// Issue #248: a daemon started below normal waited seconds for a processor on
// a busy host and answered even health that late, and every git, agent and
// test command it started inherited its low processor, disk and memory page
// priority. Task Scheduler's default priority 7 was one such launcher (#253);
// a detached start or a restart for update from a below-normal process is
// another. This starts a real daemon the way launchDetachedDaemon does, from a
// process lowered the way priority 7 lowers it, and requires the daemon, and
// the log sink it starts first, to run at normal on all three. The launcher is
// lowered only for the process creation: at those values the rest of a
// detached start took minutes on a busy host.
func TestDaemonStartedBelowNormalRaisesItselfToNormal(t *testing.T) {
	root, err := os.MkdirTemp("", "dprio")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_HOME", root)
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "daemon")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "daemon", "run", "--root", root)
	setSysProcAttr(cmd)

	original := readProcessPriorities(t, windows.CurrentProcess())
	t.Cleanup(func() { setProcessPriorities(t, original) })
	launcher := processPriorities{class: windows.BELOW_NORMAL_PRIORITY_CLASS, io: 1, page: 2}
	setProcessPriorities(t, launcher)
	startErr := cmd.Start()
	setProcessPriorities(t, original)
	if startErr != nil {
		t.Fatalf("start daemon: %v", startErr)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		shutdownIsolatedDaemon(t, p, pid)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if alive, _ := daemonHealthCheck(p); alive {
			break
		}
		if time.Now().After(deadline) {
			bootstrap, _ := os.ReadFile(p.DaemonBootstrapLog())
			t.Fatalf("daemon %d did not answer health within 2m\nbootstrap log:\n%s", pid, bootstrap)
		}
		time.Sleep(50 * time.Millisecond)
	}

	normal := processPriorities{class: windows.NORMAL_PRIORITY_CLASS, io: normalIOPriority, page: normalPagePriority}
	got, alive := livePriorities(t, uint32(pid))
	if !alive {
		t.Fatalf("daemon %d exited after answering health", pid)
	}
	if got != normal {
		t.Fatalf("daemon started from a %v launcher reports %v, want %v", launcher, got, normal)
	}
	children := childPIDs(t, uint32(pid))
	if len(children) == 0 {
		t.Fatal("daemon has no child process; want its bootstrap log sink")
	}
	for _, child := range children {
		if got, alive := livePriorities(t, child); alive && got != normal {
			t.Fatalf("daemon child %d reports %v, want %v", child, got, normal)
		}
	}
}
