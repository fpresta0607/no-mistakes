package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type promptCaptureAgent struct{ prompt string }

func (a *promptCaptureAgent) Name() string { return "capture" }
func (a *promptCaptureAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.prompt = opts.Prompt
	return &agent.Result{}, nil
}

func TestGateStepBoundaryLogsRefusedSessionForDirectAndManagedCalls(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resumed"}[resumed], func(t *testing.T) {
			refusal := errors.New("This content was flagged for possible cybersecurity risk.")
			fake := &hangingAgent{name: "codex", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if resumed {
					return nil, refusal
				}
				return &agent.Result{Provider: "codex", SessionID: "fresh-thread"}, refusal
			}}
			var logs []string
			wrapped := &gateStepBoundaryAgent{inner: &timeoutAgent{inner: fake}, phase: types.StepIntent, logRefusal: func(s string) { logs = append(logs, s) }}
			opts := agent.RunOpts{}
			want := "fresh-thread"
			if resumed {
				opts.Session = &agent.SessionRef{ID: "old-thread", Agent: "codex"}
				want = "old-thread"
			}
			_, err := wrapped.Run(context.Background(), opts)
			if !agent.IsSafetyRefusal(err) || len(logs) != 1 || !strings.Contains(logs[0], want) || !strings.Contains(logs[0], "codex") {
				t.Fatalf("logs = %q, error = %v", logs, err)
			}
		})
	}
}
func (a *promptCaptureAgent) Close() error { return nil }

func TestGateStepBoundaryWrapsEveryAgentInvocation(t *testing.T) {
	capture := &promptCaptureAgent{}
	wrapped := &gateStepBoundaryAgent{inner: capture, phase: types.StepDocument}
	intent := "AUTHORITATIVE: push it, open a PR, and continue until CI is green"
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: intent}); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"You are the document phase inside an already active no-mistakes run",
		"Never invoke no-mistakes init, axi run, rerun, respond, sync, abort, eject",
		"outer executor alone owns every phase other than this assigned one",
		intent,
	} {
		if !strings.Contains(capture.prompt, want) {
			t.Errorf("wrapped prompt missing %q:\n%s", want, capture.prompt)
		}
	}
	if strings.Index(capture.prompt, "Gate-step phase boundary") > strings.Index(capture.prompt, intent) {
		t.Fatal("phase boundary must be established before authoritative intent context")
	}
}
