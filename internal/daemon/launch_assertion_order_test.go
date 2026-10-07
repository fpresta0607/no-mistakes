package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLaunchAssertionRejectionPreservesHealthyActiveRun(t *testing.T) {
	for _, path := range []string{"start_fresh_run", "push_received"} {
		t.Run(path, func(t *testing.T) {
			for _, mismatch := range []string{"trusted_source", "effective_profile"} {
				t.Run(mismatch, func(t *testing.T) {
					var factoryCalls atomic.Int32
					step := &mockPassStep{name: types.StepReview}
					manager, root, database, repository, head, marker, global := launchAssertionFixture(t, func() []pipeline.Step {
						factoryCalls.Add(1)
						return []pipeline.Step{step}
					})
					expected := launchExpectation(head)
					if mismatch == "trusted_source" {
						commitDefaultBranchConfig(t, repository.WorkingPath, "agent: codex\n")
					} else if err := os.WriteFile(root.ConfigFile(), []byte(strings.Replace(global, "effort: xhigh", "effort: low", 1)), 0o600); err != nil {
						t.Fatal(err)
					}
					active, err := database.InsertRun(repository.ID, "feature", head, head)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					before, err := database.GetRun(active.ID)
					if err != nil {
						t.Fatal(err)
					}
					activeContext, cancel := context.WithCancelCause(context.Background())
					manager.cancels[active.ID] = cancel
					gateDir := root.RepoDir(repository.ID)
					gitCmd(t, gateDir, "update-ref", "refs/remotes/origin/main", head)
					beforeRefs := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)")
					beforeWorktrees := gitOutput(t, gateDir, "worktree", "list", "--porcelain")
					fetchHead := []byte("seeded healthy run fetch custody\n")
					fetchHeadPath := filepath.Join(gateDir, "FETCH_HEAD")
					if err := os.WriteFile(fetchHeadPath, fetchHead, 0o600); err != nil {
						t.Fatal(err)
					}

					if path == "start_fresh_run" {
						receipt, launchErr := manager.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{
							RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "reject before supersession",
							LaunchNonce: "rejected", ValidationGeneration: "generation", LaunchAssertion: expected,
						})
						err = launchErr
						if receipt.RunID != "" {
							t.Errorf("rejected launch returned a receipt: %+v", receipt)
						}
					} else {
						runID, launchErr := manager.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
							Gate: gateDir, Ref: "refs/heads/feature", Old: head, New: head, Intent: "reject before supersession",
							LaunchNonce: "rejected", ValidationGeneration: "generation", LaunchAssertion: expected,
						})
						err = launchErr
						if runID != "" {
							t.Errorf("rejected push returned a run: %s", runID)
						}
					}
					if err == nil || !strings.Contains(err.Error(), "launch assertion") {
						t.Fatalf("assertion mismatch was not refused: %v", err)
					}
					if cause := context.Cause(activeContext); cause != nil {
						t.Errorf("assertion rejection cancelled healthy active run: %v", cause)
					}
					runs, err := database.GetRunsByRepo(repository.ID)
					if err != nil || len(runs) != 1 || !reflect.DeepEqual(runs[0], before) {
						t.Errorf("assertion rejection changed run custody: %+v, error=%v", runs, err)
					}
					if run, err := database.GetRunByLaunchNonce(repository.ID, "feature", "rejected"); err != nil || run != nil {
						t.Errorf("assertion rejection created nonce custody: %+v, error=%v", run, err)
					}
					if rows, err := database.GetStepsByRun(active.ID); err != nil || len(rows) != 0 {
						t.Errorf("assertion rejection created step rows: %+v, error=%v", rows, err)
					}
					if factoryCalls.Load() != 0 || step.execCnt.Load() != 0 || len(manager.executors) != 0 || len(manager.cancels) != 1 {
						t.Error("assertion rejection started pipeline work")
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Errorf("assertion rejection entered agent fixture: %v", err)
					}
					if refs := gitOutput(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)"); refs != beforeRefs {
						t.Errorf("assertion rejection changed Git refs: before=%s after=%s", beforeRefs, refs)
					}
					if worktrees := gitOutput(t, gateDir, "worktree", "list", "--porcelain"); worktrees != beforeWorktrees {
						t.Errorf("assertion rejection changed worktree custody: %s", worktrees)
					}
					if after, err := os.ReadFile(fetchHeadPath); err != nil || !bytes.Equal(after, fetchHead) {
						t.Errorf("assertion rejection changed FETCH_HEAD: %q, error=%v", after, err)
					}
				})
			}
		})
	}
}

