package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewLoop_NativeSafetyRefusalPreservesWorkAndParks(t *testing.T) {
	_, database, run, repo, workDir := reviewSessionHarness(t, &sessionMockAgent{}, nil)
	originalHead := run.HeadSHA
	originalFeature, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertRunAgentSession(run.ID, string(pipeline.SessionRoleFixer), "codex", "original-fixer"); err != nil {
		t.Fatal(err)
	}
	refusal := "This content was flagged for possible cybersecurity risk."
	review := `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","description":"needs a decision","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"needs a decision","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`
	reviewText, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	reviewEvent := fmt.Sprintf(`{"type":"item.completed","item":{"type":"agent_message","text":%s}}`, reviewText)
	refusalEvent := fmt.Sprintf(`{"type":"error","message":%q}`, refusal)
	script := fmt.Sprintf("#!/bin/sh\necho call >> invocations.txt\nif [ \"$2\" = resume ]; then\n  echo retained > retained.txt\n  echo retained >> feature.txt\n  echo '%s'\n  exit 1\nfi\necho '%s'\n", refusalEvent, reviewEvent)
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.cmd"
		script = fmt.Sprintf("@echo off\r\necho call>>invocations.txt\r\nif \"%%2\"==\"resume\" goto refused\r\necho %s\r\nexit /b 0\r\n:refused\r\necho retained>retained.txt\r\necho retained>>feature.txt\r\necho %s\r\nexit /b 1\r\n", reviewEvent, refusalEvent)
	}
	bin := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	native, err := agent.New(types.AgentCodex, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	appPaths := paths.WithRoot(t.TempDir())
	executor := pipeline.NewExecutor(database, appPaths, &config.Config{SessionReuse: true, AutoFix: config.AutoFix{Review: 3}}, native,
		[]pipeline.Step{&ReviewStep{}, &TestStep{}, &PushStep{}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if t.Failed() {
				log, _ := os.ReadFile(filepath.Join(appPaths.RunLogDir(run.ID), "review.log"))
				t.Logf("executor: %v\n%s", err, log)
			}
		case <-time.After(5 * time.Second):
			t.Error("executor did not stop")
		}
	})
	waitForReviewStatus(t, database, run.ID, types.StepStatusAwaitingApproval)
	if err := executor.Respond(types.StepReview, types.ActionFix, []string{"f-1"}); err != nil {
		t.Fatal(err)
	}
	waitForReviewStatus(t, database, run.ID, types.StepStatusFixReview)
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
		t.Fatal("approval advanced past a safety refusal")
	}
	storedRun, err := database.GetRun(run.ID)
	if err != nil || storedRun.Status != types.RunRunning || storedRun.AwaitingAgentSince == nil || storedRun.HeadSHA != originalHead {
		t.Fatalf("refusal must keep the original run parked: %+v, %v", storedRun, err)
	}
	if head := gitCmd(t, workDir, "rev-parse", "HEAD"); head != originalHead {
		t.Errorf("refusal committed work: got %s, want %s", head, originalHead)
	}
	stepResults, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range stepResults {
		if step.StepName == types.StepReview {
			if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, refusal) {
				t.Errorf("refusal is not visible in the gate: %+v", step)
			}
		} else if step.Status != types.StepStatusPending {
			t.Errorf("validation or publication advanced after refusal: %+v", step)
		}
	}
	invocations, err := os.ReadFile(filepath.Join(workDir, "invocations.txt"))
	if err != nil || len(strings.Fields(string(invocations))) != 2 {
		t.Errorf("expected only the review and refused resume, got %q, %v", invocations, err)
	}
	retained, err := os.ReadFile(filepath.Join(workDir, "retained.txt"))
	if err != nil || strings.TrimSpace(string(retained)) != "retained" {
		t.Errorf("refused process edits were lost: %q, %v", retained, err)
	}
	feature, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
	if err != nil || strings.ReplaceAll(string(feature), "\r\n", "\n") != strings.ReplaceAll(string(originalFeature), "\r\n", "\n")+"retained\n" {
		t.Errorf("refused process source edits were lost: %q, %v", feature, err)
	}
	if staged := gitCmd(t, workDir, "show", ":feature.txt"); staged != strings.TrimSpace(string(originalFeature)) {
		t.Errorf("refusal changed the index: got %q, want %q", staged, originalFeature)
	}
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "original-fixer" {
		t.Errorf("original fixer session was lost: %+v, %v", sessions, err)
	}
	log, err := os.ReadFile(filepath.Join(appPaths.RunLogDir(run.ID), "review.log"))
	if err != nil || !strings.Contains(string(log), refusal) || strings.Contains(string(log), "starting a fresh") {
		t.Errorf("refusal log was lost or announced a retry: %s, %v", log, err)
	}
}
