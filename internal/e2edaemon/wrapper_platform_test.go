package e2edaemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On Windows a bare bash can resolve to the WSL launcher, whose Linux shell
// built and started the suite with another platform's go, or found none (issue
// #153). The wrapper refuses before its first build or daemon launch unless its
// shell and the go it finds target one platform.
func TestE2EWrapperRefusesAGoOfAnotherPlatformBeforeAnyBuild(t *testing.T) {
	bash := suiteWrapperBash(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		goAnswer   string
		goExitCode string
		wantReport string
	}{
		{name: "go of another platform", goAnswer: "plan9", goExitCode: "0", wantReport: "targets plan9"},
		{name: "no usable go", goAnswer: "", goExitCode: "127", wantReport: "targets nothing"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scratch := t.TempDir()
			goLog := filepath.ToSlash(filepath.Join(scratch, "go.log"))
			fakeGo := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>'" + goLog + "'\n[ -n '" + testCase.goAnswer + "' ] && printf '%s\\n' '" + testCase.goAnswer + "'\nexit " + testCase.goExitCode + "\n"
			if err := os.WriteFile(filepath.Join(scratch, "go"), []byte(fakeGo), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			wrapper := exec.CommandContext(ctx, bash, "scripts/e2e.sh", "test")
			wrapper.Dir = root
			wrapper.Env = append(filterInventoryEnv(os.Environ()), "PATH="+scratch+string(os.PathListSeparator)+os.Getenv("PATH"))

			output, err := wrapper.CombinedOutput()

			if err == nil {
				t.Fatalf("wrapper ran with a go of another platform; output:\n%s", output)
			}
			if !strings.Contains(string(output), testCase.wantReport) || !strings.Contains(string(output), "nothing was built or started") {
				t.Fatalf("wrapper refusal does not name the mismatch (want %q); output:\n%s", testCase.wantReport, output)
			}
			calls, err := os.ReadFile(goLog)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(calls)); got != "env GOOS" {
				t.Fatalf("wrapper asked go for more than its platform before refusing: %q", got)
			}
		})
	}
}
