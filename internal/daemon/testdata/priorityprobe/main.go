//go:build windows

// Command priorityprobe is a test helper for internal/daemon. The test builds
// it with -ldflags=-H=windowsgui, so Task Scheduler starts it without a
// console window, and registers it as the action of a disposable copy of the
// managed daemon's scheduled task. Task Scheduler passes it the daemon's
// arguments, "daemon run --root <root>". It starts itself once more the way
// the daemon starts git, agents and test commands, with no priority or
// creation flags, and that child writes its own processor priority class,
// disk (I/O) priority and memory page priority to <root>/child-priority.txt
// as "class=<hex> io=<n> page=<n>".
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func main() {
	args := os.Args[1:]
	if len(args) < 2 || args[len(args)-2] != "--root" {
		os.Exit(2)
	}
	root := args[len(args)-1]
	if args[0] == "child" {
		if err := os.WriteFile(filepath.Join(root, "child-priority.txt"), []byte(report()), 0o644); err != nil {
			os.Exit(1)
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	if err := exec.Command(self, "child", "--root", root).Run(); err != nil {
		os.Exit(1)
	}
}

func report() string {
	process := windows.CurrentProcess()
	class, err := windows.GetPriorityClass(process)
	if err != nil {
		return "error=" + err.Error()
	}
	var ioPriority, pagePriority uint32
	if err := windows.NtQueryInformationProcess(process, windows.ProcessIoPriority, unsafe.Pointer(&ioPriority), uint32(unsafe.Sizeof(ioPriority)), nil); err != nil {
		return "error=" + err.Error()
	}
	if err := windows.NtQueryInformationProcess(process, windows.ProcessPagePriority, unsafe.Pointer(&pagePriority), uint32(unsafe.Sizeof(pagePriority)), nil); err != nil {
		return "error=" + err.Error()
	}
	return fmt.Sprintf("class=%#x io=%d page=%d", class, ioPriority, pagePriority)
}
