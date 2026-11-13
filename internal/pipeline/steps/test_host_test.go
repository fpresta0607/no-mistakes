package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
)

func TestNativeTestBashRefusesWSLBeforeVerifyingNativeShell(t *testing.T) {
	t.Parallel()
	root, err := realpath.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	systemRoot := filepath.Join(root, "Windows")
	wsl := filepath.Join(systemRoot, "System32", "bash.exe")
	native := filepath.Join(root, "Git", "bin", "bash.exe")
	for _, path := range []string{wsl, native} {
		writeTestFile(t, path, "fixture")
	}
	var verified []string
	got, err := resolveNativeTestBash([]string{wsl, native}, systemRoot, func(path string) error {
		verified = append(verified, path)
		return nil
	})
	if err != nil || got != native || len(verified) != 1 || verified[0] != native {
		t.Fatalf("resolved %q, verified %q, error %v", got, verified, err)
	}
}

func TestNativeTestBashMissingOrNonNativeRefusesWithoutWSL(t *testing.T) {
	t.Parallel()
	for _, isPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "not_native"}[isPresent], func(t *testing.T) {
			root, err := realpath.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			wsl := filepath.Join(root, "Windows", "System32", "bash.exe")
			native := filepath.Join(root, "Git", "bin", "bash.exe")
			writeTestFile(t, wsl, "never execute")
			if isPresent {
				writeTestFile(t, native, "not a native shell")
			}
			_, err = resolveNativeTestBash([]string{wsl, native}, filepath.Join(root, "Windows"), func(path string) error {
				if path == wsl {
					t.Fatal("WSL was verified or launched")
				}
				return errors.New("not MSYS Bash")
			})
			if err == nil || !strings.Contains(err.Error(), "native Git Bash") {
				t.Fatalf("missing prerequisite = %v", err)
			}
		})
	}
}

func TestTestHostScratchIsUniqueAndPreservesDeepEvidence(t *testing.T) {
	t.Parallel()
	evidence := filepath.Join(t.TempDir(), strings.Repeat("deep-", 25), "evidence")
	writeTestFile(t, filepath.Join(evidence, "retained.txt"), "retained evidence")
	first, err := newTestScratch("", runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(first)
	second, err := newTestScratch("", runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(second)
	writeTestFile(t, filepath.Join(second, "owned.txt"), "another run")
	if first == second || strings.HasPrefix(first, evidence) {
		t.Fatalf("scratch ownership overlaps: %q %q", first, second)
	}
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(evidence, "retained.txt"), filepath.Join(second, "owned.txt")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cleanup removed another owner's file %s: %v", path, err)
		}
	}
}

func TestTestHostScratchRefusesAnUnsupportedWindowsPath(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), strings.Repeat("deep", 25))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := newTestScratch(parent, "windows")
	if err == nil || !strings.Contains(err.Error(), "short") {
		t.Fatalf("unsupported Windows scratch path = %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refusal left scratch behind: %v, %v", entries, err)
	}
}

