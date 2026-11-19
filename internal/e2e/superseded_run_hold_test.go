//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSupersededRunHoldsWhileTestStartsItsCommand is issue #213's regression.
// The Test step reads running before it starts commands.test, so anything the
// journey changes once it sees running can land before the command starts. A
// test_command_wrapper that waits for a release file holds the step in exactly
// that window, and the journey's own preparation runs inside it: the first run
// must still hold its slow baseline until the superseding push cancels it.
func TestSupersededRunHoldsWhileTestStartsItsCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the configured test command and the hold wrapper are POSIX shell scripts")
	}
	wrapperDir := t.TempDir()
	release := filepath.Join(wrapperDir, "release")
	wrapper := filepath.Join(wrapperDir, "nm-e2e-hold-test-start")
	script := "#!/bin/sh\ni=0\nwhile [ ! -e '" + release + "' ] && [ \"$i\" -lt 600 ]; do\n  sleep 0.1\n  i=$((i + 1))\ndone\nexec \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write hold wrapper: %v", err)
	}
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		GlobalConfigExtra: "test_command_wrapper:\n  command: [\"" + wrapper + "\"]\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	assertSupersededRunCancellationAfter(t, h, func() {
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatalf("release hold wrapper: %v", err)
		}
	})
}
