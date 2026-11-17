package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// handbackParkingTestStep parks a Test failure with automatic repair disabled
// and counts every handback the daemon delivers to it.
type handbackParkingTestStep struct {
	handbacks atomic.Int32
}

func (s *handbackParkingTestStep) Name() types.StepName { return types.StepTest }

func (s *handbackParkingTestStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"command failed"}]}`}, nil
}

func (s *handbackParkingTestStep) HandbackStep(sctx *pipeline.StepContext, request ipc.StepHandbackParams) (*ipc.StepHandbackResult, error) {
	s.handbacks.Add(1)
	return &ipc.StepHandbackResult{RunID: sctx.Run.ID, Step: types.StepTest, ReceiptID: "receipt", CallerDir: request.CallerDir, ReleasedHead: sctx.Run.HeadSHA}, nil
}

type stepHandbackDaemonFixture struct {
	manager  *RunManager
	client   *ipc.Client
	repo     *db.Repo
	run      *db.Run
	step     *handbackParkingTestStep
	executor *pipeline.Executor
}

// setupStepHandbackDaemon parks a live Test gate behind the daemon's real IPC
// handlers, so a handback request carries the OS-authenticated peer.
func setupStepHandbackDaemon(t *testing.T) *stepHandbackDaemonFixture {
	t.Helper()
	root := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head-1", "base-1")
	if err != nil {
		t.Fatal(err)
	}
	step := &handbackParkingTestStep{}
	executor := pipeline.NewExecutor(database, root, &config.Config{}, nil, []pipeline.Step{step}, nil)
	manager := NewRunManager(database, root, nil)
	manager.mu.Lock()
	manager.executors[run.ID] = executor
	manager.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), run, repo, t.TempDir()) }()
	t.Cleanup(func() {
		_ = executor.Respond(types.StepTest, types.ActionAbort, nil)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("parked executor never finished")
		}
	})
	for deadline := time.Now().Add(10 * time.Second); !executor.AwaitingApproval(); {
		if time.Now().After(deadline) {
			t.Fatal("Test gate never parked")
		}
		time.Sleep(10 * time.Millisecond)
	}

	socketRoot, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketRoot) })
	socket := paths.WithRoot(socketRoot).Socket()
	server := ipc.NewServer()
	registerHandlers(server, manager, database, func() {})
	if err := server.Listen(socket); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeReady() }()
	t.Cleanup(func() { server.Close(); <-served })
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &stepHandbackDaemonFixture{manager: manager, client: client, repo: repo, run: run, step: step, executor: executor}
}

func (f *stepHandbackDaemonFixture) params(t *testing.T) ipc.StepHandbackParams {
	return ipc.StepHandbackParams{RepoID: f.repo.ID, RunID: f.run.ID, Branch: f.run.Branch, CallerDir: t.TempDir(), HeadSHA: f.run.HeadSHA}
}

func TestHandleStepHandbackReleasesTheLatestRunsParkedGate(t *testing.T) {
	// Arrange
	fixture := setupStepHandbackDaemon(t)
	params := fixture.params(t)

	// Act
	var result ipc.StepHandbackResult
	err := fixture.client.Call(ipc.MethodStepHandback, params, &result)

	// Assert
	if err != nil {
		t.Fatalf("handback: %v", err)
	}
	if result.ReceiptID != "receipt" || result.RunID != fixture.run.ID || result.CallerDir != params.CallerDir {
		t.Fatalf("handback result = %+v", result)
	}
	if fixture.step.handbacks.Load() != 1 || !fixture.executor.AwaitingApproval() {
		t.Fatalf("parked step received %d handbacks, waiting %t; want one release and the gate still parked", fixture.step.handbacks.Load(), fixture.executor.AwaitingApproval())
	}
}

func TestHandleStepHandbackRefusesARequestItCannotRouteToTheParkedGate(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, fixture *stepHandbackDaemonFixture) ipc.StepHandbackParams
		refusal string
	}{
		{"not the latest run on the branch", func(t *testing.T, fixture *stepHandbackDaemonFixture) ipc.StepHandbackParams {
			if _, err := fixture.manager.db.InsertRun(fixture.repo.ID, fixture.run.Branch, "head-2", "base-1"); err != nil {
				t.Fatal(err)
			}
			return fixture.params(t)
		}, "latest run"},
		{"relative caller directory", func(t *testing.T, fixture *stepHandbackDaemonFixture) ipc.StepHandbackParams {
			params := fixture.params(t)
			params.CallerDir = filepath.Join("relative", "caller")
			return params
		}, "absolute caller directory"},
		{"no executor for the run", func(t *testing.T, fixture *stepHandbackDaemonFixture) ipc.StepHandbackParams {
			newer, err := fixture.manager.db.InsertRun(fixture.repo.ID, fixture.run.Branch, "head-2", "base-1")
			if err != nil {
				t.Fatal(err)
			}
			params := fixture.params(t)
			params.RunID = newer.ID
			return params
		}, "no active executor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			fixture := setupStepHandbackDaemon(t)
			params := test.prepare(t, fixture)

			// Act
			var result ipc.StepHandbackResult
			err := fixture.client.Call(ipc.MethodStepHandback, params, &result)

			// Assert
			if err == nil || !strings.Contains(err.Error(), test.refusal) {
				t.Fatalf("handback = %+v, %v; want a refusal naming %q", result, err, test.refusal)
			}
			if fixture.step.handbacks.Load() != 0 || !fixture.executor.AwaitingApproval() {
				t.Fatalf("refused request reached the step %d times, waiting %t", fixture.step.handbacks.Load(), fixture.executor.AwaitingApproval())
			}
			stored, err := fixture.manager.db.GetRun(fixture.run.ID)
			if err != nil || stored.HeadSHA != fixture.run.HeadSHA {
				t.Fatalf("run = %+v, %v; want its head %s unchanged", stored, err, fixture.run.HeadSHA)
			}
		})
	}
}