func TestTestHostScratchCleanupOnAgentFailureAndInterruption(t *testing.T) {
	t.Parallel()
	for _, failure := range []error{errors.New("agent failed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			var scratch string
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				scratch, _ = envValue(opts.Env, "TMPDIR")
				writeTestFile(t, filepath.Join(scratch, "temporary.txt"), "disposable")
				return nil, failure
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			writeTestFile(t, filepath.Join(sctx.EvidenceDir, "retained.txt"), "evidence")
			_, err := (&TestStep{}).Execute(sctx)
			if err == nil || scratch == "" {
				t.Fatalf("failure=%v scratch=%q", err, scratch)
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Fatalf("scratch outlived failure: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(sctx.EvidenceDir, "retained.txt"))
			if err != nil || string(data) != "evidence" {
				t.Fatalf("retained evidence changed: %q %v", data, err)
			}
		})
	}
}

func TestTestHostDeepEvidenceUsesShortScratchForAnActualGitPush(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	var scratch string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		scratch, _ = envValue(opts.Env, "GOTMPDIR")
		work := filepath.Join(scratch, "work")
		bare := filepath.Join(scratch, "TestAuthoredAdmissionSchedulesEveryStageFreshWithoutMovingSourceOrPredecessor", "001", "repos", "launch-assertion.git")
		if err := os.MkdirAll(work, 0o700); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, work, "init")
		gitCmd(t, work, "config", "user.name", "Test")
		gitCmd(t, work, "config", "user.email", "test@example.com")
		writeTestFile(t, filepath.Join(work, "test.txt"), "isolated push")
		gitCmd(t, work, "add", ".")
		gitCmd(t, work, "commit", "-m", "fixture")
		gitCmd(t, work, "init", "--bare", bare)
		gitCmd(t, work, "push", bare, "HEAD:refs/heads/main")
		if got, want := gitCmd(t, work, "rev-parse", "HEAD"), gitCmd(t, bare, "rev-parse", "refs/heads/main"); got != want {
			t.Fatalf("pushed %q, want %q", got, want)
		}
		return &agent.Result{Output: json.RawMessage(passingTestTurnOutput)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.EvidenceDir = filepath.Join(t.TempDir(), strings.Repeat("evidence-", 10))
	writeTestFile(t, filepath.Join(sctx.EvidenceDir, "retained.txt"), "recorded evidence")
	deepIncoming := filepath.Join(sctx.EvidenceDir, testScratchDirName, "TestAuthoredAdmissionSchedulesEveryStageFreshWithoutMovingSourceOrPredecessor", "001", "repos", "launch-assertion.git", "objects", "incoming-123456", "pack", "tmp_pack_123456")
	if len(deepIncoming) <= 260 {
		t.Fatalf("fixture does not reproduce the deep incoming path: %d", len(deepIncoming))
	}
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && len(scratch) > testWindowsScratchMaxChars {
		t.Fatalf("unsupported Windows scratch: %s", scratch)
	}
	if _, err := os.Stat(filepath.Join(sctx.EvidenceDir, "retained.txt")); err != nil {
		t.Fatalf("evidence was removed: %v", err)
	}
	t.Logf("source=%s deep_incoming_chars=%d owned_scratch=%s real_git=%s push=passed", head, len(deepIncoming), scratch, testGitExecutable)
}

// An operator-approved Test command writes its private outputs outside the
// worktree, so every Test agent prompt names the operator's directories as the
// one extra destination, and names nothing it was not given (issue #123).
func TestTestStep_PromptNamesOnlyTheOperatorAuthorizedOutputDirs(t *testing.T) {
	t.Parallel()
	const authorizedLine = "The operator authorized these directories as output destinations"
	for _, tc := range []struct {
		name          string
		isFixing      bool
		hasOutputDirs bool
		wantCalls     int
	}{
		{"evidence_turn_without_dirs", false, false, 1},
		{"evidence_turn_with_dirs", false, true, 1},
		{"fix_round_without_dirs", true, false, 2},
		{"fix_round_with_dirs", true, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			authorized, unlisted := t.TempDir(), t.TempDir()
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("test repair\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair test failure","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			if tc.hasOutputDirs {
				sctx.Run.TestOutputDirs = []string{authorized}
			}
			if tc.isFixing {
				sctx.Fixing = true
				sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","file":"fix.txt","description":"test failed"}]}`
			}

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatalf("test step failed: %v", err)
			}

			if len(ag.calls) != tc.wantCalls {
				t.Fatalf("agent calls = %d, want %d", len(ag.calls), tc.wantCalls)
			}
			for index, call := range ag.calls {
				if strings.Contains(call.Prompt, unlisted) {
					t.Errorf("prompt %d names the unlisted directory %s", index, unlisted)
				}
				wantLines, wantNamed := 0, 0
				if tc.hasOutputDirs {
					wantLines, wantNamed = 1, 1
				}
				if got := strings.Count(call.Prompt, authorizedLine); got != wantLines {
					t.Errorf("prompt %d carries the authorized-destination line %d times, want %d:\n%s", index, got, wantLines, call.Prompt)
				}
				if got := strings.Count(call.Prompt, authorized); got != wantNamed {
					t.Errorf("prompt %d names %s %d times, want %d", index, authorized, got, wantNamed)
				}
			}
		})
	}
}
