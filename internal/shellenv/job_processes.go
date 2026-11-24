package shellenv

import "strings"

// jobProcessState is what could be established about a process that a
// Windows job still lists after its leader exited.
type jobProcessState int

const (
	// jobProcessRunning: the process was opened, has not exited, and is still
	// a member of the job.
	jobProcessRunning jobProcessState = iota
	// jobProcessExited: the listing is stale. The process is gone, or its id
	// now belongs to a process outside the job.
	jobProcessExited
	// jobProcessUnknown: the process could not be inspected, so nothing can be
	// said about it.
	jobProcessUnknown
)

// jobProcess is one entry of a job's process list with the facts the
// child-completion check needs.
type jobProcess struct {
	pid       uint32
	parentPID uint32
	state     jobProcessState
	// imagePath is the process's full image path when the inspection could
	// read it; it names the member in the active verdict's detail.
	imagePath string
	// systemConsoleHost is true only for the operating system's conhost.exe,
	// identified by its full image path under the system directory.
	systemConsoleHost bool
}

// offendingJobProcesses returns the entries that make a leader's clean exit
// unverified: a process that is really still running, or one that could not be
// inspected. Three kinds of entry are not returned. An entry that has already
// exited is a stale listing. The operating system's console host that the
// leader itself started (its parent is the leader) is not a child the leader
// owns: a console child gets a headless console host that joins the job and can
// briefly outlive it. The MSVC toolchain's telemetry helper is not unfinished
// work: link.exe starts it and it outlives the link that started it (issue
// #259). All three are excluded by identity, never by waiting them out.
// Anything that cannot be proven to be one of the three is returned, which
// keeps the check fail-closed.
func offendingJobProcesses(procs []jobProcess, leaderPID uint32) []jobProcess {
	var offenders []jobProcess
	for _, p := range procs {
		switch {
		case p.state == jobProcessExited:
		case p.state == jobProcessRunning && p.systemConsoleHost && p.parentPID == leaderPID:
		case p.state == jobProcessRunning && isMSVCTelemetryHelper(p.imagePath):
		default:
			offenders = append(offenders, p)
		}
	}
	return offenders
}

// msvcTelemetryHelperPath is the tail of the telemetry helper's image path in
// an MSVC toolchain, from the toolchain root to the file:
// VC\Tools\MSVC\<version>\bin\Host<arch>\<arch>\vctip.exe. An empty
// component matches any non-empty one, and "Host" matches as a prefix.
var msvcTelemetryHelperPath = []string{"VC", "Tools", "MSVC", "", "bin", "Host", "", "vctip.exe"}

// isMSVCTelemetryHelper reports whether imagePath is vctip.exe inside an MSVC
// toolchain's bin directory. The whole directory shape is required, so a
// vctip.exe anywhere else is still a child.
func isMSVCTelemetryHelper(imagePath string) bool {
	parts := strings.Split(imagePath, `\`)
	if len(parts) <= len(msvcTelemetryHelperPath) {
		return false
	}
	parts = parts[len(parts)-len(msvcTelemetryHelperPath):]
	for i, want := range msvcTelemetryHelperPath {
		got := parts[i]
		switch {
		case got == "":
			return false
		case want == "":
		case want == "Host":
			if len(got) <= len(want) || !strings.EqualFold(got[:len(want)], want) {
				return false
			}
		case !strings.EqualFold(got, want):
			return false
		}
	}
	return true
}

func liveJobProcesses(procs []jobProcess, leaderPID uint32) int {
	return len(offendingJobProcesses(procs, leaderPID))
}

// jobHasLiveProcesses decides whether a job still holds a process that makes
// the leader's clean exit unverified. The job's process list and the per
// process inspections are not one atomic read: a listed child can start a new
// process after the list was read and exit before it is inspected, which leaves
// that child classified as exited while its own child was never listed.
// totalBefore and totalAfter are the job's TotalProcesses counter, which counts
// every process ever assigned to the job (exited ones included), read before the
// list and again after the last inspection. If it moved, some process joined
// the job while it was being inspected, so the classification of procs cannot be
// trusted and the job is reported as active without waiting. This also
// covers a new process that reused the id of a listed one.
func jobHasLiveProcesses(totalBefore, totalAfter uint32, procs []jobProcess, leaderPID uint32) bool {
	return totalAfter != totalBefore || liveJobProcesses(procs, leaderPID) > 0
}
