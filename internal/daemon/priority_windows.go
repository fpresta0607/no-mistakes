package daemon

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	normalIOPriority   = 2 // IoPriorityNormal
	normalPagePriority = 5 // MEMORY_PRIORITY_NORMAL
)

// raiseInheritedPriority lifts the daemon to a normal processor priority
// class, disk (I/O) priority and memory page priority when its launcher
// started it lower, and reports what it raised ("" when nothing). The daemon
// outlives its launcher and every git, agent and test command it starts
// inherits all three, so they must not depend on how it was started: Task
// Scheduler's default task priority 7 (issue #253) and a detached start or a
// restart for update from a below-normal process all leave it below normal.
// On a busy host a below-normal daemon waited seconds for a processor and
// answered even its health request that late, while the same IPC server at
// normal priority answered in milliseconds (issue #248). Higher values are
// left alone.
func raiseInheritedPriority() (string, error) {
	process := windows.CurrentProcess()
	var raised string
	class, err := windows.GetPriorityClass(process)
	if err != nil {
		return "", fmt.Errorf("read priority class: %w", err)
	}
	if class == windows.IDLE_PRIORITY_CLASS || class == windows.BELOW_NORMAL_PRIORITY_CLASS {
		if err := windows.SetPriorityClass(process, windows.NORMAL_PRIORITY_CLASS); err != nil {
			return "", fmt.Errorf("raise priority class %#x to normal: %w", class, err)
		}
		raised = fmt.Sprintf("class=%#x", class)
	}
	for _, setting := range []struct {
		name   string
		class  int32
		normal uint32
	}{
		{name: "io", class: windows.ProcessIoPriority, normal: normalIOPriority},
		{name: "page", class: windows.ProcessPagePriority, normal: normalPagePriority},
	} {
		var value uint32
		if err := windows.NtQueryInformationProcess(process, setting.class, unsafe.Pointer(&value), uint32(unsafe.Sizeof(value)), nil); err != nil {
			return raised, fmt.Errorf("read %s priority: %w", setting.name, err)
		}
		if value >= setting.normal {
			continue
		}
		normal := setting.normal
		if err := windows.NtSetInformationProcess(process, setting.class, unsafe.Pointer(&normal), uint32(unsafe.Sizeof(normal))); err != nil {
			return raised, fmt.Errorf("raise %s priority %d to %d: %w", setting.name, value, normal, err)
		}
		if raised != "" {
			raised += " "
		}
		raised += fmt.Sprintf("%s=%d", setting.name, value)
	}
	return raised, nil
}
