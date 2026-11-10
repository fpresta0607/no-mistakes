package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testlink"
)

func TestApplyEditsCreatesParentDirectoriesForNewFiles(t *testing.T) {
	dir := t.TempDir()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	if err := applyEdits([]Edit{{Path: filepath.Join("nested", "dir", "note.txt"), New: "hello\n"}}); err != nil {
		t.Fatalf("applyEdits: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "nested", "dir", "note.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("file contents = %q, want %q", data, "hello\n")
	}
}

func TestActionStructuredJSONUsesRawPayload(t *testing.T) {
	action := Action{
		Structured:    map[string]any{"summary": "ignored"},
		StructuredRaw: `"not an object"`,
	}

	if got := string(action.structuredJSON()); got != `"not an object"` {
		t.Fatalf("structuredJSON() = %s, want raw payload", got)
	}
}

func TestApplyActionStagesFiles(t *testing.T) {
	dir := t.TempDir()
	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gitCmd("init")

	if err := applyActionInDir(dir, Action{
		Edits: []Edit{{Path: "agent_test.go", New: "package main\n"}},
		Stage: []string{"agent_test.go"},
	}); err != nil {
		t.Fatalf("applyActionInDir: %v", err)
	}

	status := gitCmd("status", "--porcelain")
	if !strings.Contains(status, "A  agent_test.go") {
		t.Fatalf("git status = %q, want staged agent_test.go", status)
	}
}

func TestApplyActionHonorsDelay(t *testing.T) {
	start := time.Now()
	if err := applyActionInDir(t.TempDir(), Action{DelayMS: 20}); err != nil {
		t.Fatalf("applyActionInDir: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("applyActionInDir returned after %s, want at least 20ms", elapsed)
	}
}

func TestApplyEditsRejectsPathsOutsideWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	err = applyEdits([]Edit{{Path: filepath.Join("..", filepath.Base(outside)), New: "hello\n"}})
	if err == nil {
		t.Fatal("applyEdits succeeded, want error")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside file exists or unexpected error: %v", statErr)
	}
}

func TestApplyEditsRejectsSymlinkPathsOutsideWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")

	linkPath := filepath.Join(dir, "escape")
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	err = applyEdits([]Edit{{Path: filepath.Join("escape", "outside.txt"), New: "hello\n"}})
	if err == nil {
		t.Fatal("applyEdits succeeded, want error")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside file exists or unexpected error: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(linkPath, "outside.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("symlink target file exists or unexpected error: %v", statErr)
	}
	if !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("error = %q, want working directory violation", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote(filepath.Join("escape", "outside.txt"))) {
		t.Fatalf("error = %q, want offending path", err)
	}
}

// The e2e harness's scratch home can sit below a directory junction, as the
// live gate's tmp folder does, and edits there must still land (issue #172).
func TestApplyEditsInDirWritesBelowAJunctionedWorkingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T, linkRoot string) string
	}{
		{"same volume", func(t *testing.T, _ string) string { return t.TempDir() }},
		{"another volume", func(t *testing.T, linkRoot string) string { return testlink.OtherVolumeDir(t, linkRoot) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			linkRoot := t.TempDir()
			target := tc.target(t, linkRoot)
			if err := os.Mkdir(filepath.Join(target, "run"), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(linkRoot, "tmp")
			testlink.Junction(t, link, target)

			err := applyEditsInDir(filepath.Join(link, "run"), []Edit{{Path: filepath.Join("nested", "file.txt"), New: "hello"}})

			if err != nil {
				t.Fatalf("applyEditsInDir below a junction: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(target, "run", "nested", "file.txt"))
			if err != nil || string(data) != "hello" {
				t.Fatalf("file content = %q, %v; want %q", data, err, "hello")
			}
		})
	}
}

func TestApplyEditsInDirRejectsAJunctionOutOfTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	testlink.Junction(t, filepath.Join(dir, "escape"), outsideDir)

	err := applyEditsInDir(dir, []Edit{{Path: filepath.Join("escape", "outside.txt"), New: "hello"}})

	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("applyEditsInDir through an escaping junction: %v, want working directory violation", err)
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "outside.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("junction target file exists or unexpected error: %v", statErr)
	}
}

func TestRunClaudeFailsWhenScenarioEditReplacementMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	defer os.Chdir(wd)

	scenario := &Scenario{Actions: []Action{{
		Match: "fix it",
		Edits: []Edit{{Path: "note.txt", Old: "missing", New: "after"}},
	}}}

	if code := runClaude([]string{"-p"}, strings.NewReader("fix it"), scenario); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "before\n" {
		t.Fatalf("file contents = %q, want unchanged", data)
	}
}
