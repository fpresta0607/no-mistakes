package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// childLaunchStep passes and records what the pipeline handed it.
type childLaunchStep struct {
	name        types.StepName
	calls       *atomic.Int32
	evidenceDir *atomic.Value
}

func (s *childLaunchStep) Name() types.StepName { return s.name }

func (s *childLaunchStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls.Add(1)
	s.evidenceDir.Store(sctx.EvidenceDir)
	return &pipeline.StepOutcome{}, nil
}

type childLaunchFixture struct {
	manager  *RunManager
	database *db.DB
	calls    *atomic.Int32
	evidence *atomic.Value
	launch   func() (runID string, isStarted bool, err error)
}

// childLaunches are the paths that start a run without going through startRun,
// so every rule startRun applies to a new run has to hold on them as well.
var childLaunches = map[string]func(*testing.T) childLaunchFixture{
	"authored": authoredChildLaunch,
	"terminal": terminalChildLaunch,
}

func childLaunchFactory(calls *atomic.Int32, evidence *atomic.Value) StepFactory {
	return func() []pipeline.Step {
		var sequence []pipeline.Step
		for _, name := range types.AllSteps() {
			sequence = append(sequence, &childLaunchStep{name: name, calls: calls, evidenceDir: evidence})
		}
		return sequence
	}
}

func authoredChildLaunch(t *testing.T) childLaunchFixture {
	t.Helper()
	calls, evidence := &atomic.Int32{}, &atomic.Value{}
	manager, database, _, request, _ := authoredAdmissionFixture(t, childLaunchFactory(calls, evidence))
	return childLaunchFixture{manager: manager, database: database, calls: calls, evidence: evidence, launch: func() (string, bool, error) {
		result, err := manager.validateAuthored(context.Background(), request, os.Getpid())
		if err != nil {
			return "", false, err
		}
		return result.Receipt.RunID, result.Started, nil
	}}
}

func terminalChildLaunch(t *testing.T) childLaunchFixture {
	t.Helper()
	calls, evidence := &atomic.Int32{}, &atomic.Value{}
	manager, p, database, repo, head, _, _ := launchAssertionFixture(t, childLaunchFactory(calls, evidence))
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	gate := p.RepoDir(repo.ID)
	gitCmd(t, gate, "update-ref", "refs/heads/feature", head)
	assertion := launchExpectation(head)
	proof, err := assertion.Verify(head, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", head, head, nil, "nonce", "ordinary2", "intent", "", false, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	managed := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), gate, managed, head); err != nil {
		t.Fatal(err)
	}
	if err := git.CopyLocalUserIdentity(context.Background(), repo.WorkingPath, managed); err != nil {
		t.Fatal(err)
	}
	for _, name := range types.AllSteps() {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, "original.log"); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunErrorStatusWithVerifiedHead(run.ID, "interrupted", types.RunFailed, head); err != nil {
		t.Fatal(err)
	}
	observed, err := database.ReadContinuationSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "correction.txt"), []byte("correction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, managed, "add", "correction.txt")
	gitCmd(t, managed, "commit", "-m", "correction")
	request := ipc.RecoverTerminalParams{RepoID: repo.ID, RunID: run.ID, Branch: run.Branch, CallerDir: repo.WorkingPath, SnapshotDigest: observed.Digest(), Reason: "continue from the correction"}
	return childLaunchFixture{manager: manager, database: database, calls: calls, evidence: evidence, launch: func() (string, bool, error) {
		result, err := manager.recoverTerminal(context.Background(), request, os.Getpid())
		if err != nil {
			return "", false, err
		}
		return result.RunID, result.Started, nil
	}}
}

func appendGlobalConfig(t *testing.T, manager *RunManager, lines string) {
	t.Helper()
	content, err := os.ReadFile(manager.paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.paths.ConfigFile(), append(content, []byte(lines)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func queuedEntries(manager *RunManager, runID string) int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	count := 0
	for _, entry := range manager.runQueue {
		if entry.runID == runID {
			count++
		}
	}
	return count
}

// A child waiting for an execution slot is already scheduled, so a replay only
// observes it. A second queue entry would start after the first finished, fail
// its pending check, and record the finished child as failed. A replay starts
// nothing, so a pending restart does not refuse it either.
func TestChildReplayLeavesAQueuedChildToTheQueue(t *testing.T) {
	for name, build := range childLaunches {
		for _, isRestartPending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart_pending=%t", name, isRestartPending), func(t *testing.T) {
				fixture := build(t)
				manager := fixture.manager
				appendGlobalConfig(t, manager, "max_concurrent_runs: 1\n")
				const heldSlot = "held-slot"
				manager.mu.Lock()
				manager.executors[heldSlot] = &pipeline.Executor{}
				manager.mu.Unlock()

				runID, isStarted, err := fixture.launch()
				if err != nil || !isStarted {
					t.Fatalf("first launch = %q, %v, %v", runID, isStarted, err)
				}
				manager.restart.pending.Store(isRestartPending)
				replayID, isReplayStarted, replayErr := fixture.launch()
				manager.restart.pending.Store(false)
				entries := queuedEntries(manager, runID)
				manager.mu.Lock()
				delete(manager.executors, heldSlot)
				manager.mu.Unlock()
				manager.pumpQueue()

				if replayErr != nil || isReplayStarted || replayID != runID {
					t.Fatalf("replay = %q, %v, %v; want it to observe %q", replayID, isReplayStarted, replayErr, runID)
				}
				if entries != 1 {
					t.Fatalf("queue held %d entries for the child, want 1", entries)
				}
				child := waitForRunTerminalState(t, fixture.database, runID)
				if child.Status != types.RunCompleted || fixture.calls.Load() != int32(len(types.AllSteps())) {
					reason := ""
					if child.Error != nil {
						reason = *child.Error
					}
					t.Fatalf("child = %s %q after %d step calls", child.Status, reason, fixture.calls.Load())
				}
			})
		}
	}
}

// Like every launch but a plain push, a child is refused while an update
// restart waits for the daemon to go idle, so it cannot run on the old binary.
func TestChildLaunchIsRefusedWhileARestartIsPending(t *testing.T) {
	for name, build := range childLaunches {
		t.Run(name, func(t *testing.T) {
			fixture := build(t)
			fixture.manager.restart.pending.Store(true)

			runID, isStarted, err := fixture.launch()

			if !errors.Is(err, errRestartPending) || isStarted {
				t.Fatalf("launch = %q, %v, %v; want the restart refusal", runID, isStarted, err)
			}
			if fixture.calls.Load() != 0 {
				t.Fatalf("a refused launch ran %d steps", fixture.calls.Load())
			}
		})
	}
}

// A child is a new run, so its evidence goes under evidence_dir like any other.
func TestChildRunKeepsEvidenceUnderEvidenceDir(t *testing.T) {
	for name, build := range childLaunches {
		t.Run(name, func(t *testing.T) {
			fixture := build(t)
			root := t.TempDir()
			appendGlobalConfig(t, fixture.manager, "evidence_dir: "+filepath.ToSlash(root)+"\n")

			runID, isStarted, err := fixture.launch()
			if err != nil || !isStarted {
				t.Fatalf("launch = %q, %v, %v", runID, isStarted, err)
			}
			child := waitForRunTerminalState(t, fixture.database, runID)

			want := filepath.Join(root, runID)
			if child.EvidencePath() != want || fixture.evidence.Load() != want {
				t.Fatalf("pinned %q, step used %v; want %q", child.EvidencePath(), fixture.evidence.Load(), want)
			}
		})
	}
}
