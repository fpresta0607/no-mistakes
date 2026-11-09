//go:build e2e

package e2e

import (
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
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiTerminalValidationKeepsTheReturnedCandidateAndProvesTheNewSelection(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: branchSyncScenario(t), GlobalConfigExtra: "agent_config:\n  codex: {model: fixture, effort: xhigh}\nagent_args_override:\n  codex: ['-c', 'service_tier=\"default\"']\n"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/terminal-validation"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "initial terminal candidate")
	caller := h.AddWorktree(branch)
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := launchassert.Expectation{TrustedSHA: strings.TrimSpace(h.WorktreeRefSHA("main")), Apply: true, Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	assertionFile := filepath.Join(t.TempDir(), "selection.json")
	payload, err := json.Marshal(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assertionFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(caller, "axi", "run", "--intent", "validate the exact retained candidate", "--launch-nonce", "terminal-original", "--validation-generation", "original-lane", "--launch-assertion", assertionFile); err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("launch: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "abort"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
	parent := h.WaitForRun(branch, 30*time.Second)
	if out, err := h.Run("daemon", "stop"); err != nil {
		t.Fatalf("stop fixture daemon: %v\n%s", err, out)
	}
	root := paths.WithRoot(h.NMHome)
	database, err := db.Open(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunsCustodyReturned([]string{parent.ID}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunErrorStatus(parent.ID, "provider quota exhausted", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetRun(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(caller, "axi", "terminal-preflight", "--predecessor", parent.ID); err != nil || !strings.Contains(out, "historical_source_completeness: unknown") || !strings.Contains(out, "candidate_is_terminal_head: true") {
		t.Fatalf("read-only preflight: %v\n%s", err, out)
	}
	if isRunning, err := daemon.IsRunning(root); err != nil || isRunning {
		t.Fatalf("preflight started a daemon: %v %v", isRunning, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tree, err := h.runGit(ctx, caller, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := realpath.Resolve(caller)
	if err != nil {
		t.Fatal(err)
	}
	selection.Model = "available-successor"
	fresh := launchassert.Expectation{TrustedSHA: assertion.TrustedSHA, Apply: true, Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: parent.RepoID, Branch: branch, CallerDir: canonical, CandidateSHA: before.HeadSHA, CandidateTree: strings.TrimSpace(string(tree)), LaunchNonce: *before.LaunchNonce, ValidationGeneration: *before.LaunchValidationGeneration, IntentDigest: *before.LaunchIntentDigest, ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("a", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: &fresh, HistoricalSourceCompleteness: "unknown", Reason: "accept this committed candidate while old dirty/untracked source remains unknown", TerminalGeneration: "new-independent-lane", AcceptUnknownSource: true}
	admissionFile := filepath.Join(t.TempDir(), "admission.json")
	payload, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(admissionFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(caller, "axi", "validate-authored", "--admission-file", admissionFile); err == nil || !strings.Contains(out, "use validate-terminal") {
		t.Fatalf("wrong route accepted terminal request: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "validate-terminal", "--admission-file", admissionFile, "--wait", "30s"); err != nil || !strings.Contains(out, "automatic_repairs_remaining: 0") || !strings.Contains(out, "new-independent-lane") {
		t.Fatalf("terminal validation: %v\n%s", err, out)
	}
	child := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if child.ID == parent.ID || child.HeadSHA != before.HeadSHA {
		t.Fatalf("candidate or predecessor replaced: %+v", child)
	}
	database, err = db.OpenReadOnly(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	after, err := database.GetRun(parent.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("predecessor changed: %v", err)
	}
	childRun, err := database.GetRun(child.ID)
	if err != nil || childRun.LaunchAssertionProof.Check(&fresh) != nil || childRun.ReviewApprovedHeadSHA != nil || childRun.CIReadyAt != nil || *childRun.LaunchNonce == *before.LaunchNonce {
		t.Fatalf("new authority is not independently bound: %+v %v", childRun, err)
	}
	database.Close()
	if out, err := h.RunInDir(caller, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err == nil || !strings.Contains(out, "zero") {
		t.Fatalf("renewed manual repairs: %v\n%s", err, out)
	}
	if out, err := h.Run("daemon", "restart", "--force"); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "validate-terminal", "--admission-file", admissionFile, "--wait", "5s"); err != nil || !strings.Contains(out, child.ID) || !strings.Contains(out, "started: false") {
		t.Fatalf("replay after restart: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err == nil || !strings.Contains(out, "zero") {
		t.Fatalf("restart renewed repair capacity: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "abort"); err != nil {
		t.Fatalf("abort child: %v\n%s", err, out)
	}
}
