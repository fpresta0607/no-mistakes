//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func TestCIRevalidationAdoptsRetainedFailedPublicationWithoutAnotherFixer(t *testing.T) {
	isolation := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME", "CFO_ROOT", "CFO_HOME", "CODE_GOBLINS_ROOT", "GOBLINS_ROOT"} {
		directory := filepath.Join(isolation, name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, directory)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(name, "")
	}
	scenario := filepath.Join(isolation, "scenario.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "CI checks have failed"
    text: "repair the failing build"
    edits:
      - path: ci-repair.txt
        new: "repaired\n"
    structured:
      summary: "repair the failing build"
      code_change_needed: true
  - text: "clean"
    structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "focused fixture"
      risk_scope: source-or-external
      reviewed_paths: [feature.txt, ci-repair.txt]
      tested: ["disposable public CLI"]
      testing_summary: "fake external services"
      scenarios:
        - name: "disposable fixture"
          result: pass
          live: true
          evidence: "fixture"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: revalidate retained repair"
      body: "Retain and revalidate the CI repair."
`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(isolation, executableName("fakecli"))
	build := exec.CommandContext(t.Context(), "go", "build", "-o", helper, "./internal/pipeline/fakecli")
	build.Dir = root
	winproc.Harden(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake services: %v\n%s", err, output)
	}
	for _, name := range []string{"gh", "git", "baseline-check", "lint-check"} {
		path := filepath.Join(h.BinDir, executableName(name))
		if name == "gh" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		link := os.Symlink
		if runtime.GOOS == "windows" {
			link = os.Link
		}
		if err := link(helper, path); err != nil {
			t.Fatal(err)
		}
	}
	const branch = "feature/retained-ci-repair"
	const origin = "https://github.com/example/revalidation.git"
	reject := filepath.Join(isolation, "reject-repair")
	if err := os.WriteFile(reject, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"FAKE_CLI_MODE": "ci-revalidation", "FAKE_CLI_REAL_GIT": realGit,
		"FAKE_CLI_REVALIDATION_REMOTE": h.UpstreamDir, "FAKE_CLI_REVALIDATION_BRANCH": branch,
		"FAKE_CLI_REVALIDATION_URL": origin, "FAKE_CLI_REVALIDATION_REJECT": reject,
		"FAKE_CLI_PR_BODY_FILE": filepath.Join(isolation, "pr-body"),
		"FAKE_CLI_LOG":          filepath.Join(isolation, "forge.log"),
		"FAKE_CLI_BASELINE_LOG": filepath.Join(isolation, "baseline-count"),
	} {
		t.Setenv(key, value)
	}
	if err := os.WriteFile(filepath.Join(isolation, "pr-body"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rewrite := (&url.URL{Scheme: "file", Path: filepath.ToSlash(h.UpstreamDir)}).String()
	if output, err := h.runGit(t.Context(), h.WorkDir, "config", "--global", "url."+rewrite+".insteadOf", origin); err != nil {
		t.Fatalf("rewrite fake origin locally: %v\n%s", err, output)
	}
	if output, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", origin); err != nil {
		t.Fatalf("set fake origin: %v\n%s", err, output)
	}
	configuration := "allow_repo_commands: true\nauto_fix:\n  ci: 1\ncommands:\n  test: baseline-check\n  lint: lint-check\ncommit:\n  trailers: []\n"
	h.CommitChange("main", ".no-mistakes.yaml", configuration, "configure the disposable CI repair")
	if output, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("publish trusted fixture config: %v\n%s", err, output)
	}
	if output, err := h.Run("init"); err != nil {
		t.Fatalf("initialize isolated fixture: %v\n%s", err, output)
	}
	submitted := h.CommitChange(branch, "feature.txt", "feature\n", "add the fixture feature")
	caller := h.AddWorktree(branch)
	output, err := h.RunInDir(caller, "axi", "run", "--intent", "retain and revalidate the exact failed-publication candidate", "--wait", "45s")
	if err != nil {
		t.Fatalf("initial public run: %v\n%s", err, output)
	}
	for gateCount := 0; gateCount < 6; gateCount++ {
		active := h.ActiveRun(branch)
		if active == nil {
			t.Fatalf("fixture run disappeared before CI:\n%s", output)
		}
		var gate *ipc.StepResultInfo
		for index := range active.Steps {
			step := &active.Steps[index]
			if step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview {
				gate = step
				break
			}
		}
		if gate == nil || gate.StepName == types.StepCI {
			break
		}
		if gate.FindingsJSON == nil {
			t.Fatalf("fixture gate has no recorded findings: %+v", gate)
		}
		findings, parseErr := types.ParseFindingsJSON(*gate.FindingsJSON)
		if parseErr != nil || len(findings.Items) != 0 {
			t.Fatalf("fixture requires an unexpected decision: %v\n%s", parseErr, *gate.FindingsJSON)
		}
		output, err = h.RunInDir(caller, "axi", "respond", "--action", "approve", "--wait", "45s")
		if err != nil && !strings.Contains(output, "elapsed while driving the run") {
			t.Fatalf("approve clean fixture gate: %v\n%s", err, output)
		}
	}
	ciGateDeadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(ciGateDeadline) {
		active := h.ActiveRun(branch)
		isParked := false
		if active != nil {
			for _, step := range active.Steps {
				isParked = isParked || (step.StepName == types.StepCI && (step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview))
			}
		}
		if isParked {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatalf("no retained active run:\n%s", output)
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(run.ID)
	if err != nil || stored == nil {
		t.Fatalf("read original run: %v", err)
	}
	candidateOutput, err := h.runGit(t.Context(), stored.WorktreePath(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read retained candidate: %v\n%s", err, candidateOutput)
	}
	candidate := strings.TrimSpace(string(candidateOutput))
	if candidate == submitted || stored.HeadSHA != submitted || stored.LastPushedSHA == nil || *stored.LastPushedSHA != submitted || h.UpstreamBranchSHA(branch) != submitted {
		t.Fatalf("failed publication was not retained: candidate=%s run=%+v\n%s", candidate, stored, output)
	}
	t.Logf("red reproduction: same run %s; public/recorded %s; retained candidate %s", run.ID, submitted, candidate)
	fixerCount := 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "CI checks have failed") {
			fixerCount++
		}
	}
	if fixerCount != 1 {
		t.Fatalf("repair invocations = %d, want the original automatic 1/1 only", fixerCount)
	}
	output, err = h.RunInDir(caller, "axi", "respond", "--action", "revalidate", "--candidate", candidate, "--reason", "adopt these exact unvalidated contents for fresh required validation", "--wait", "30s")
	if err != nil {
		t.Fatalf("explicit same-run adoption is unsupported: %v\n%s", err, output)
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		fresh, readErr := database.GetRun(run.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if fresh.HeadSHA == candidate && fresh.ReviewApprovedHeadSHA != nil && *fresh.ReviewApprovedHeadSHA == candidate {
			break
		}
		select {
		case <-t.Context().Done():
			t.Fatal(context.Cause(t.Context()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "CI checks have failed") {
			fixerCount--
		}
	}
	if fixerCount != 0 {
		t.Fatal("adoption launched another CI fixer")
	}
	if h.ActiveRun(branch).ID != run.ID {
		t.Fatal("adoption replaced the original run")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview {
			rounds, err := database.GetRoundsByStep(step.ID)
			if err != nil || len(rounds) < 2 {
				t.Fatalf("fresh Review did not run: rounds=%d err=%v\n%s", len(rounds), err, output)
			}
		}
	}
}
