//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiAuthoredValidationHasFreshStagesAndNoRepairAuthorityAfterCrash(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: branchSyncScenario(t), GlobalConfigExtra: "agent_config:\n  codex: {model: fixture, effort: xhigh}\nagent_args_override:\n  codex: ['-c', 'service_tier=\"default\"']\n"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("isolated init: %v\n%s", err, out)
	}
	branch := "feature/authored-validation"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "author initial candidate")
	caller := h.AddWorktree(branch)
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := launchassert.Expectation{TrustedSHA: strings.TrimSpace(h.WorktreeRefSHA("main")), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	assertionPath := filepath.Join(t.TempDir(), "launch.json")
	payload, err := json.Marshal(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assertionPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(caller, "axi", "run", "--intent", "Validate this explicit authored candidate", "--launch-nonce", "original-fixture", "--validation-generation", "original-policy", "--launch-assertion", assertionPath); err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("original launch: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "abort"); err != nil {
		t.Fatalf("terminalize fixture predecessor: %v\n%s", err, out)
	}
	parent := h.WaitForRun(branch, 30*time.Second)
	if out, err := h.Run("daemon", "restart"); err != nil {
		t.Fatalf("settle original executor: %v\n%s", err, out)
	}
	root := paths.WithRoot(h.NMHome)
	database, err := db.OpenReadOnly(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	before, err := database.ReadTerminalSnapshot(parent.ID)
	database.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, "feature.txt"), []byte("authored correction\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(caller, ".no-mistakes.yaml")
	configuration, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration = append(configuration, []byte("auto_fix:\n  review: 9\n  test: 9\n  document: 9\n  lint: 9\n  ci: 9\n  rebase: 9\n")...)
	if err := os.WriteFile(configPath, configuration, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, arguments := range [][]string{{"add", "feature.txt", ".no-mistakes.yaml"}, {"commit", "-m", "author a complete declared candidate"}} {
		if output, err := h.runGit(ctx, caller, arguments...); err != nil {
			t.Fatalf("author fixture: %v\n%s", err, output)
		}
	}
	head, err := h.runGit(ctx, caller, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := h.runGit(ctx, caller, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := realpath.Resolve(caller)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: before.Digest(), RepoID: before.Run.RepoID, Branch: branch, CallerDir: canonical, CandidateSHA: strings.TrimSpace(string(head)), CandidateTree: strings.TrimSpace(string(tree)), LaunchNonce: *before.Run.LaunchNonce, ValidationGeneration: *before.Run.LaunchValidationGeneration, IntentDigest: *before.Run.LaunchIntentDigest,
		ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("a", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: &assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate the declared authored source without certifying missing historical files"}
	admissionFile := filepath.Join(t.TempDir(), "admission.json")
	payload, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(admissionFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := h.RunInDir(caller, "axi", "validate-authored", "--admission-file", admissionFile, "--wait", "30s")
	if err != nil || !strings.Contains(output, "automatic_repairs_remaining: 0") || !strings.Contains(output, "controller_assertion") {
		t.Fatalf("public authored admission: %v\n%s", err, output)
	}
	child := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if child.ID == parent.ID {
		t.Fatal("admission restarted the cancelled predecessor")
	}
	database, err = db.OpenReadOnly(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	after, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		database.Close()
		t.Fatalf("original history changed: %v", err)
	}
	childSteps, err := database.GetStepsByRun(child.ID)
	database.Close()
	if err != nil || len(childSteps) != 9 {
		t.Fatalf("fresh child stages: %d %v", len(childSteps), err)
	}
	for _, step := range childSteps {
		if step.StepOrder > types.StepReview.Order() && step.Status != types.StepStatusPending {
			t.Fatalf("copied an old pass for %s", step.StepName)
		}
		if step.StepOrder < types.StepReview.Order() && step.Status != types.StepStatusCompleted {
			t.Fatalf("did not freshly execute %s", step.StepName)
		}
	}
	if out, err := h.RunInDir(caller, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err == nil || !strings.Contains(out, "zero") {
		t.Fatalf("manual repair was renewed: %v\n%s", err, out)
	}
	pid, err := daemon.ReadPID(root)
	if err != nil || !e2edaemon.MatchesDaemonRoot(pid, h.NMHome) {
		t.Fatalf("cannot prove disposable daemon ownership: pid=%d error=%v", pid, err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		process.Release()
		t.Fatal(err)
	}
	process.Release()
	for deadline := time.Now().Add(10 * time.Second); ; {
		alive, err := e2edaemon.ProcessAlive(pid)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned daemon %d did not exit", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("restart isolated daemon: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err == nil || !strings.Contains(out, "zero") {
		t.Fatalf("restart restored manual repair capacity: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "validate-authored", "--admission-file", admissionFile, "--wait", "5s"); err != nil || !strings.Contains(out, child.ID) || !strings.Contains(out, "started: false") {
		t.Fatalf("idempotent admission after restart: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "abort"); err != nil {
		t.Fatalf("cancel fixture child: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "validate-authored", "--admission-file", admissionFile, "--wait", "5s"); err == nil || !strings.Contains(out, child.ID) || !strings.Contains(out, "cancelled") {
		t.Fatalf("cancelled child was restarted: %v\n%s", err, out)
	}
	finalContext, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finalCancel()
	current, err := h.runGit(finalContext, caller, "rev-parse", "HEAD")
	if err != nil || !bytes.Equal(current, head) {
		t.Fatalf("author source moved: %v", err)
	}
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Investigate previous review findings") || strings.Contains(invocation.Prompt, "Previous review findings to address") {
			t.Fatal("validation-only journey invoked a repair agent")
		}
	}

	database, err = db.Open(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var refusedStep *db.StepResult
	for _, step := range childSteps {
		if step.StepName == types.StepReview {
			refusedStep = step
		}
	}
	if refusedStep == nil {
		t.Fatal("terminal child has no review step")
	}
	if err := database.SetStepFindings(refusedStep.ID, `{"findings":[{"id":"agent-safety-refusal","severity":"error","description":"native provider refused execution","action":"ask-user"}]}`); err != nil {
		t.Fatal(err)
	}
	refusedSnapshot, err := database.ReadTerminalSnapshot(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, "feature.txt"), []byte("authored correction after refused turn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refusalContext, refusalCancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer refusalCancel()
	for _, arguments := range [][]string{{"add", "feature.txt"}, {"commit", "-m", "author correction after refused turn"}} {
		if output, err := h.runGit(refusalContext, caller, arguments...); err != nil {
			t.Fatalf("author after refusal: %v\n%s", err, output)
		}
	}
	newHead, err := h.runGit(refusalContext, caller, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	newTree, err := h.runGit(refusalContext, caller, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	request.PredecessorID, request.SnapshotDigest, request.LaunchNonce = child.ID, refusedSnapshot.Digest(), *refusedSnapshot.Run.LaunchNonce
	request.CandidateSHA, request.CandidateTree = strings.TrimSpace(string(newHead)), strings.TrimSpace(string(newTree))
	payload, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(admissionFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(caller, "axi", "validate-authored", "--admission-file", admissionFile, "--wait", "5s"); err == nil || !strings.Contains(out, "native agent safety refusal") {
		t.Fatalf("authored descendant bypassed native refusal: %v\n%s", err, out)
	}
	if event, err := database.GetValidationAdmission(child.ID); err != nil || event != nil {
		t.Fatalf("refused descendant recorded an admission: %+v %v", event, err)
	}
}