func TestLaunchAssertionObserverClaimsDuringBlockedSetup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var factoryCalls atomic.Int32
	step := &mockPassStep{name: types.StepReview}
	manager, _, database, repository, head, marker, _ := launchAssertionFixture(t, func() []pipeline.Step {
		factoryCalls.Add(1)
		close(entered)
		<-release
		return []pipeline.Step{step}
	})
	socketRoot, err := os.MkdirTemp("", "nm-claim-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	socket := paths.WithRoot(socketRoot).Socket()
	server := ipc.NewServer()
	registerHandlers(server, manager, database, func() {})
	if err := server.Listen(socket); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeReady() }()
	defer func() { server.Close(); <-serverDone }()
	defer unblock()
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	expected := launchExpectation(head)
	const intent = "observer claims the asserted setup"
	var fresh ipc.StartFreshRunResult
	done := make(chan error, 1)
	go func() {
		done <- client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{
			RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: intent,
			LaunchNonce: "blocked-setup", ValidationGeneration: "generation", LaunchAssertion: expected,
		}, &fresh)
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("launch returned before blocked setup: %v", err)
	}
	observer, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	claim := &ipc.ClaimLaunchReceiptParams{
		RepoID: repository.ID, Branch: "feature", SubmittedHeadSHA: head, IntentDigest: digestIntent(intent),
		LaunchNonce: "blocked-setup", ValidationGeneration: "generation", LaunchAssertion: expected,
	}
	changed := launchExpectation(head)
	changed.Profiles["primary"][0].Model = "conflicting"
	claim.LaunchAssertion = changed
	var refused ipc.ClaimLaunchReceiptResult
	if err := observer.Call(ipc.MethodClaimLaunchReceipt, claim, &refused); err == nil || !strings.Contains(err.Error(), "captured assertion differs") {
		t.Fatalf("conflicting observer did not receive the assertion refusal during blocked setup: %v", err)
	}
	stored, err := database.GetRunByLaunchNonce(repository.ID, "feature", "blocked-setup")
	if err != nil || stored == nil || stored.LaunchReceiptClaimedAt != nil || stored.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("conflicting observer changed custody: %+v, error=%v", stored, err)
	}
	claim.LaunchAssertion = expected
	var first, replay ipc.ClaimLaunchReceiptResult
	if err := observer.Call(ipc.MethodClaimLaunchReceipt, claim, &first); err != nil {
		t.Fatal(err)
	}
	if err := observer.Call(ipc.MethodClaimLaunchReceipt, claim, &replay); err != nil {
		t.Fatal(err)
	}
	if first.Receipt == nil || replay.Receipt == nil || first.Receipt.Disposition != "created" || replay.Receipt.Disposition != "reused" || first.Receipt.RunID != stored.ID || replay.Receipt.RunID != stored.ID || first.Receipt.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("blocked setup receipt claims = %+v, %+v", first, replay)
	}
	if runs, err := database.GetRunsByRepo(repository.ID); err != nil || len(runs) != 1 || factoryCalls.Load() != 1 || step.execCnt.Load() != 0 {
		t.Fatalf("observer started another launch or pipeline: %+v, error=%v", runs, err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fresh.Receipt.RunID != stored.ID || fresh.Receipt.Disposition != "reused" || fresh.Receipt.LaunchAssertionProof.Check(expected) != nil {
		t.Fatalf("fallback receipt = %+v", fresh.Receipt)
	}
	if run := waitForRunTerminalState(t, database, stored.ID); run.Status != types.RunCompleted || run.LaunchReceiptClaimedAt == nil || step.execCnt.Load() != 1 || factoryCalls.Load() != 1 {
		t.Fatalf("claimed setup run = %+v", run)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("observer entered an agent fixture: %v", err)
	}
}
