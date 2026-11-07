package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestShutdownAdmissionOrdersBothWinners(t *testing.T) {
	for _, isAdmissionFirst := range []bool{true, false} {
		name := "shutdown_first"
		if isAdmissionFirst {
			name = "admission_first"
		}
		t.Run(name, func(t *testing.T) {
			p, database := newParkedRunEnv(t)
			manager := NewRunManager(database, p, nil)
			repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			proceed := make(chan struct{})
			finished := make(chan error, 1)
			if !isAdmissionFirst {
				if err := manager.prepareShutdown(); err != nil {
					t.Fatal(err)
				}
			}
			go func() {
				_, err := manager.withBranchLock(repo.ID, "feature", func() (string, error) {
					close(entered)
					select {
					case <-proceed:
					case <-time.After(5 * time.Second):
						return "", context.DeadlineExceeded
					}
					run, err := database.InsertRun(repo.ID, "feature", "head", "base")
					if err != nil {
						return "", err
					}
					return run.ID, nil
				})
				finished <- err
			}()
			if isAdmissionFirst {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("admission did not enter")
				}
				if err := manager.prepareShutdown(); err == nil {
					t.Error("shutdown accepted an in-flight admission before its row existed")
				}
			}
			close(proceed)
			select {
			case err := <-finished:
				if isAdmissionFirst && err != nil || !isAdmissionFirst && (err == nil || !strings.Contains(err.Error(), "shutting down")) {
					t.Fatalf("admission result = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("admission did not finish")
			}
			active, err := database.GetActiveRuns()
			if err != nil {
				t.Fatal(err)
			}
			if isAdmissionFirst {
				if len(active) != 1 || active[0].Status != types.RunPending {
					t.Fatalf("winning admission was changed: %+v", active)
				}
				if err := manager.prepareShutdown(); err == nil {
					t.Fatal("shutdown accepted the pending run")
				}
			} else if len(active) != 0 {
				t.Fatalf("losing admission mutated runs: %+v", active)
			}
		})
	}
}

func TestPreparedShutdownRejectsLaunchAndResumeIPC(t *testing.T) {
	p, database := startTestDaemon(t)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var prepared ipc.ShutdownResult
	if err := client.Call(ipc.MethodPrepareShutdown, &ipc.ShutdownParams{}, &prepared); err != nil || !prepared.OK {
		t.Fatalf("prepare shutdown = %+v, %v", prepared, err)
	}
	for _, method := range []string{ipc.MethodAdmitPush, ipc.MethodPushReceived, ipc.MethodClaimLaunchReceipt, ipc.MethodRespond, ipc.MethodAnswerReview} {
		t.Run(method, func(t *testing.T) {
			err := client.Call(method, map[string]string{}, nil)
			if err == nil || !strings.Contains(err.Error(), "shutting down") {
				t.Fatalf("request after closure = %v", err)
			}
		})
	}
	active, err := database.GetActiveRuns()
	if err != nil || len(active) != 0 {
		t.Fatalf("requests after closure mutated runs: %+v, %v", active, err)
	}
}

func TestPrepareShutdownRefusesUnreadableDatabase(t *testing.T) {
	p, database := newParkedRunEnv(t)
	manager := NewRunManager(database, p, nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.prepareShutdown(); err == nil || manager.admissionClosed {
		t.Fatalf("unreadable guard = %v, closed = %t", err, manager.admissionClosed)
	}
}

func TestPrepareShutdownRefusesEveryActiveRunState(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunPending, types.RunRunning} {
		t.Run(string(status), func(t *testing.T) {
			p, database := newParkedRunEnv(t)
			manager := NewRunManager(database, p, nil)
			repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, status); err != nil {
				t.Fatal(err)
			}
			if err := manager.prepareShutdown(); err == nil || !strings.Contains(err.Error(), run.ID) {
				t.Fatalf("active run guard = %v", err)
			}
			stored, _ := database.GetRun(run.ID)
			if stored.Status != status {
				t.Fatalf("run changed: %+v", stored)
			}
		})
	}
}

func TestStopPreservesAtomicRefusalBeforeWindowsServiceAndPIDFallback(t *testing.T) {
	for _, refusal := range []string{"busy", "unsupported", "unconfirmed"} {
		t.Run(refusal, func(t *testing.T) {
			p, _ := newParkedRunEnv(t)
			defer stubServiceRuntime(t)()
			runtimeGOOS = "windows"
			serviceCommandRunner = func(string, ...string) ([]byte, error) {
				t.Error("refused stop reached Windows service command")
				return nil, nil
			}
			oldKill := daemonKillPID
			daemonKillPID = func(int) error { t.Error("refused stop reached PID kill"); return nil }
			t.Cleanup(func() { daemonKillPID = oldKill })
			server := ipc.NewServer()
			if refusal != "unsupported" {
				server.Handle(ipc.MethodPrepareShutdown, func(context.Context, json.RawMessage) (interface{}, error) {
					if refusal == "busy" {
						return nil, fmt.Errorf("refusing shutdown: run admitted after idle observation")
					}
					return &ipc.ShutdownResult{}, nil
				})
			}
			if err := server.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- server.ServeReady() }()
			t.Cleanup(func() {
				server.Close()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("fake IPC server did not stop")
				}
			})
			if err := Stop(p); err == nil {
				t.Fatal("stop accepted an unproven idle guard")
			}
		})
	}
}
