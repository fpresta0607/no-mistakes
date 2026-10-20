package winproc

import "fmt"

// Window is one visible top-level window a program opened on a
// HiddenDesktop.
type Window struct {
	// PID is the process that owns the window.
	PID int
	// Image is the base name of that process's executable, or empty when it
	// could not be read.
	Image string
	// Title is the window's title, cut to a bounded length.
	Title string
}

// String names the window for a person: its program, its process and its
// title.
func (w Window) String() string {
	image := w.Image
	if image == "" {
		image = "a program"
	}
	title := "with no title"
	if w.Title != "" {
		title = fmt.Sprintf("titled %q", w.Title)
	}
	return fmt.Sprintf("%s (pid %d) %s", image, w.PID, title)
}
