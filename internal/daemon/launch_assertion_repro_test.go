package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This wire-only reproduction also compiles against the pre-repair source.
func TestLaunchAssertionRunStartRejectsUnreadableRequiredProof(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	paths, database := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
	repository, head := setupTestGitRepo(t, paths, database, "required-launch-proof")
	client, err := ipc.Dial(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, proof := range []interface{}{nil, map[string]string{}} {
		var result ipc.StartFreshRunResult
		err := client.Call(ipc.MethodStartFreshRun, map[string]interface{}{
			"repo_id": repository.ID, "branch": "main", "head_sha": head,
			"intent": "required source proof", "launch_nonce": "required-proof", "validation_generation": "generation",
			"launch_assertion": proof,
		}, &result)
		if err == nil {
			t.Fatal("run-start silently ignored a missing or unreadable required launch assertion")
		}
	}
	runs, err := database.GetRunsByRepo(repository.ID)
	if err != nil || len(runs) != 0 || step.execCnt.Load() != 0 {
		t.Fatalf("invalid proof created validation work: runs=%d steps=%d error=%v", len(runs), step.execCnt.Load(), err)
	}
}
