package pipeline

import (
	"context"
	"testing"

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
