package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLaunchAssertionWireCannotEraseCapturedExpectation(t *testing.T) {
	for _, method := range []string{ipc.MethodStartFreshRun, ipc.MethodPushReceived, ipc.MethodClaimLaunchReceipt} {
		t.Run(method, func(t *testing.T) {
			var factoryCalls atomic.Int32
			step := &mockPassStep{name: types.StepReview}
			manager, root, database, repository, head, marker, _ := launchAssertionFixture(t, func() []pipeline.Step {
				factoryCalls.Add(1)
				return []pipeline.Step{step}
			})
			active, err := database.InsertRun(repository.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			activeContext, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			manager.cancels[active.ID] = cancel
			gateDirectory := root.RepoDir(repository.ID)
			gitCmd(t, gateDirectory, "update-ref", "refs/remotes/origin/main", head)
			beforeRefs := gitOutput(t, gateDirectory, "for-each-ref", "--format=%(refname) %(objectname)")
			beforeWorktrees := gitOutput(t, gateDirectory, "worktree", "list", "--porcelain")
			fetchHead := []byte("wire fixture healthy custody\n")
			fetchHeadPath := filepath.Join(gateDirectory, "FETCH_HEAD")
			if err := os.WriteFile(fetchHeadPath, fetchHead, 0o600); err != nil {
				t.Fatal(err)
			}
			socketRoot, err := os.MkdirTemp("", "nm-wire-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(socketRoot)
			server := ipc.NewServer()
			registerHandlers(server, manager, database, func() {})
			socket := paths.WithRoot(socketRoot).Socket()
			if err := server.Listen(socket); err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- server.ServeReady() }()
			defer func() { server.Close(); <-serverDone }()
			client, err := ipc.Dial(socket)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			expected := launchExpectation(strings.Repeat("a", 40))
			if expected.TrustedSHA == head {
				t.Fatal("wire fixture mismatch premise is absent")
			}
			encodedExpectation, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			variants := []string{
				`"launch_assertion":` + string(encodedExpectation),
				`"launch_assertion":null`,
				`"launch_assertion":` + string(encodedExpectation) + `,"LAUNCH_ASSERTION":null`,
				`"LAUNCH_ASSERTION":null`,
			}
			for index, assertionFields := range variants {
				nonce := fmt.Sprintf("wire-%d", index)
				if method == ipc.MethodClaimLaunchReceipt {
					if _, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", head, head, nil, nonce, "generation", "digest", "", false, nil, nil, nil); err != nil {
						t.Fatal(err)
					}
				}
				beforeRuns, err := database.GetRunsByRepo(repository.ID)
				if err != nil {
					t.Fatal(err)
				}
				var base []byte
				switch method {
				case ipc.MethodStartFreshRun:
					base, err = json.Marshal(ipc.StartFreshRunParams{RepoID: repository.ID, Branch: "feature", HeadSHA: head, Intent: "wire refuses erased expectation", LaunchNonce: nonce, ValidationGeneration: "generation"})
				case ipc.MethodPushReceived:
					base, err = json.Marshal(ipc.PushReceivedParams{Gate: gateDirectory, Ref: "refs/heads/feature", Old: head, New: head, Intent: "wire refuses erased expectation", LaunchNonce: nonce, ValidationGeneration: "generation"})
				case ipc.MethodClaimLaunchReceipt:
					base, err = json.Marshal(ipc.ClaimLaunchReceiptParams{RepoID: repository.ID, Branch: "feature", SubmittedHeadSHA: head, IntentDigest: "digest", LaunchNonce: nonce, ValidationGeneration: "generation"})
				}
				if err != nil {
					t.Fatal(err)
				}
				raw := json.RawMessage(string(base[:len(base)-1]) + "," + assertionFields + "}")
				var result json.RawMessage
				callError := client.Call(method, raw, &result)
				var rpcError *ipc.RPCError
				if callError == nil {
					t.Errorf("assertion fields were erased at the wire boundary: variant=%d result=%s", index, result)
				} else if !errors.As(callError, &rpcError) || !strings.Contains(callError.Error(), "launch assertion") && !strings.Contains(callError.Error(), "captured assertion differs") {
					t.Fatalf("fixture/environment error before assertion behavior: %v", callError)
				}
				afterRuns, err := database.GetRunsByRepo(repository.ID)
				if err != nil || !reflect.DeepEqual(afterRuns, beforeRuns) {
					t.Errorf("erased assertion changed run/receipt custody: %+v, error=%v", afterRuns, err)
				}
				if context.Cause(activeContext) != nil || factoryCalls.Load() != 0 || step.execCnt.Load() != 0 {
					t.Errorf("erased assertion cancelled or launched work: cause=%v factories=%d steps=%d", context.Cause(activeContext), factoryCalls.Load(), step.execCnt.Load())
				}
				if rows, err := database.GetStepsByRun(active.ID); err != nil || len(rows) != 0 {
					t.Errorf("erased assertion changed healthy step custody: %+v, error=%v", rows, err)
				}
				if refs := gitOutput(t, gateDirectory, "for-each-ref", "--format=%(refname) %(objectname)"); refs != beforeRefs {
					t.Errorf("erased assertion changed refs: %s", refs)
				}
				if worktrees := gitOutput(t, gateDirectory, "worktree", "list", "--porcelain"); worktrees != beforeWorktrees {
					t.Errorf("erased assertion changed worktree custody: %s", worktrees)
				}
				if after, err := os.ReadFile(fetchHeadPath); err != nil || !bytes.Equal(after, fetchHead) {
					t.Errorf("erased assertion changed FETCH_HEAD: %q, error=%v", after, err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Errorf("erased assertion entered agent fixture: %v", err)
				}
			}
			var probe ipc.ProbeLaunchAssertionResult
			if err := client.Call(ipc.MethodProbeLaunchAssertion, json.RawMessage(`{"LAUNCH_ASSERTION":null}`), &probe); err == nil {
				t.Error("capability probe accepted an absent assertion")
			}
			if method == ipc.MethodClaimLaunchReceipt {
				if _, err := database.InsertRunWithLaunchAssertion(repository.ID, "feature", head, head, nil, "legacy-omitted", "generation", "digest", "", false, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				var legacy ipc.ClaimLaunchReceiptResult
				if err := client.Call(method, &ipc.ClaimLaunchReceiptParams{RepoID: repository.ID, Branch: "feature", SubmittedHeadSHA: head, IntentDigest: "digest", LaunchNonce: "legacy-omitted", ValidationGeneration: "generation"}, &legacy); err != nil || legacy.Receipt == nil || legacy.Receipt.Disposition != "created" || legacy.Receipt.LaunchAssertionProof != nil {
					t.Errorf("legacy omitted assertion lost optional-nonce compatibility: %+v, error=%v", legacy, err)
				}
			}
		})
	}
}
