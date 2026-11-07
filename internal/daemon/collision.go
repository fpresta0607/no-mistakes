package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// daemonProcessInfo describes a running `no-mistakes daemon run --root <root>`
// process discovered via the OS process list.
type daemonProcessInfo struct {
	PID  int
	Root string // raw --root value exactly as it appeared on the command line
}

// daemonListDaemonProcesses enumerates no-mistakes managed-daemon processes. It
// is a package var so tests can stub the process list deterministically.
var daemonListDaemonProcesses = listDaemonProcesses

// errDaemonCollisionHealthy signals that Start refused to launch because a
// healthy daemon for the same logical root is already running under a different
// path spelling (e.g. a symlinked NM_HOME).
var errDaemonCollisionHealthy = errors.New("daemon already running")

// daemonProcessLineSplitter extracts a (pid, command line) pair from one line
// of platform-specific process-listing output.
type daemonProcessLineSplitter func(line string) (pid int, command string, ok bool)

// parseDaemonProcessOutput walks a process listing and returns every line that
// looks like `no-mistakes daemon run --root <root>`, extracting the pid and the
// raw --root value. The splitter normalizes each platform's output into a pid
// plus the full command line.
func parseDaemonProcessOutput(output string, split daemonProcessLineSplitter) []daemonProcessInfo {
	var infos []daemonProcessInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		pid, command, ok := split(line)
		if !ok {
			continue
		}
		if !looksLikeDaemonRunCommand(command) {
			continue
		}
		root, hasRoot := extractRootFromCommand(command)
		if !hasRoot {
			continue
		}
		infos = append(infos, daemonProcessInfo{PID: pid, Root: root})
	}
	return infos
}

// looksLikeDaemonRunCommand reports whether a command line is a no-mistakes
// daemon invocation (`... daemon run ...`) with an explicit root.
func looksLikeDaemonRunCommand(command string) bool {
	tokens := splitCommandLineTokens(command)
	hasDaemon, hasRun := false, false
	for _, token := range tokens {
		switch strings.ToLower(token) {
		case "daemon":
			hasDaemon = true
		case "run":
			hasRun = true
		}
	}
	return hasDaemon && hasRun
}

// extractRootFromCommand pulls the --root value out of a command line, accepting
// both `--root <value>` and `--root=<value>`, with or without shell quoting.
func extractRootFromCommand(command string) (string, bool) {
	tokens := splitCommandLineTokens(command)
	for i := 0; i < len(tokens); i++ {
		switch {
		case tokens[i] == "--root":
			if i+1 < len(tokens) {
				return tokens[i+1], true
			}
			return "", false
		case strings.HasPrefix(tokens[i], "--root="):
			return strings.TrimPrefix(tokens[i], "--root="), true
		}
	}
	return "", false
}

// splitCommandLineTokens splits a command line into argv-style tokens, honoring
// double-quote grouping and backslash escapes the way a POSIX shell / Windows
// command line roughly would. It is intentionally permissive: the goal is to
// recover the --root argument from ps/CIM output, not to be a flawless shell
// parser.
func splitCommandLineTokens(command string) []string {
	var tokens []string
	var current strings.Builder
	inQuotes := false
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case escaped:
			current.WriteByte(ch)
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
		case (ch == ' ' || ch == '\t') && !inQuotes:
			flush()
		default:
			current.WriteByte(ch)
		}
	}
	flush()
	return tokens
}

// reconcileCollidingDaemons detects no-mistakes daemons already running for the
// same logical root as p but started under a different path spelling (e.g. a
// symlinked or relative NM_HOME). The socket-keyed health check in Start cannot
// see those: paths.Paths stores the raw root string and derives its socket from
// it, so two spellings of the same directory produce two different sockets.
//
// Live or unreadable owners refuse startup even when IPC does not answer.
// A process already gone needs no kill or service stop; the caller retains
// lifecycle and singleton ownership while checking and clearing stale artifacts.
func reconcileCollidingDaemons(p *paths.Paths) error {
	want := canonicalRoot(p.Root())
	if want == "" {
		return nil
	}
	procs, err := daemonListDaemonProcesses()
	if err != nil {
		return fmt.Errorf("cannot inspect daemon ownership; refusing start: %w", err)
	}

	self := os.Getpid()
	for _, info := range procs {
		if info.PID == self {
			continue
		}
		if info.Root == "" || canonicalRoot(info.Root) != want {
			continue
		}
		if alive, _ := daemonHealthCheck(paths.WithRoot(info.Root)); alive {
			slog.Info("daemon already running under alternate root path", "pid", info.PID, "root", info.Root)
			return fmt.Errorf("%w (pid %d)", errDaemonCollisionHealthy, info.PID)
		}
		running, err := daemonProcessRunning(info.PID)
		if err != nil {
			return fmt.Errorf("cannot inspect daemon pid %d; refusing start: %w", info.PID, err)
		}
		if running {
			return fmt.Errorf("daemon pid %d has not confirmed shutdown; refusing start", info.PID)
		}
	}
	return nil
}
