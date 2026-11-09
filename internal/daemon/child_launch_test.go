package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
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
	repoID   string
	calls    *atomic.Int32
	evidence *atomic.Value
	launch   func() (runID string, isStarted bool, err error)
}

// childLaunches are the paths that start a run without going through startRun,
// so every rule startRun applies to a new run has to hold on them as well.
var childLaunches = map[string]func(*testing.T, launchConfigurer) childLaunchFixture{
	"authored": authoredChildLaunch,
	"terminal": terminalChildLaunch,
}

// launchConfigurer returns the launch a parent run asserts and the default
// branch commit a child's trusted config is read from.
type launchConfigurer func(t *testing.T, root *paths.Paths, repo *db.Repo, head, global string) (*launchassert.Expectation, string)

func assertedLaunch(_ *testing.T, _ *paths.Paths, _ *db.Repo, head, _ string) (*launchassert.Expectation, string) {
	return launchExpectation(head), head
}

// launchRefusedByItsAgent passes the launch proof and fails in agent
// construction: the operator's args bring project settings back to claude, and
// the trusted default branch has since turned them off.
func launchRefusedByItsAgent(t *testing.T, root *paths.Paths, repo *db.Repo, head, global string) (*launchassert.Expectation, string) {
	t.Helper()
	claudeConfig := strings.NewReplacer(
		"agent: codex", "agent: claude",
		"  codex: {model: fixture, effort: xhigh}", "  claude: {model: fixture, effort: xhigh}",
		"  codex: ['-c', 'service_tier=\"default\"']", "  claude: ['--setting-sources', 'project,user']",
	).Replace(global)
	if err := os.WriteFile(root.ConfigFile(), []byte(claudeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := []agentcfg.Selection{{Harness: types.AgentClaude, Model: "fixture", Effort: agentcfg.EffortXHigh}}
	assertion := &launchassert.Expectation{TrustedSHA: head, Profiles: map[string][]agentcfg.Selection{"primary": profile, "reviewer": profile, "fixer": profile}}
	return assertion, commitDefaultBranchConfig(t, repo.WorkingPath, "disable_project_settings: true\n")
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

// pinIntentCheck records the policy snapshot startRun gives every real run, so
// a fixture's parent has the same row a real parent does.
func pinIntentCheck(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	policy, err := json.Marshal(config.IntentCheck{Mode: config.IntentCheckOff})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunIntentCheck(runID, string(policy)); err != nil {
		t.Fatal(err)
	}
}

func authoredChildLaunch(t *testing.T, configure launchConfigurer) childLaunchFixture {
	t.Helper()
	calls, evidence := &atomic.Int32{}, &atomic.Value{}
	manager, database, repo, request, _ := authoredAdmissionFixtureFor(t, childLaunchFactory(calls, evidence), configure)
	return childLaunchFixture{manager: manager, database: database, repoID: repo.ID, calls: calls, evidence: evidence, launch: func() (string, bool, error) {
		result, err := manager.validateAuthored(context.Background(), request, os.Getpid())
		if err != nil {
			return "", false, err
		}
		return result.Receipt.RunID, result.Started, nil
	}}
}

func terminalChildLaunch(t *testing.T, configure launchConfigurer) childLaunchFixture {
	t.Helper()
	calls, evidence := &atomic.Int32{}, &atomic.Value{}
	manager, p, database, repo, head, _, global := launchAssertionFixture(t, childLaunchFactory(calls, evidence))
	assertion, _ := configure(t, p, repo, head, global)
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature", head)
	gate := p.RepoDir(repo.ID)
	gitCmd(t, gate, "update-ref", "refs/heads/feature", head)
	proof, err := assertion.Verify(head, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", head, head, nil, "nonce", "ordinary2", "intent", "", false, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	pinIntentCheck(t, database, run.ID)
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
	return childLaunchFixture{manager: manager, database: database, repoID: repo.ID, calls: calls, evidence: evidence, launch: func() (string, bool, error) {
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
				fixture := build(t, assertedLaunch)
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
			fixture := build(t, assertedLaunch)
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

// Agent construction holds fail-closed launch checks the proof does not cover.
// A launch it refuses records nothing, because an accepted child would hold the
// branch for a run that never started.
func TestChildLaunchRefusedByItsAgentRecordsNothing(t *testing.T) {
	for name, build := range childLaunches {
		t.Run(name, func(t *testing.T) {
			fixture := build(t, launchRefusedByItsAgent)

			runID, isStarted, err := fixture.launch()

			if err == nil || !strings.Contains(err.Error(), "does not neutralize") || isStarted {
				t.Fatalf("launch = %q, %v, %v; want the agent's refusal", runID, isStarted, err)
			}
			runs, err := fixture.database.GetRunsByRepo(fixture.repoID)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 {
				t.Fatalf("refused launch left %d runs on the repository, want only the parent", len(runs))
			}
		})
	}
}

// A child is a new run, so its evidence goes under evidence_dir like any other.
func TestChildRunKeepsEvidenceUnderEvidenceDir(t *testing.T) {
	for name, build := range childLaunches {
		t.Run(name, func(t *testing.T) {
			fixture := build(t, assertedLaunch)
			root, err := realpath.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
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
