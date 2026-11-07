package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReadReviewScopeDecisionRejectsMalformedAuthority(t *testing.T) {
	valid := types.ReviewScopeDecision{Version: 1, DecisionID: "decision", Authority: "controller_assertion", RunID: "run", StepResultID: "step", RoundID: "round", SourceHeadSHA: "head", SourceBaseSHA: "base", TrustedConfigSHA: "trusted", Finding: types.Finding{ID: "R1", Description: "original"}, AcceptedScope: "retain fixed cleanup bounds"}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"missing", `{}`},
		{"invalid json", `{`},
		{"extra object", string(encoded) + `{}`},
		{"false native authority", strings.Replace(string(encoded), "controller_assertion", "authenticated_primary", 1)},
		{"actor label", strings.TrimSuffix(string(encoded), "}") + `,"actor":"primary"}`},
		{"oversized", strings.Repeat(" ", 32*1024) + string(encoded)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readReviewScopeDecision(writeIntentFile(t, test.raw)); err == nil {
				t.Fatal("accepted malformed or unsupported scope authority")
			}
		})
	}
	got, err := readReviewScopeDecision(writeIntentFile(t, string(encoded)))
	if err != nil || *got != valid {
		t.Fatalf("explicit assertion changed: got=%v err=%v", got, err)
	}
}

func TestAxiRespondForwardsAndAcknowledgesExplicitScope(t *testing.T) {
	var hasResponded atomic.Bool
	received := make(chan ipc.RespondParams, 1)
	fixture := newAxiTimeoutFixture(t, axiTimeoutOpts{respondScope: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RespondParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		received <- params
		hasResponded.Store(true)
		return &ipc.RespondResult{OK: true, Fixed: []string{"R1"}, ScopeDecisionID: params.ScopeDecision.DecisionID}, nil
	}})
	fixture.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if hasResponded.Load() {
			return fixture.completed(), nil
		}
		return fixture.awaiting(), nil
	})
	decision := types.ReviewScopeDecision{Version: 1, DecisionID: "decision", Authority: "controller_assertion", RunID: "run-timeout", StepResultID: "step", RoundID: "round", SourceHeadSHA: "head", SourceBaseSHA: "base", TrustedConfigSHA: "trusted", Finding: types.Finding{ID: "R1", Description: "original"}, AcceptedScope: "retain fixed cleanup bounds"}
	encoded, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newAxiRespondCmd()
	cmd.SetArgs([]string{"--action", "fix", "--findings", "R1", "--scope-decision-file", writeIntentFile(t, string(encoded)), "--wait", "3s"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("respond failed: %v\n%s", err, output.String())
	}
	select {
	case params := <-received:
		if params.ScopeDecision == nil || *params.ScopeDecision != decision || len(params.Instructions) != 0 {
			t.Fatalf("explicit scope was changed or mixed into fixer guidance: %+v", params)
		}
	default:
		t.Fatal("explicit scope never reached its IPC method")
	}
	for _, want := range []string{"accepted_scope", "controller_assertion", "retain fixed cleanup bounds"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("response acknowledgement omitted %q: %s", want, output.String())
		}
	}
}

func TestScopeDecisionNeverFallsBackToAnOrdinaryResponse(t *testing.T) {
	var hasOrdinaryResponse atomic.Bool
	newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(context.Context, json.RawMessage) (interface{}, error) {
		hasOrdinaryResponse.Store(true)
		return &ipc.RespondResult{OK: true}, nil
	}})
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	client := dialReady(t, p.Socket())
	defer client.Close()
	decision := &types.ReviewScopeDecision{DecisionID: "decision"}
	_, err = sendRespond(client, "run", types.StepReview, types.ActionFix, []string{"R1"}, nil, nil, nil, "", types.ResponseOriginInteractive, decision)
	if err == nil || hasOrdinaryResponse.Load() {
		t.Fatalf("an unsupported scope method must refuse without an ordinary response: err=%v ordinary=%v", err, hasOrdinaryResponse.Load())
	}
}
