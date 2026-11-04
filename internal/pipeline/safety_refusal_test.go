package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSafetyRefusalGateDoesNotResume(t *testing.T) {
	resumer := &evidenceReadingResumer{name: types.StepReview}
	executor := &Executor{}
	findings := `{"findings":[{"id":"agent-safety-refusal","severity":"error","description":"Native safety refusal","action":"ask-user"}]}`
	action, resumed, err := executor.resumeApprovalGate(context.Background(), resumer, &StepContext{EvidenceDir: t.TempDir()}, findings)
	if err != nil || resumed || action != "" || len(resumer.evidenceDirs()) != 0 {
		t.Fatalf("refusal resumed automatically: action=%s resumed=%v err=%v calls=%v", action, resumed, err, resumer.evidenceDirs())
	}
}

// stopCountingSideLanes stands in for the work a review started beside it.
type stopCountingSideLanes struct{ stops atomic.Int32 }

func (l *stopCountingSideLanes) Stop(sctx *StepContext) {
	l.stops.Add(1)
	sctx.Log("side lane stopped by the test")
}

// Parking on a native refusal stops the work started beside the review, so no
// lane goes on to start an agent while the operator decides. An ordinary park
// leaves that work running, as side_by_side intends.
func TestSafetyRefusalParkStopsSideLanes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantStops int32
	}{
		{name: "native refusal", err: errors.New("codex exited: exit status 1: This content was flagged for possible cybersecurity risk."), wantStops: 1},
		{name: "ordinary park"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			lanes := &stopCountingSideLanes{}
			review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				sctx.Shared.SetSideLanes(lanes)
				if tc.err != nil {
					return nil, tc.err
				}
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"f-1","severity":"error","description":"needs a decision","action":"ask-user"}]}`}, nil
			}}
			executor := NewExecutor(database, p, &config.Config{SideBySide: true}, nil, []Step{review}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, run, repo, t.TempDir()) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("executor did not stop")
				}
			})

			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

			if got := lanes.stops.Load(); got != tc.wantStops {
				t.Fatalf("side lanes stopped %d time(s), want %d", got, tc.wantStops)
			}
			if tc.wantStops > 0 {
				log, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "review.log"))
				if err != nil || !strings.Contains(string(log), "side lane stopped by the test") {
					t.Fatalf("the stop was not logged to the parked step: %q, %v", log, err)
				}
			}
		})
	}
}
