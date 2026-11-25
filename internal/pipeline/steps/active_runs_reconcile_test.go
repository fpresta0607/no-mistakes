package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type reconcileEnvStep struct {
	step pipeline.Step
	env  []string
}

func (s reconcileEnvStep) Name() types.StepName { return s.step.Name() }
func (s reconcileEnvStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx.Env = s.env
	return s.step.Execute(ctx)
}
func (s reconcileEnvStep) ReconcileApprovalGate(ctx *pipeline.StepContext) (bool, error) {
	ctx.Env = s.env
	return s.step.(pipeline.ApprovalGateReconciler).ReconcileApprovalGate(ctx)
}

func TestCIGateReconciliationClearsActiveRunAfterPRBecomesTerminal(t *testing.T) {
	t.Parallel()
	for _, terminalState := range []string{"MERGED", "CLOSED"} {
		t.Run(terminalState, func(t *testing.T) {
			database, p, run, repo, dir, statePath, env := setupCIGateReconcileTest(t)
			gate := runToCIGate(t, database, p, run, repo, dir, env)
			if err := os.WriteFile(statePath, []byte(terminalState+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := <-gate.done; err != nil {
				t.Fatalf("%s PR did not reconcile: Execute() error = %v", terminalState, err)
			}

			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			active, err := lifecycle.ActiveRuns(p)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != types.RunCompleted || persisted.AwaitingAgentSince != nil || len(active) != 0 {
				t.Fatalf("terminal PR reconciliation: status=%s awaiting=%v active=%d", persisted.Status, persisted.AwaitingAgentSince, len(active))
			}
		})
	}
}

func TestCIGateReconciliationPreservesOpenErrorAndUnknownStates(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"OPEN", "ERROR", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			database, p, run, repo, dir, statePath, env := setupCIGateReconcileTest(t)
			gate := runToCIGate(t, database, p, run, repo, dir, env)
			if err := os.WriteFile(statePath, []byte(state+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(60 * time.Millisecond)

			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			active, err := lifecycle.ActiveRuns(p)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != types.RunRunning || persisted.AwaitingAgentSince == nil || len(active) != 1 {
				t.Fatalf("state %s did not fail closed: status=%s awaiting=%v active=%d", state, persisted.Status, persisted.AwaitingAgentSince, len(active))
			}
			if err := gate.exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			if err := <-gate.done; err != nil {
				t.Fatalf("approval did not finish preserved gate: Execute() error = %v", err)
			}
		})
	}
}

// A test that ends while its run is parked, as one that fails an assertion
// does, must not leave that run using the store its temporary directory is
// about to remove: Windows refuses to delete an open state.sqlite (issue #265).
func TestCIGateRunStopsBeforeItsStoreIsRemoved(t *testing.T) {
	t.Parallel()
	var done <-chan error
	var storePath string
	t.Run("ends at the gate", func(t *testing.T) {
		database, p, run, repo, dir, _, env := setupCIGateReconcileTest(t)
		done = runToCIGate(t, database, p, run, repo, dir, env).done
		storePath = p.DB()
	})
	select {
	case err := <-done:
		if !errors.Is(err, pipeline.ErrParkedAtShutdown) {
			t.Fatalf("Execute() error = %v, want ErrParkedAtShutdown from the fixture stopping the parked run", err)
		}
	default:
		t.Fatal("the run was still executing after its test ended")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("the store outlived its test's temporary directory: %v", err)
	}
}

func setupCIGateReconcileTest(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo, string, string, []string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ensureHermeticOrigin(t, dir)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}

	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	statePath := filepath.Join(t.TempDir(), "pr-state")
	if err := os.WriteFile(statePath, []byte("OPEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":        "ci-gh-reconcile",
		"FAKE_CLI_STATE_PATH":  statePath,
		"FAKE_CLI_PR_HEAD_SHA": "deadbeef",
	})
	return database, p, run, repo, dir, statePath, env
}

// ciGateRun is a PR and CI run that runToCIGate parked at its CI gate.
type ciGateRun struct {
	exec *pipeline.Executor
	done <-chan error
}

// runToCIGate runs the PR and CI steps over the fixture and returns once the
// CI step parks at its approval gate. It waits on the executor's park event or
// on Execute returning, never on a wall-clock deadline: the two steps start
// many git and gh processes, which a loaded Windows runner has taken longer
// than a fixed 15 s wait to finish (issue #265), and a real hang still
// surfaces as the package's go test -timeout with every goroutine's stack.
// The run is stopped, and Execute has returned, before the fixture closes its
// store and removes its directory, because a run left parked keeps
// state.sqlite open and Windows refuses to delete it.
func runToCIGate(t *testing.T, database *db.DB, p *paths.Paths, run *db.Run, repo *db.Repo, dir string, env []string) ciGateRun {
	t.Helper()
	parked := make(chan struct{}, 1)
	emit := func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepCI && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			select {
			case parked <- struct{}{}:
			default:
			}
		}
	}
	exec := pipeline.NewExecutor(database, p, &config.Config{Agent: types.AgentClaude, CITimeout: 80 * time.Millisecond}, &mockAgent{name: "test"}, []pipeline.Step{
		reconcileEnvStep{step: &PRStep{}, env: env},
		reconcileEnvStep{step: &CIStep{pollIntervalOverride: 10 * time.Millisecond}, env: env},
	}, emit)
	exec.SetGateReconcileTimings(20*time.Millisecond, 5*time.Second)

	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- exec.Execute(ctx, run, repo, dir)
	}()
	t.Cleanup(func() {
		cancel(pipeline.ErrDaemonShutdown)
		<-finished
	})
	select {
	case <-parked:
	case err := <-done:
		t.Fatalf("CI step did not reach awaiting_approval: Execute() returned %v", err)
	}
	return ciGateRun{exec: exec, done: done}
}
