package db

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func TestOpenReadOnlyAfterCrashWithMappedSharedMemory(t *testing.T) {
	if root := os.Getenv("NM_DB_CRASH_WRITER"); root != "" {
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	writer := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenReadOnlyAfterCrashWithMappedSharedMemory$")
	writer.Env = append(os.Environ(), "NM_DB_CRASH_WRITER="+root)
	winproc.Harden(writer)
	if output, err := writer.CombinedOutput(); err != nil {
		t.Fatalf("crash writer: %v\n%s", err, output)
	}

	// Windows can retain a mapped section after releasing a dead process's
	// locks. Keep that section alive deterministically, without a SQLite owner.
	sharedMemory, err := os.Open(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	defer sharedMemory.Close()
	mapping, err := windows.CreateFileMapping(windows.Handle(sharedMemory.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(mapping)
	view, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(view)

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
	run, err := readonly.GetRunByLaunchNonce("crash-repo", "feature/crash", "nonce")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.HeadSHA != "head" || run.PiProfile == nil || run.PiProfile.Model != "provider/crash-model" || run.PiProfile.Effort != agentcfg.EffortHigh || run.LaunchReceiptClaimedAt != nil {
		t.Fatalf("persisted run or unclaimed receipt changed: %+v", run)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenReadOnlyWhileAnotherProcessWrites$")
	reader.Env = append(os.Environ(), "NM_DB_LIVE_READER="+path, "NM_DB_LIVE_READER_EXPECTED_PATH=committed")
	winproc.Harden(reader)
	if output, err := reader.CombinedOutput(); err != nil {
		t.Fatalf("concurrent reader: %v\n%s", err, output)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	reader = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenReadOnlyWhileAnotherProcessWrites$")
	reader.Env = append(os.Environ(), "NM_DB_LIVE_READER="+path, "NM_DB_LIVE_READER_EXPECTED_PATH=updated")
	winproc.Harden(reader)
	if output, err := reader.CombinedOutput(); err != nil {
		t.Fatalf("reader after writer commits: %v\n%s", err, output)
	}
}
