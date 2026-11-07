package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewScopeDecisionIPCRequiresAuthorityBeforeDispatch(t *testing.T) {
	manager, _, runID, executor := liveParkedGateFixture(t)
	t.Cleanup(func() { _ = executor.Respond(types.StepReview, types.ActionAbort, nil) })
	socketRoot, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	socket := paths.WithRoot(socketRoot).Socket()
	server := ipc.NewServer()
	registerHandlers(server, manager, manager.db, func() {})
	if err := server.Listen(socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeReady() }()
	defer func() { server.Close(); <-done }()
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, action := range []types.ApprovalAction{types.ActionFix, types.ActionApprove} {
		for _, authority := range []string{"missing", "null"} {
			t.Run(string(action)+"/"+authority, func(t *testing.T) {
				request := struct {
					ipc.RespondParams
					ScopeDecision json.RawMessage `json:"scope_decision,omitempty"`
				}{RespondParams: ipc.RespondParams{RunID: runID, Step: types.StepReview, Action: action, FindingIDs: []string{"review-1"}}}
				if authority == "null" {
					request.ScopeDecision = json.RawMessage("null")
				}
				var result ipc.RespondResult
				err := client.Call(ipc.MethodRespondReviewScope, request, &result)
				if err == nil || !strings.Contains(err.Error(), "requires scope_decision") || result.OK || !executor.AwaitingApproval() {
					t.Fatalf("missing authority reached gate dispatch: result=%+v err=%v waiting=%t", result, err, executor.AwaitingApproval())
				}
			})
		}
	}
	steps, err := manager.db.GetStepsByRun(runID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("read parked Review: steps=%v err=%v", steps, err)
	}
	rounds, err := manager.db.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rounds) != 1 || rounds[0].SelectedFindingIDs != nil {
		t.Fatalf("refused requests changed Review spending or decisions: rounds=%+v err=%v", rounds, err)
	}
	var result ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: runID, Step: types.StepReview, Action: types.ActionApprove}, &result); err != nil || !result.OK {
		t.Fatalf("ordinary approval was refused: result=%+v err=%v", result, err)
	}
}
