//go:build windows

package winproc

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HiddenDesktop is a desktop of its own in this process's window station.
// Nothing ever switches to it, so a window a program opens on it never shows
// on the user's screen, and a process started on it passes it on to every
// process it starts. While it is open it records every visible top-level
// window that appears on it, so the windows its programs opened can be named
// even after they closed.
type HiddenDesktop struct {
	name   string
	handle uintptr
	stop   chan struct{}
	done   chan struct{}

	closeOnce sync.Once
	closeErr  error

	mu      sync.Mutex
	seen    map[windowIdentity]bool
	windows []Window
	more    int
}

// windowIdentity tells two windows apart. A closed window's handle can be
// reused, so the owning process is part of it.
type windowIdentity struct {
	handle uintptr
	pid    uint32
}

const (
	// desktopAllAccess is DESKTOP_ALL_ACCESS less the standard rights.
	desktopAllAccess = 0x01ff
	// desktopWatchInterval paces the look for new windows. A window open
	// for less than this can be missed.
	desktopWatchInterval = 250 * time.Millisecond
	// maxDesktopWindows bounds the windows a desktop records; the rest are
	// only counted.
	maxDesktopWindows = 50
	// maxWindowTitle bounds the title a Window keeps, so a program that puts
	// a whole document in its title cannot fill a report.
	maxWindowTitle = 120
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	procCreateDesktopW     = user32.NewProc("CreateDesktopW")
	procCloseDesktop       = user32.NewProc("CloseDesktop")
	procEnumDesktopWindows = user32.NewProc("EnumDesktopWindows")
	procGetWindowTextW     = user32.NewProc("GetWindowTextW")

	// enumDesktopWindow is created once: a Go process can create only a
	// limited number of callbacks, and never frees one.
	enumDesktopWindow = sync.OnceValue(func() uintptr {
		return syscall.NewCallback(func(window, _ uintptr) uintptr {
			enumerated = append(enumerated, window)
			return 1
		})
	})
	// enumMu serializes the enumerations that fill enumerated.
	enumMu     sync.Mutex
	enumerated []uintptr
)

// NewHiddenDesktop creates a hidden desktop and starts recording the windows
// opened on it. Close it once nothing runs on it any more.
func NewHiddenDesktop() (*HiddenDesktop, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	name := "no-mistakes-" + hex.EncodeToString(id[:])
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, _, callErr := procCreateDesktopW.Call(uintptr(unsafe.Pointer(namePtr)), 0, 0, 0, desktopAllAccess, 0)
	if handle == 0 {
		return nil, fmt.Errorf("create hidden desktop %s: %w", name, callErr)
	}
	desktop := &HiddenDesktop{
		name:   name,
		handle: handle,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		seen:   map[windowIdentity]bool{},
	}
	go desktop.watch()
	return desktop, nil
}

// Name is the desktop's name, which a process start names to run there.
func (d *HiddenDesktop) Name() string {
	if d == nil {
		return ""
	}
	return d.name
}

// Windows returns the visible top-level windows seen on the desktop so far,
// in the order they appeared, and how many more appeared past the ones it
// records.
func (d *HiddenDesktop) Windows() ([]Window, int) {
	if d == nil {
		return nil, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Window(nil), d.windows...), d.more
}

// Close stops the recording, after a last look for windows, and closes the
// desktop. Windows destroys it once nothing runs on it any more.
func (d *HiddenDesktop) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		close(d.stop)
		<-d.done
		d.record()
		if closed, _, err := procCloseDesktop.Call(d.handle); closed == 0 {
			d.closeErr = fmt.Errorf("close hidden desktop %s: %w", d.name, err)
		}
	})
	return d.closeErr
}

func (d *HiddenDesktop) watch() {
	defer close(d.done)
	ticker := time.NewTicker(desktopWatchInterval)
	defer ticker.Stop()
	for {
		d.record()
		select {
		case <-d.stop:
			return
		case <-ticker.C:
		}
	}
}

// record adds every visible top-level window on the desktop it has not seen.
func (d *HiddenDesktop) record() {
	for _, handle := range desktopWindows(d.handle) {
		if !windows.IsWindowVisible(windows.HWND(handle)) {
			continue
		}
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(windows.HWND(handle), &pid); err != nil {
			continue
		}
		identity := windowIdentity{handle: handle, pid: pid}
		d.mu.Lock()
		isSeen := d.seen[identity]
		d.seen[identity] = true
		d.mu.Unlock()
		if isSeen {
			continue
		}
		window := Window{PID: int(pid), Image: processImage(pid), Title: windowTitle(windowText(handle))}
		d.mu.Lock()
		if len(d.windows) < maxDesktopWindows {
			d.windows = append(d.windows, window)
		} else {
			d.more++
		}
		d.mu.Unlock()
	}
}

// desktopWindows lists the top-level windows on desktop.
func desktopWindows(desktop uintptr) []uintptr {
	callback := enumDesktopWindow()
	enumMu.Lock()
	defer enumMu.Unlock()
	enumerated = nil
	procEnumDesktopWindows.Call(desktop, callback, 0)
	found := enumerated
	enumerated = nil
	return found
}

func windowText(window uintptr) string {
	text := make([]uint16, 512)
	copied, _, _ := procGetWindowTextW.Call(window, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)))
	return windows.UTF16ToString(text[:copied])
}

// windowTitle trims title and cuts it to maxWindowTitle characters.
func windowTitle(title string) string {
	title = strings.TrimSpace(title)
	if runes := []rune(title); len(runes) > maxWindowTitle {
		return string(runes[:maxWindowTitle]) + "..."
	}
	return title
}

// processImage is the base name of pid's executable, or empty when the
// process is gone or cannot be read.
func processImage(pid uint32) string {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(process)
	path := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(path))
	if err := windows.QueryFullProcessImageName(process, 0, &path[0], &size); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(path[:size]))
}
