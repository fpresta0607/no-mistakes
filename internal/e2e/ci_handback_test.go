//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiPublishedCIConflictHandback(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	remoteURL := "https://github.com/example/handback.git"
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	mustGit := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(t.Context(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mustGit(h.WorkDir, "remote", "set-url", "origin", remoteURL)
	statePath := filepath.Join(filepath.Dir(h.AgentLog), "forge.json")
	t.Setenv("FAKEAGENT_GH_MODE", "stateful-pr")
	t.Setenv("FAKEAGENT_GH_PARENT", "example/handback")
	t.Setenv("FAKEAGENT_GH_STATE", statePath)
	// The fake forge reports the conflict only after the published review fix.
	state := map[string]any{"prs": map[string]any{}}
	writeState := func() {
		t.Helper()
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeState()
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\nno_ci: true\n", "declare fixture without checks")
	h.CommitChange("main", "feature.txt", "original\n", "seed conflict surface")
	mustGit(h.WorkDir, "push", "origin", "main")
	h.CommitChange("init-handback", "seed.txt", "seed\n", "seed init")
	initFromOwnWorktree(t, h, "init-handback")
	const branch = "feature/ci-handback"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add feature")
	operator := h.AddWorktree(branch)
	out, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve pipeline fixes through an additive CI conflict repair")
	if err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("review gate: %v\n%s", err, out)
	}
	out, err = h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1")
	if err != nil || !strings.Contains(out, "fix_review") {
		t.Fatalf("review fix: %v\n%s", err, out)
	}
	// Diverge the base after the rebase step, while Review is still parked.
	base := h.CommitChange("main", "feature.txt", "base change\n", "advance conflicting base")
	mustGit(h.WorkDir, "push", "origin", "main")
	state["prs"] = map[string]any{branch: map[string]any{
		"number": 100, "url": "https://github.com/example/handback/pull/100",
		"title": "fixture", "body": "fixture", "base": "main", "head": branch,
		"state": "OPEN", "mergeable": "CONFLICTING",
	}}
	writeState()
	out, err = h.RunInDir(operator, "axi", "respond", "--action", "approve")
	if err != nil || !strings.Contains(out, "PR has merge conflicts") {
		t.Fatalf("CI conflict: %v\n%s", err, out)
	}
	parked := h.ActiveRun(branch)
	if parked == nil || parked.Status != types.RunRunning {
		t.Fatalf("parked run = %#v", parked)
	}
	ci, ok := findStep(parked.Steps, types.StepCI)
	if !ok || ci.Status != types.StepStatusAwaitingApproval || ci.AutoFixLimit != 0 || ci.FixRoundCount != 0 {
		t.Fatalf("CI must park with zero auto repairs: %#v", ci)
	}
	if parked.HeadSHA == submitted {
		t.Fatal("fixture did not retain a pipeline fix commit")
	}
	before := len(h.AgentInvocations())
	if out, err := h.RunInDir(operator, "axi", "handback"); err == nil {
		t.Fatalf("handback accepted a caller missing the pipeline fix: %s", out)
	}
	out, err = h.RunInDir(operator, "axi", "sync")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if got := mustGit(operator, "rev-parse", "HEAD"); got != parked.HeadSHA {
		t.Fatalf("caller = %s, published = %s", got, parked.HeadSHA)
	}
	out, err = h.RunInDir(operator, "axi", "sync", "--check")
	if err != nil || !strings.Contains(out, "already_synchronized") {
		t.Fatalf("sync check: %v\n%s", err, out)
	}
	t.Logf("before handback: submitted=%s published=%s base=%s run=%s PR=%s", submitted, parked.HeadSHA, base, parked.ID, deref(parked.PRURL))
	database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	recorded, err := database.GetRun(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	review, _ := findStep(parked.Steps, types.StepReview)
	priorReviewRounds, err := database.GetRoundsByStep(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	priorCIRounds, err := database.GetRoundsByStep(ci.ID)
	if err != nil || len(priorCIRounds) != 1 {
		t.Fatalf("initial conflict round: %#v %v", priorCIRounds, err)
	}
	repo, err := database.GetRepo(recorded.RepoID)
	if err != nil || repo == nil {
		t.Fatalf("recorded repository: %#v %v", repo, err)
	}
	duplicate := filepath.Join(t.TempDir(), "duplicate-caller")
	for _, test := range []struct {
		name    string
		prepare func()
		restore func()
	}{
		{"dirty caller", func() {
			if err := os.WriteFile(filepath.Join(operator, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, func() {
			if err := os.Remove(filepath.Join(operator, "dirty.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"active writer", func() {
			if err := database.SetRunPushActive(parked.ID, true); err != nil {
				t.Fatal(err)
			}
		}, func() {
			if err := database.SetRunPushActive(parked.ID, false); err != nil {
				t.Fatal(err)
			}
		}},
		{"moved publication", func() { mustGit(h.WorkDir, "push", "origin", "main:refs/heads/"+branch, "--force") }, func() { mustGit(h.WorkDir, "push", "origin", parked.HeadSHA+":refs/heads/"+branch, "--force") }},
		{"mismatched target", func() {
			if _, err := database.ReplaceRepoURLs(repo.ID, "https://github.com/other/handback.git", ""); err != nil {
				t.Fatal(err)
			}
		}, func() {
			if _, err := database.ReplaceRepoURLs(repo.ID, repo.UpstreamURL, repo.ForkURL); err != nil {
				t.Fatal(err)
			}
		}},
		{"ambiguous caller", func() { mustGit(h.WorkDir, "worktree", "add", "--force", duplicate, branch) }, func() { mustGit(h.WorkDir, "worktree", "remove", "--force", duplicate) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.prepare()
			defer test.restore()
			out, err := h.RunInDir(operator, "axi", "handback")
			if err == nil {
				t.Fatalf("unsafe handback accepted: %s", out)
			}
			receipt, err := database.LatestCIHandback(parked.ID)
			if err != nil || receipt != nil {
				t.Fatalf("refusal wrote a receipt: %#v %v", receipt, err)
			}
			if head := mustGit(operator, "rev-parse", "HEAD"); head != parked.HeadSHA {
				t.Fatalf("refusal moved caller to %s", head)
			}
		})
	}
	out, err = h.RunInDir(operator, "axi", "handback")
	if err != nil {
		t.Fatalf("published conflict has no caller handback: %v\n%s", err, out)
	}
	if !strings.Contains(out, "released") {
		t.Fatalf("handback output:\n%s", out)
	}
	after := h.RunInfo(parked.ID)
	afterCI, _ := findStep(after.Steps, types.StepCI)
	if after.ID != parked.ID || deref(after.PRURL) != deref(parked.PRURL) || afterCI.Status != ci.Status || deref(afterCI.FindingsJSON) != deref(ci.FindingsJSON) {
		t.Fatalf("handback changed the unresolved run: %#v", after)
	}
	if len(h.AgentInvocations()) != before {
		t.Fatal("handback launched an agent")
	}
	receipt, err := database.LatestCIHandback(parked.ID)
	if err != nil || receipt == nil || receipt.ReleasedHead != parked.HeadSHA || receipt.PushGeneration != *recorded.PushGeneration {
		t.Fatalf("release receipt = %#v, %v", receipt, err)
	}
	for _, action := range []string{"approve", "skip", "fix"} {
		args := []string{"axi", "respond", "--action", action}
		if action == "fix" {
			args = append(args, "--findings", "ci-1")
		}
		if out, err := h.RunInDir(operator, args...); err == nil {
			t.Fatalf("released gate accepted %s: %s", action, out)
		}
	}
	if out, err := h.RunInDir(operator, "axi", "handback", "--resume"); err == nil {
		t.Fatalf("unchanged head resumed: %s", out)
	}
	mustGit(operator, "fetch", "origin", "main")
	if out, err := h.runGit(t.Context(), operator, "merge", "--no-ff", "--no-edit", base); err == nil {
		t.Fatalf("fixture lacks a real merge conflict: %s", out)
	}
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("safe\nbase change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(operator, "add", "feature.txt")
	mustGit(operator, "commit", "-m", "resolve conflict additively")
	followup := mustGit(operator, "rev-parse", "HEAD")
	for _, ancestor := range []string{submitted, parked.HeadSHA, base} {
		mustGit(operator, "merge-base", "--is-ancestor", ancestor, followup)
	}
	if out, err := h.RunInDir(operator, "axi", "run", "--intent", "submit the caller follow-up"); err == nil {
		t.Fatalf("ordinary run replaced the released run: %s", out)
	}
	if current := h.ActiveRun(branch); current == nil || current.ID != parked.ID {
		t.Fatalf("released run was superseded: %#v", current)
	}
	// A fresh review still parks. Old validation cannot make this head ready.
	out, err = h.RunInDir(operator, "axi", "handback", "--resume", "--receipt", receipt.ID)
	if err != nil || !strings.Contains(out, "step: review") {
		t.Fatalf("follow-up did not receive fresh Review: %v\n%s", err, out)
	}
	validating := h.RunInfo(parked.ID)
	if validating.CIReady || validating.HeadSHA != followup || validating.ID != parked.ID {
		t.Fatalf("new head inherited readiness: %#v", validating)
	}
	remoteHead := mustGit(h.UpstreamDir, "rev-parse", "refs/heads/"+branch)
	if remoteHead != parked.HeadSHA {
		t.Fatalf("follow-up published before revalidation: %s", remoteHead)
	}
	refreshed, err := database.GetRun(parked.ID)
	if err != nil || refreshed.ReviewApprovedHeadSHA != nil || *refreshed.LastPushedSHA != parked.HeadSHA || *refreshed.PushGeneration != *recorded.PushGeneration {
		t.Fatalf("fresh validation binding = %#v %v", refreshed, err)
	}
	state["prs"] = map[string]any{branch: map[string]any{
		"number": 100, "url": "https://github.com/example/handback/pull/100",
		"title": "fixture", "body": "fixture", "base": "main", "head": branch,
		"state": "OPEN", "mergeable": "MERGEABLE",
	}}
	writeState()
	// The fixture reviewer becomes clean on its next actual review turn.
	if err := os.WriteFile(h.Scenario, []byte(`actions:
  - text: clean
    structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: clean
      risk_scope: source-or-external
      tested: [live fixture]
      testing_summary: passed
      scenarios:
        - name: live fixture
          result: pass
          live: true
          evidence: fake service
          reason: ""
      verdict: go
      artifacts: []
      title: fix conflict
      body: additive resolution
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1")
	if err != nil {
		t.Fatalf("fresh review turn: %v\n%s", err, out)
	}
	if strings.Contains(out, "fix_review") {
		out, err = h.RunInDir(operator, "axi", "respond", "--action", "approve")
		if err != nil {
			t.Fatalf("fresh review conclusion: %v\n%s", err, out)
		}
	}
	deadline := time.Now().Add(45 * time.Second)
	var finalHead string
	for time.Now().Before(deadline) {
		final := h.RunInfo(parked.ID)
		if final.CIReady {
			finalHead = final.HeadSHA
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finalHead == "" {
		h.dumpDebugState()
		t.Fatal("revalidated head never became ready")
	}
	for _, ancestor := range []string{submitted, parked.HeadSHA, base, followup} {
		mustGit(operator, "merge-base", "--is-ancestor", ancestor, finalHead)
	}
	final := h.RunInfo(parked.ID)
	for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		step, ok := findStep(final.Steps, stepName)
		if !ok || step.Status != types.StepStatusCompleted || step.RoundCount < 2 {
			t.Fatalf("missing fresh %s validation: %#v", stepName, step)
		}
	}
	rounds, err := database.GetRoundsByStep(review.ID)
	if err != nil || len(rounds) <= len(priorReviewRounds) {
		t.Fatalf("review history lost: %#v %v", rounds, err)
	}
	for index := range priorReviewRounds {
		if rounds[index].ID != priorReviewRounds[index].ID {
			t.Fatal("prior validation receipt replaced")
		}
	}
	ciRounds, err := database.GetRoundsByStep(ci.ID)
	if err != nil || len(ciRounds) < len(priorCIRounds) || !reflect.DeepEqual(ciRounds[:len(priorCIRounds)], priorCIRounds) || ciRounds[0].SelectionSource != nil {
		t.Fatalf("conflict history was decided or lost: %#v %v", ciRounds, err)
	}
	if final.ID != parked.ID || deref(final.PRURL) != deref(parked.PRURL) {
		t.Fatal("follow-up replaced run or PR identity")
	}
	t.Logf("after handback: released=%s accepted=%s validated=%s run=%s PR=%s", parked.HeadSHA, followup, finalHead, final.ID, deref(final.PRURL))
}

func TestAxiOrdinaryCIHandbackRevalidatesAuthoredCorrection(t *testing.T) {
	ordinaryCIHandbackRevalidatesAuthoredCorrection(t, false)
}

func TestAxiPostDocumentCIHandbackRevalidatesAuthoredCorrection(t *testing.T) {
	ordinaryCIHandbackRevalidatesAuthoredCorrection(t, true)
}

func ordinaryCIHandbackRevalidatesAuthoredCorrection(t *testing.T, hasDocumentCommit bool) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	if hasDocumentCommit {
		original, err := os.ReadFile(h.Scenario)
		if err != nil {
			t.Fatal(err)
		}
		documentAction := `actions:
  - match: "report only what you could not resolve.\n\nContext:\n- branch: feature/ordinary-handback"
    text: "document the reviewed feature"
    edits:
      - path: "README.md"
        old: "# e2e\n"
        new: "# e2e\n\nAccurate feature usage.\n"
    structured:
      findings: []
      summary: "document accurate feature usage"
`
		if err := os.WriteFile(h.Scenario, []byte(documentAction+strings.TrimPrefix(string(original), "actions:\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const remoteURL = "https://github.com/example/ordinary-handback.git"
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	mustGit := func(directory string, args ...string) string {
		t.Helper()
		out, err := h.runGit(t.Context(), directory, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mustGit(h.WorkDir, "remote", "set-url", "origin", remoteURL)
	statePath := filepath.Join(filepath.Dir(h.AgentLog), "ordinary-forge.json")
	t.Setenv("FAKEAGENT_GH_MODE", "stateful-pr")
	t.Setenv("FAKEAGENT_GH_PARENT", "example/ordinary-handback")
	t.Setenv("FAKEAGENT_GH_STATE", statePath)
	const branch = "feature/ordinary-handback"
	state := map[string]any{
		"prs": map[string]any{branch: map[string]any{
			"number": 100, "url": "https://github.com/example/ordinary-handback/pull/100",
			"title": "fixture", "body": "fixture", "base": "main", "head": branch,
			"state": "OPEN", "mergeable": "MERGEABLE",
		}},
	}
	writeState := func(conclusion string) {
		t.Helper()
		status := "COMPLETED"
		if conclusion == "" {
			status = "IN_PROGRESS"
		}
		state["checks"] = []map[string]any{{"__typename": "CheckRun", "databaseId": 123, "name": "fixture tests", "status": status, "conclusion": conclusion, "detailsUrl": "https://github.com/example/ordinary-handback/actions/runs/10/job/123", "checkSuite": map[string]any{"app": map[string]string{"slug": "github-actions"}}}}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeState("FAILURE")
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\ncommands:\n  test: git diff --check\n  lint: git diff --check\n", "configure deterministic fixture validation")
	mustGit(h.WorkDir, "push", "origin", "main")
	h.CommitChange("init-ordinary", "seed.txt", "seed\n", "seed init")
	initFromOwnWorktree(t, h, "init-ordinary")
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add feature")
	operator := h.AddWorktree(branch)
	if out, err := h.RunInDir(operator, "axi", "run", "--intent", "correct failed CI while retaining prior pipeline fixes"); err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("initial review: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil || !strings.Contains(out, "fix_review") {
		t.Fatalf("initial repair: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "approve"); err != nil || !strings.Contains(out, "fixture tests") {
		t.Fatalf("ordinary CI failure: %v\n%s", err, out)
	}
	parked := h.ActiveRun(branch)
	if parked == nil || parked.HeadSHA == submitted {
		t.Fatalf("fixture has no retained repair: %#v", parked)
	}
	ci, ok := findStep(parked.Steps, types.StepCI)
	if !ok || ci.Status != types.StepStatusAwaitingApproval || ci.AutoFixLimit != 0 || ci.FixRoundCount != 0 {
		t.Fatalf("CI did not park without repairs: %#v", ci)
	}
	if out, err := h.RunInDir(operator, "axi", "sync"); err != nil {
		t.Fatalf("synchronize retained output: %v\n%s", err, out)
	}
	database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	before, err := database.GetRun(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hasDocumentCommit {
		if before.ReviewApprovedHeadSHA == nil || *before.ReviewApprovedHeadSHA == before.HeadSHA || !strings.Contains(mustGit(operator, "show", before.HeadSHA+":README.md"), "Accurate feature usage.") {
			t.Fatalf("Document did not commit after Review and Test: %#v", before)
		}
		if out, err := h.RunInDir(operator, "axi", "handback"); err == nil {
			t.Fatalf("default route accepted an unreviewed publication: %s", out)
		}
	}
	review, _ := findStep(parked.Steps, types.StepReview)
	priorReview, err := database.GetRoundsByStep(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	priorCI, err := database.GetRoundsByStep(ci.ID)
	if err != nil {
		t.Fatal(err)
	}
	invocations := len(h.AgentInvocations())
	bindingPath := filepath.Join(t.TempDir(), "expected.json")
	caller, err := filepath.EvalSymlinks(operator)
	if err != nil {
		t.Fatal(err)
	}
	binding := ipc.CIHandbackBinding{RepoID: before.RepoID, RunID: before.ID, Branch: branch, CallerDir: caller, CallerHead: before.HeadSHA, CurrentHead: before.HeadSHA, PublishedHead: *before.LastPushedSHA, PushGeneration: *before.PushGeneration}
	writeBinding := func() {
		t.Helper()
		data, err := json.Marshal(binding)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bindingPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binding.RunID = "stale-run"
	writeBinding()
	if out, err := h.RunInDir(operator, "axi", "handback", "--expect", bindingPath); err == nil {
		t.Fatalf("stale wrapper binding adopted a newer active run: %s", out)
	}
	binding.RunID = before.ID
	writeBinding()
	for range 2 {
		arguments := []string{"axi", "handback", "--expect", bindingPath}
		if hasDocumentCommit {
			arguments = append(arguments, "--revalidate-publication")
		}
		if out, err := h.RunInDir(operator, arguments...); err != nil || !strings.Contains(out, "released") || !strings.Contains(out, "receipt:") {
			t.Fatalf("ordinary release/replay: %v\n%s", err, out)
		}
	}
	receipt, err := database.LatestCIHandback(before.ID)
	if err != nil || receipt == nil || receipt.Source == nil {
		t.Fatalf("release has no durable ordinary receipt: %#v %v", receipt, err)
	}
	if len(h.AgentInvocations()) != invocations {
		t.Fatal("release launched a fixer or validation agent")
	}
	if current, err := database.GetRun(parked.ID); err != nil || !reflect.DeepEqual(before, current) {
		t.Fatalf("release changed failed run: %#v %v", current, err)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "approve"); err == nil {
		t.Fatalf("released failure was approved: %s", out)
	}
	if out, err := h.Run("daemon", "restart", "--force"); err != nil {
		t.Fatalf("disposable daemon restart: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "status"); err != nil || !strings.Contains(out, "resume_ci_handback") {
		t.Fatalf("released source lost on restart: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(operator, "correction.txt"), []byte("corrected fixture expectation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(operator, "add", "correction.txt")
	mustGit(operator, "commit", "-m", "correct failed fixture expectation")
	correction := mustGit(operator, "rev-parse", "HEAD")
	binding.CallerHead = correction
	writeBinding()
	if mustGit(h.UpstreamDir, "rev-parse", "refs/heads/"+branch) != parked.HeadSHA {
		t.Fatal("author correction published before revalidation")
	}
	if err := os.WriteFile(h.Scenario, []byte(`actions:
  - text: clean validation
    structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: validated fixture correction
      risk_scope: source-or-external
      tested: [live fixture]
      testing_summary: passed
      scenarios:
        - name: live fixture
          result: pass
          live: true
          evidence: disposable source and service
          reason: ""
      verdict: go
      artifacts: []
      title: correct fixture
      body: retain prior repairs
`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeState("")
	if out, err := h.RunInDir(operator, "axi", "handback", "--resume", "--receipt", "wrong-receipt", "--expect", bindingPath); err == nil {
		t.Fatalf("resume consumed a different receipt: %s", out)
	}
	if out, err := h.RunInDir(operator, "axi", "handback", "--resume", "--receipt", receipt.ID, "--expect", bindingPath, "--wait", "1s"); err == nil || !strings.Contains(out, "wait of 1s elapsed") || !strings.Contains(out, "handback: accepted") || !strings.Contains(out, receipt.ID) {
		h.dumpDebugState()
		t.Fatalf("bounded submission lost its accepted checkpoint: %v\n%s", err, out)
	}
	writeState("SUCCESS")
	final := h.RunInfo(parked.ID)
	deadline := time.Now().Add(3 * time.Minute)
	for !final.CIReady && final.Status == types.RunRunning && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		final = h.RunInfo(parked.ID)
	}
	if !final.CIReady {
		h.dumpDebugState()
		t.Fatalf("normal revalidation/publication did not reach green CI: %#v", final)
	}
	invocations = len(h.AgentInvocations())
	subdirectory := filepath.Join(operator, "nested")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(subdirectory, "axi", "handback", "--resume", "--receipt", receipt.ID, "--expect", bindingPath); err != nil || !strings.Contains(out, "checks-passed") || !strings.Contains(out, receipt.ID) || len(h.AgentInvocations()) != invocations {
		h.dumpDebugState()
		t.Fatalf("exact accepted replay restarted validation or lost its receipt: %v\n%s", err, out)
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		step, ok := findStep(final.Steps, name)
		if !ok || step.Status != types.StepStatusCompleted || step.RoundCount < 2 {
			t.Fatalf("missing new %s validation: %#v", name, step)
		}
	}
	for _, head := range []string{submitted, parked.HeadSHA, correction} {
		mustGit(operator, "merge-base", "--is-ancestor", head, final.HeadSHA)
	}
	for _, original := range []struct {
		step   string
		rounds []*db.StepRound
	}{{review.ID, priorReview}, {ci.ID, priorCI}} {
		rounds, err := database.GetRoundsByStep(original.step)
		if err != nil || len(rounds) < len(original.rounds) || !reflect.DeepEqual(rounds[:len(original.rounds)], original.rounds) || (original.step == review.ID && len(rounds) == len(original.rounds)) {
			t.Fatalf("original history or spent rounds changed: %#v %v", rounds, err)
		}
	}
	after, err := database.GetRun(parked.ID)
	if err != nil || after.ID != before.ID || after.SubmittedHeadSHA == nil || *after.SubmittedHeadSHA != *before.SubmittedHeadSHA || *after.PushGeneration != *before.PushGeneration+1 || final.PRURL == nil || *final.PRURL != *parked.PRURL {
		t.Fatalf("identity/publication history changed: %#v %v", after, err)
	}
	finalCI, _ := findStep(final.Steps, types.StepCI)
	if !final.CIReady || after.CIReadyAt == nil || finalCI.RoundStartedAt == nil || ci.RoundStartedAt == nil || *finalCI.RoundStartedAt <= *ci.RoundStartedAt || *after.CIReadyAt < *finalCI.RoundStartedAt || after.LastPushedSHA == nil || *after.LastPushedSHA != final.HeadSHA || after.ReviewApprovedHeadSHA == nil || *after.ReviewApprovedHeadSHA != final.HeadSHA || finalCI.FixRoundCount != 0 || finalCI.AutoFixLimit != 0 || mustGit(h.UpstreamDir, "rev-parse", "refs/heads/"+branch) != final.HeadSHA {
		t.Fatalf("new published head lacks green CI or gained repair allowance: %#v", final)
	}
	if out, err := h.RunInDir(operator, "axi", "abort", "--run", final.ID); err != nil || !strings.Contains(out, "cancelled") {
		t.Fatalf("disposable handback cancellation: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "handback", "--resume", "--receipt", receipt.ID, "--expect", bindingPath); err == nil {
		t.Fatalf("cancelled handback was resurrected: %s", out)
	}
	t.Logf("same run %s retained failed CI and prior repair; released %s, authored %s, fully validated and published %s; cancellation refused resurrection", final.ID, parked.HeadSHA, correction, final.HeadSHA)
}
