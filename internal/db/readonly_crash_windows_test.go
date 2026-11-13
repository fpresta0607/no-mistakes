package db

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// helperProcessContext bounds a helper process this test starts by the test's
// own deadline, keeping a tenth of the remaining time back so the test reports
// the helper's failure before the binary's deadline panic. A fixed 15s bound
// cut the crash writer off on a slow runner, and Windows reported the kill
// only as "exit status 1" (issue #251).
func helperProcessContext(t *testing.T) context.Context {
	t.Helper()
	deadline, hasDeadline := t.Deadline()
	if !hasDeadline {
		return t.Context()
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline.Add(-time.Until(deadline)/10))
	t.Cleanup(cancel)
	return ctx
}

// runHelperProcess runs helper to completion and fails t with the helper's own
// output, naming a helper its context cut off apart from one that failed.
func runHelperProcess(t *testing.T, ctx context.Context, name string, helper *exec.Cmd) {
	t.Helper()
	output, err := helper.CombinedOutput()
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish before the test's deadline (%v): %v\n%s", name, ctx.Err(), err, output)
	}
	t.Fatalf("%s failed: %v\n%s", name, err, output)
}

// crashWithMappedSharedMemory leaves a registry the way a Windows crash can:
// a writer that exited without closing SQLite, its committed rows only in the
// WAL, and its -shm section still mapped with no SQLite owner. writerDelay
// holds the writer back before it writes, standing in for a slow runner.
func crashWithMappedSharedMemory(t *testing.T, writerDelay time.Duration) string {
	t.Helper()
	if root := os.Getenv("NM_DB_CRASH_WRITER"); root != "" {
		if delay, err := strconv.Atoi(os.Getenv("NM_DB_CRASH_WRITER_DELAY_MS")); err == nil {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
		database, err := Open(filepath.Join(root, "state.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		repo, err := database.InsertRepoWithID("crash-repo", root, "https://example.com/repo.git", "main")
		if err != nil {
			t.Fatal(err)
		}
		pin := &agentcfg.PiProfile{Model: "provider/crash-model", Effort: agentcfg.EffortHigh}
		if _, err := database.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature/crash", "head", "base", nil, "nonce", "generation", "digest", "", false, nil, pin); err != nil {
			t.Fatal(err)
		}
		// Exit without closing SQLite, leaving committed data in the WAL.
		os.Exit(0)
	}

	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite")
	ctx := helperProcessContext(t)
	writer := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$")
	writer.Env = append(os.Environ(), "NM_DB_CRASH_WRITER="+root, "NM_DB_CRASH_WRITER_DELAY_MS="+strconv.Itoa(int(writerDelay.Milliseconds())))
	winproc.Harden(writer)
	runHelperProcess(t, ctx, "crash writer", writer)

	// Windows can retain a mapped section after releasing a dead process's
	// locks. Keep that section alive deterministically, without a SQLite owner.
	sharedMemory, err := os.Open(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sharedMemory.Close() })
	mapping, err := windows.CreateFileMapping(windows.Handle(sharedMemory.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(mapping) })
	view, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.UnmapViewOfFile(view) })
	return path
}

func requireCrashWriterRun(t *testing.T, database *DB) {
	t.Helper()
	run, err := database.GetRunByLaunchNonce("crash-repo", "feature/crash", "nonce")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.HeadSHA != "head" || run.PiProfile == nil || run.PiProfile.Model != "provider/crash-model" || run.PiProfile.Effort != agentcfg.EffortHigh || run.LaunchReceiptClaimedAt != nil {
		t.Fatalf("persisted run or unclaimed receipt changed: %+v", run)
	}
}

func TestOpenAfterCrashWithMappedSharedMemory(t *testing.T) {
	path := crashWithMappedSharedMemory(t, 0)

	database, err := Open(path)
	if err != nil {
		t.Fatalf("writable registry after crash: %v", err)
	}
	defer database.Close()

	requireCrashWriterRun(t, database)
	if _, err := database.InsertRepoWithID("after-crash", "after-crash", "https://example.com/after.git", "main"); err != nil {
		t.Fatalf("write after crash recovery: %v", err)
	}
	repos, err := database.GetRepos()
	if err != nil || len(repos) != 2 {
		t.Fatalf("registry after crash recovery and a new write: %v, %v", repos, err)
	}
}

// Issue #251: on a slow runner the crash writer needed longer than the fixed
// 15s it was given, was killed, and the test failed on a bare exit status
// before it checked anything. The delay holds this writer past that bound.
func TestCrashWriterSlowerThanFifteenSecondsStillCompletes(t *testing.T) {
	path := crashWithMappedSharedMemory(t, 16*time.Second)

	database, err := Open(path)
	if err != nil {
		t.Fatalf("writable registry after a slow crash writer: %v", err)
	}
	defer database.Close()
	requireCrashWriterRun(t, database)
}

func TestOpenReadOnlyAfterCrashWithMappedSharedMemory(t *testing.T) {
	path := crashWithMappedSharedMemory(t, 0)

	before := make(map[string][]byte)
	for _, filename := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		before[filename] = data
	}
	if len(before[path+"-wal"]) <= 32 {
		t.Fatal("crash writer left no WAL frames")
	}

	readonly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("read-only registry after crash: %v", err)
	}
	defer readonly.Close()
	requireCrashWriterRun(t, readonly)
	if _, err := readonly.sql.Exec("DELETE FROM repos"); err == nil {
		t.Fatal("read-only registry accepted a write")
	}
	if err := readonly.Close(); err != nil {
		t.Fatal(err)
	}
	for filename, data := range before {
		after, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, after) {
			t.Fatalf("read-only inspection changed %s", filepath.Base(filename))
		}
	}
}

func TestOpenReadOnlyWhileAnotherProcessWrites(t *testing.T) {
	if path := os.Getenv("NM_DB_LIVE_READER"); path != "" {
		readonly, err := OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer readonly.Close()
		expectedWorkingPath := os.Getenv("NM_DB_LIVE_READER_EXPECTED_PATH")
		repos, err := readonly.GetRepos()
		if err != nil || len(repos) != 1 || repos[0].ID != "committed" || repos[0].WorkingPath != expectedWorkingPath {
			t.Fatalf("registry with expected working path %q: %v, %v", expectedWorkingPath, repos, err)
		}
		if _, err := readonly.sql.Exec("DELETE FROM repos"); err == nil {
			t.Fatal("read-only registry accepted a write")
		}
		return
	}

	path := filepath.Join(t.TempDir(), "state.sqlite")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.InsertRepoWithID("committed", "committed", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	transaction, err := writer.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.Exec("UPDATE repos SET working_path = ? WHERE id = ?", "updated", "committed"); err != nil {
		t.Fatal(err)
	}
	ctx := helperProcessContext(t)
	reader := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenReadOnlyWhileAnotherProcessWrites$")
	reader.Env = append(os.Environ(), "NM_DB_LIVE_READER="+path, "NM_DB_LIVE_READER_EXPECTED_PATH=committed")
	winproc.Harden(reader)
	runHelperProcess(t, ctx, "concurrent reader", reader)
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	reader = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenReadOnlyWhileAnotherProcessWrites$")
	reader.Env = append(os.Environ(), "NM_DB_LIVE_READER="+path, "NM_DB_LIVE_READER_EXPECTED_PATH=updated")
	winproc.Harden(reader)
	runHelperProcess(t, ctx, "reader after writer commits", reader)
}
