package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// serveTestIPC serves server on a short socket path for the rest of the test.
func serveTestIPC(t *testing.T, server *ipc.Server) string {
	t.Helper()
	socketRoot, err := os.MkdirTemp("", "nm-hangup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socket := paths.WithRoot(socketRoot).Socket()
	if err := server.Listen(socket); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeReady() }()
	t.Cleanup(func() { server.Close(); <-served })
	return socket
}

// An admission whose client gave up while it waited for its branch lock used to
// create its run anyway once the lock came free, then fail writing the reply, so
// the client could not tell whether it had happened and a retry superseded the
// run it never heard of (#249). It now stops before it supersedes or creates
// anything, and a retry with the same launch nonce creates the one run.
func TestAdmissionWhoseClientHungUpCreatesNoRunAndARetryCreatesOne(t *testing.T) {
	for _, method := range []string{ipc.MethodStartFreshRun, ipc.MethodPushReceived} {
		t.Run(method, func(t *testing.T) {
			// Arrange
			step := &mockPassStep{name: types.StepReview}
			manager, root, database, repository, head, _, _ := launchAssertionFixture(t, func() []pipeline.Step { return []pipeline.Step{step} })
			const nonce, generation, intent = "hung-up", "generation", "admitted after its client left"
			admit := func(ctx context.Context) (string, error) {
				if method == ipc.MethodStartFreshRun {
					receipt, err := manager.HandleStartFreshRun(ctx, &ipc.StartFreshRunParams{
						RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: intent,
						LaunchNonce: nonce, ValidationGeneration: generation,
					})
					return receipt.RunID, err
				}
				return manager.HandlePushReceived(ctx, &ipc.PushReceivedParams{
					Gate: root.RepoDir(repository.ID), Ref: "refs/heads/feature", Old: head, New: head, Intent: intent,
					LaunchNonce: nonce, ValidationGeneration: generation,
				})
			}
			type outcome struct {
				runID string
				err   error
			}
			admitted := make(chan context.Context, 1)
			outcomes := make(chan outcome, 1)
			server := ipc.NewServer()
			server.Handle(method, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
				admitted <- ctx
				runID, err := admit(ctx)
				outcomes <- outcome{runID: runID, err: err}
				return map[string]string{"run_id": runID}, err
			})
			socket := serveTestIPC(t, server)
			branchLock := manager.branchMutex(repository.ID, "feature")
			branchLock.Lock()
			isLocked := true
			t.Cleanup(func() {
				if isLocked {
					branchLock.Unlock()
				}
			})
			before, err := database.GetRunsByRepo(repository.ID)
			if err != nil {
				t.Fatal(err)
			}
			client, err := ipc.Dial(socket)
			if err != nil {
				t.Fatal(err)
			}

			// Act
			gaveUp := client.CallWithTimeout(method, struct{}{}, nil, 200*time.Millisecond)
			_ = client.Close()
			admission := <-admitted
			for wait := time.Now().Add(10 * time.Second); !ipc.ClientGone(admission) && time.Now().Before(wait); {
				time.Sleep(5 * time.Millisecond)
			}
			if !ipc.ClientGone(admission) {
				t.Fatal("the server never saw the client hang up")
			}
			branchLock.Unlock()
			isLocked = false
			stopped := <-outcomes

			// Assert
			if !ipc.IsCallTimeout(gaveUp) {
				t.Fatalf("client call = %v, want it to give up waiting", gaveUp)
			}
			if !errors.Is(stopped.err, errAdmissionClientGone) || stopped.runID != "" {
				t.Fatalf("admission after the client left = (%q, %v), want a stop with no run", stopped.runID, stopped.err)
			}
			after, err := database.GetRunsByRepo(repository.ID)
			if err != nil || len(after) != len(before) {
				t.Fatalf("an admission whose client left created a run: %d runs before, %d after, err=%v", len(before), len(after), err)
			}
			if run, err := database.GetRunByLaunchNonce(repository.ID, "feature", nonce); err != nil || run != nil {
				t.Fatalf("an admission whose client left bound its nonce: %+v, err=%v", run, err)
			}
			retryRunID, err := admit(context.Background())
			if err != nil || retryRunID == "" {
				t.Fatalf("retry with the same nonce = (%q, %v), want the run created", retryRunID, err)
			}
			if run := waitForRunTerminalState(t, database, retryRunID); run.Status != types.RunCompleted || step.execCnt.Load() != 1 {
				t.Fatalf("retried run = %+v after %d step executions", run, step.execCnt.Load())
			}
		})
	}
}

// An authored admission used to bound itself with its own two minutes, started
// after its client's, so a slow stage outlived the client and the refusal that
// names the stage reached nobody (#249). It now gives up before its client does.
func TestAuthoredAdmissionRefusalReachesAClientWithAShorterDeadline(t *testing.T) {
	// Arrange
	manager, _, _, request, gate := authoredAdmissionFixture(t, nil)
	markers := t.TempDir()
	entered := filepath.Join(markers, "entered")
	block := filepath.Join(markers, "block")
	exited := filepath.Join(markers, "exited")
	if err := os.WriteFile(block, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(gate, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	slash := filepath.ToSlash
	hook := "#!/bin/sh\n: > '" + slash(entered) + "'\nwhile [ -e '" + slash(block) + "' ]; do sleep 0.05; done\n: > '" + slash(exited) + "'\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	isPresent := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	t.Cleanup(func() {
		_ = os.Remove(block)
		if !isPresent(entered) {
			return
		}
		for wait := time.Now().Add(10 * time.Second); time.Now().Before(wait) && !isPresent(exited); {
			time.Sleep(20 * time.Millisecond)
		}
	})
	server := ipc.NewServer()
	server.Handle(ipc.MethodValidateAuthored, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		return manager.validateAuthored(ctx, request, os.Getpid())
	})
	client, err := ipc.Dial(serveTestIPC(t, server))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Act
	err = client.CallWithTimeout(ipc.MethodValidateAuthored, struct{}{}, nil, 8*time.Second)

	// Assert
	if !isPresent(entered) {
		t.Fatalf("the gate's post-checkout hook never ran, so no stage outlived the client: %v", err)
	}
	var refusal *ipc.RPCError
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Error(), "deadline while accepting the admission and preparing its worktree") {
		t.Fatalf("client received %v, want the admission's own refusal naming the stage", err)
	}
}
