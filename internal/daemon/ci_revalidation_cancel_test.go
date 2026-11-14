package daemon

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func TestCIRevalidationCancellationCommitsHeadAndStatusTogether(t *testing.T) {
	for _, isQueued := range []bool{false, true} {
		for _, isRejected := range []bool{false, true} {
			name := map[bool]string{false: "executor-free", true: "queued"}[isQueued] + "/" + map[bool]string{false: "committed", true: "interrupted"}[isRejected]
			t.Run(name, func(t *testing.T) {
				repair := seedRetainedCIRepair(t)
				before, err := repair.fixture.db.ReadCIRepairSnapshot(repair.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				manager := NewRunManager(repair.fixture.db, repair.fixture.p, nil)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				if isQueued {
					manager.runQueue = []queuedRun{{
						runID: repair.run.ID, repoID: repair.repo.ID, branch: repair.run.Branch, recovered: &before.ExpectedRun,
						wtDir: repair.workDir, gateDir: repair.fixture.p.RepoDir(repair.repo.ID), cancel: cancel,
						agent: recoveredRunTestAgent{}, spawn: func() { t.Error("cancelled entry started") },
					}}
				}
				var raw *sql.DB
				if isRejected {
					raw, err = sql.Open("sqlite", repair.fixture.p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer raw.Close()
					if _, err := raw.Exec(`CREATE TRIGGER reject_cancellation BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled' BEGIN SELECT RAISE(ABORT, 'injected terminal failure'); END`); err != nil {
						t.Fatal(err)
					}
				}

				err = manager.HandleCancel(repair.run.ID)

				if isRejected && (err == nil || !strings.Contains(err.Error(), "injected terminal failure")) || !isRejected && err != nil {
					t.Fatalf("cancellation error = %v", err)
				}
				after, err := repair.fixture.db.ReadCIRepairSnapshot(repair.run.ID)
				if err != nil || !reflect.DeepEqual(before.Steps, after.Steps) {
					t.Fatalf("cancellation changed validation history: %v", err)
				}
				if isRejected {
					if after.ExpectedRun.Status != types.RunRunning || after.ExpectedRun.HeadSHA != before.ExpectedRun.HeadSHA || after.ExpectedRun.TerminalHeadVerifiedAt != nil || !reflect.DeepEqual(before.ExpectedRun.AwaitingAgentSince, after.ExpectedRun.AwaitingAgentSince) {
						t.Fatalf("failed cancellation changed running identity: %+v", after.ExpectedRun)
					}
					if held, err := manager.ciRepairHeld(t.Context(), &after.ExpectedRun); err != nil || !held {
						t.Fatalf("failed cancellation lost the retained hold: %v", err)
					}
					if len(manager.runQueue) != 0 || isQueued && ctx.Err() == nil {
						t.Fatal("failed cancellation left its anchored run runnable")
					}
					if _, err := raw.Exec(`DROP TRIGGER reject_cancellation`); err != nil {
						t.Fatal(err)
					}
					if err := manager.HandleCancel(repair.run.ID); err != nil {
						t.Fatalf("exact-head abort retry was refused: %v", err)
					}
					after, err = repair.fixture.db.ReadCIRepairSnapshot(repair.run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if after.ExpectedRun.Status != types.RunCancelled || after.ExpectedRun.HeadSHA != repair.candidate || after.ExpectedRun.TerminalHeadVerifiedAt == nil || len(manager.runQueue) != 0 || isQueued && ctx.Err() == nil {
					t.Fatalf("cancellation did not atomically record terminal identity: %+v", after.ExpectedRun)
				}
				if head := gitOutput(t, repair.fixture.p.RepoDir(repair.repo.ID), "rev-parse", "refs/no-mistakes/recover/"+repair.run.ID); head != repair.candidate {
					t.Fatal("cancellation failed to anchor the retained repair")
				}
			})
		}
	}
}

type cancellationCommitStep struct {
	commitContext context.Context
	started       chan struct{}
	stopping      chan struct{}
	release       <-chan struct{}
	quiesced      chan struct{}
	candidate     chan string
}

func (*cancellationCommitStep) Name() types.StepName { return types.StepDocument }

func (step *cancellationCommitStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	defer close(step.quiesced)
	close(step.started)
	<-sctx.Ctx.Done()
	close(step.stopping)
	select {
	case <-step.release:
	case <-step.commitContext.Done():
		return nil, step.commitContext.Err()
	}
	if _, err := git.Run(step.commitContext, sctx.WorkDir, "commit", "--allow-empty", "-m", "finish the in-flight writer"); err != nil {
		return nil, err
	}
	head, err := git.HeadSHA(step.commitContext, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	step.candidate <- head
	return nil, context.Cause(sctx.Ctx)
}

func TestCIRevalidationLiveAbortPreservesOnlyTheQuiescedHead(t *testing.T) {
	for _, isPreservationBlocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "unpreservable"}[isPreservationBlocked], func(t *testing.T) {
			repair := seedRetainedCIRepair(t)
			repair.adopt(t)
			gateDir := repair.fixture.p.RepoDir(repair.repo.ID)
			recoveryRef := "refs/no-mistakes/recover/" + repair.run.ID
			if isPreservationBlocked {
				lockPath := filepath.Join(gateDir, filepath.FromSlash(recoveryRef)+".lock")
				if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			run, err := repair.fixture.db.GetRun(repair.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			event, err := repair.fixture.db.GetCIRepairAdoption(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			writer := &cancellationCommitStep{
				commitContext: t.Context(), started: make(chan struct{}), stopping: make(chan struct{}),
				release: release, quiesced: make(chan struct{}), candidate: make(chan string, 1),
			}
			var steps []pipeline.Step
			for _, name := range types.AllSteps() {
				if name == types.StepDocument {
					steps = append(steps, writer)
				} else {
					steps = append(steps, &mockPassStep{name: name})
				}
			}
			manager := NewRunManager(repair.fixture.db, repair.fixture.p, nil)
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				manager.Shutdown()
			})
			manager.resumeRecoveredRun(recoveredRunPlan{
				run: run, repo: repair.repo, workDir: repair.workDir, gateDir: gateDir,
				cfg: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), agent: recoveredRunTestAgent{}, steps: steps, adoption: event,
			})
			manager.mu.Lock()
			done := manager.dones[run.ID]
			manager.mu.Unlock()
			select {
			case <-writer.started:
			case <-time.After(5 * time.Second):
				t.Fatal("adopted Document writer did not start")
			}
			before, err := repair.fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			if err := manager.HandleCancel(run.ID); err != nil {
				t.Fatalf("abort did not reach its live writer: %v", err)
			}
			select {
			case <-writer.stopping:
			case <-time.After(5 * time.Second):
				t.Fatal("live writer did not receive cancellation")
			}
			if head, exists, err := git.DirectRefTarget(t.Context(), gateDir, recoveryRef); err != nil || exists {
				t.Fatalf("abort anchored mutable source before quiescence: %s, %v", head, err)
			}
			stored, err := repair.fixture.db.GetRun(run.ID)
			if err != nil || stored.Status != types.RunRunning || stored.TerminalHeadVerifiedAt != nil {
				t.Fatalf("abort terminalized a live writer: %+v, %v", stored, err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled writer did not finish and settle")
			}
			select {
			case <-writer.quiesced:
			default:
				t.Fatal("run ended before its writer stopped")
			}
			var finalHead string
			select {
			case finalHead = <-writer.candidate:
			default:
				t.Fatal("writer never committed its final descendant")
			}
			if finalHead == repair.candidate {
				t.Fatal("fixture did not advance the head after abort")
			}
			after, err := repair.fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if isPreservationBlocked {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("unpreservable readable source lost its held run or validation results")
				}
				if observed := gitOutput(t, repair.workDir, "rev-parse", "HEAD"); observed != finalHead {
					t.Fatal("refused cancellation lost the writer's final source")
				}
			} else {
				stored := &after.ExpectedRun
				if stored.Status != types.RunCancelled || stored.HeadSHA != finalHead || stored.TerminalHeadVerifiedAt == nil || stored.AwaitingAgentSince != nil || !reflect.DeepEqual(before.Steps, after.Steps) {
					t.Fatalf("abort did not preserve the quiesced head and results: %+v", stored)
				}
				if preserved := gitOutput(t, gateDir, "rev-parse", recoveryRef); preserved != finalHead {
					t.Fatal("an obsolete recovery anchor stranded the final descendant")
				}
			}
			if !reflect.DeepEqual(before.ExpectedRun.PushGeneration, after.ExpectedRun.PushGeneration) || !reflect.DeepEqual(before.ExpectedRun.LastPushedSHA, after.ExpectedRun.LastPushedSHA) {
				t.Fatal("cancellation changed publication provenance")
			}
		})
	}
}

func TestCIRevalidationQueuedAbortOwnsPreservationDuringAdmission(t *testing.T) {
	repair := seedRetainedCIRepair(t)
	repair.adopt(t)
	binDir := t.TempDir()
	helperPath := filepath.Join(binDir, "git")
	if runtime.GOOS == "windows" {
		helperPath += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", helperPath, "../pipeline/fakecli")
	winproc.Harden(build)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake git: %v\n%s", err, output)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	readyPath, releasePath := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_CLI_MODE", "git-cancellation")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("FAKE_CLI_CANCELLATION_READY", readyPath)
	t.Setenv("FAKE_CLI_CANCELLATION_RELEASE", releasePath)
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)
	before, err := repair.fixture.db.ReadCIRepairSnapshot(repair.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(repair.fixture.db, repair.fixture.p, nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	var starts atomic.Int32
	manager.runQueue = []queuedRun{{
		runID: repair.run.ID, repoID: repair.repo.ID, branch: repair.run.Branch, recovered: &before.ExpectedRun,
		wtDir: repair.workDir, gateDir: repair.fixture.p.RepoDir(repair.repo.ID), executor: &pipeline.Executor{},
		cancel: cancel, agent: recoveredRunTestAgent{}, spawn: func() { starts.Add(1) },
	}}
	cancelResult := make(chan error, 1)
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		cancelResult <- manager.HandleCancel(repair.run.ID)
	}()
	t.Cleanup(func() {
		t.Setenv("PATH", originalPath)
		if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
			t.Error(err)
		}
		select {
		case <-cancelDone:
		case <-time.After(5 * time.Second):
			t.Error("queued cancellation did not finish")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("queued cancellation did not reach preservation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		manager.pumpQueue()
	}()
	defer func() {
		t.Setenv("PATH", originalPath)
		_ = os.WriteFile(releasePath, nil, 0o600)
		select {
		case <-pumpDone:
		case <-time.After(5 * time.Second):
			t.Error("competing admission did not finish")
		}
	}()

	select {
	case <-pumpDone:
		t.Fatal("scheduler admitted the run while cancellation owned preservation")
	case <-time.After(100 * time.Millisecond):
	}
	t.Setenv("PATH", originalPath)
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cancelResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued cancellation did not settle")
	}
	select {
	case <-pumpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not finish after cancellation")
	}
	manager.mu.Lock()
	isScheduled := manager.runScheduledLocked(repair.run.ID)
	manager.mu.Unlock()
	if starts.Load() != 0 || isScheduled || ctx.Err() == nil {
		t.Fatal("queued abort released ownership to a competing executor")
	}
	stored, err := repair.fixture.db.GetRun(repair.run.ID)
	if err != nil || stored.Status != types.RunCancelled || stored.HeadSHA != repair.candidate || stored.TerminalHeadVerifiedAt == nil {
		t.Fatalf("queued abort did not preserve and terminalize its source: %+v, %v", stored, err)
	}
	if preserved := gitOutput(t, repair.fixture.p.RepoDir(repair.repo.ID), "rev-parse", "refs/no-mistakes/recover/"+repair.run.ID); preserved != repair.candidate {
		t.Fatal("queued abort lost the head it exclusively preserved")
	}
}

// A cancellation that anchored the candidate but failed to terminalize must
// leave no writer able to start, in this daemon or the next one: a later commit
// would conflict with the immutable anchor and strand the run with no exit.
// Only an abort of the anchored head may follow, and it settles the run.
func TestCIRevalidationFailedCancellationStartsNoWriterUntilAbortSettles(t *testing.T) {
	for _, isRestarted := range []bool{false, true} {
		t.Run(map[bool]string{false: "same daemon", true: "after restart"}[isRestarted], func(t *testing.T) {
			repair := seedRetainedCIRepair(t)
			repair.adopt(t)
			gateDir := repair.fixture.p.RepoDir(repair.repo.ID)
			recoveryRef := "refs/no-mistakes/recover/" + repair.run.ID
			run, err := repair.fixture.db.GetRun(repair.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			adoption, err := repair.fixture.db.GetCIRepairAdoption(run.ID)
			if err != nil || adoption == nil {
				t.Fatalf("fixture adoption unavailable: %v", err)
			}
			if err := os.WriteFile(repair.fixture.p.ConfigFile(), []byte("max_concurrent_runs: 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			writer := &cancellationCommitStep{
				commitContext: t.Context(), started: make(chan struct{}), stopping: make(chan struct{}),
				release: release, quiesced: make(chan struct{}), candidate: make(chan string, 1),
			}
			var steps []pipeline.Step
			for _, name := range types.AllSteps() {
				if name == types.StepDocument {
					steps = append(steps, writer)
				} else {
					steps = append(steps, &mockPassStep{name: name})
				}
			}
			manager := NewRunManager(repair.fixture.db, repair.fixture.p, nil)
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				manager.mu.Lock()
				delete(manager.executors, "occupied")
				manager.mu.Unlock()
				manager.Shutdown()
			})
			manager.mu.Lock()
			manager.executors["occupied"] = &pipeline.Executor{}
			manager.mu.Unlock()
			manager.resumeRecoveredRun(recoveredRunPlan{
				run: run, repo: repair.repo, workDir: repair.workDir, gateDir: gateDir,
				cfg: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), agent: recoveredRunTestAgent{}, steps: steps, adoption: adoption,
			})
			manager.mu.Lock()
			queuedCount := len(manager.runQueue)
			manager.mu.Unlock()
			if queuedCount != 1 {
				t.Fatalf("fixture did not queue the adopted writer behind the occupied slot: %d", queuedCount)
			}
			raw, err := sql.Open("sqlite", repair.fixture.p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER reject_cancellation BEFORE UPDATE OF status ON runs WHEN NEW.status = 'cancelled' BEGIN SELECT RAISE(ABORT, 'injected terminal failure'); END`); err != nil {
				t.Fatal(err)
			}

			if err := manager.HandleCancel(run.ID); err == nil || !strings.Contains(err.Error(), "injected terminal failure") {
				t.Fatalf("fixture did not reach the failed terminal write: %v", err)
			}
			if _, err := raw.Exec(`DROP TRIGGER reject_cancellation`); err != nil {
				t.Fatal(err)
			}
			if anchor := gitOutput(t, gateDir, "rev-parse", recoveryRef); anchor != repair.candidate {
				t.Fatalf("failed cancellation anchored %s, want the candidate", anchor)
			}

			current := manager
			if isRestarted {
				current = NewRunManager(repair.fixture.db, repair.fixture.p, nil)
				t.Cleanup(current.Shutdown)
				plans, _, err := current.recoverableParkedRuns(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, plan := range plans {
					_ = plan.agent.Close()
					if plan.run.ID == run.ID {
						t.Fatal("restart planned to resume the run whose cancellation is unsettled")
					}
				}
			} else {
				manager.mu.Lock()
				delete(manager.executors, "occupied")
				manager.mu.Unlock()
				manager.pumpQueue()
			}
			current.mu.Lock()
			isScheduled := current.runScheduledLocked(run.ID)
			current.mu.Unlock()
			select {
			case <-writer.started:
				t.Fatal("a writer started after the cancellation anchored the candidate")
			default:
			}
			if isScheduled {
				t.Fatal("the run whose cancellation is unsettled still owns a queue entry or slot")
			}

			before, err := repair.fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := pipeline.CIRepairSnapshotDigest(before)
			if err != nil {
				t.Fatal(err)
			}
			server := ipc.NewServer()
			registerHandlers(server, current, repair.fixture.db, func() {})
			if err := server.Listen(repair.fixture.p.Socket()); err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- server.ServeReady() }()
			t.Cleanup(func() {
				server.Close()
				if err := <-serverDone; err != nil {
					t.Error(err)
				}
			})
			client, err := ipc.Dial(repair.fixture.p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			params := ipc.RespondParams{RunID: run.ID, Step: types.StepCI, Action: types.ActionRevalidate, CIRevalidation: &ipc.CIRevalidationParams{
				CandidateSHA: repair.candidate, CallerDir: adoption.CallerDir, CallerHeadSHA: adoption.CallerHeadSHA,
				SnapshotDigest: digest, Reason: adoption.Reason, ReselectPublication: true,
			}}
			if err := client.Call(ipc.MethodRespond, &params, nil); err == nil || !strings.Contains(err.Error(), "unsettled cancellation") {
				t.Fatalf("publication reselection over an unsettled cancellation = %v", err)
			}
			after, err := repair.fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused reselection changed the run: %v", err)
			}

			if err := current.HandleCancel(run.ID); err != nil {
				t.Fatalf("exact-head abort retry was refused: %v", err)
			}

			stored, err := repair.fixture.db.GetRun(run.ID)
			if err != nil || stored.Status != types.RunCancelled || stored.HeadSHA != repair.candidate || stored.TerminalHeadVerifiedAt == nil {
				t.Fatalf("abort retry did not settle the anchored head: %+v, %v", stored, err)
			}
			if anchor := gitOutput(t, gateDir, "rev-parse", recoveryRef); anchor != repair.candidate {
				t.Fatalf("abort retry moved the anchor to %s", anchor)
			}
			select {
			case <-writer.started:
				t.Fatal("a writer started before the cancellation settled")
			default:
			}
		})
	}
}
