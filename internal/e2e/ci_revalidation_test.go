//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func TestCIRevalidationAdoptsRetainedFailedPublicationWithoutAnotherFixer(t *testing.T) {
	started := time.Now()
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
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			value := os.Getenv(name)
			t.Cleanup(func() { _ = os.Setenv(name, value) })
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GIT_CONFIG_COUNT", "")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(isolation, "gitconfig"))
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
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "side_by_side: true\nsession_reuse: true\n"})
	t.Cleanup(func() {
		if t.Failed() {
			h.dumpDebugState()
		}
	})
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
	lostSelection := filepath.Join(isolation, "lost-publication-selection")
	publicationCrash := filepath.Join(isolation, "crash-after-public-push")
	publicationReject := filepath.Join(isolation, "reject-selected-push")
	publicationProof := filepath.Join(isolation, "publication-proof")
	if err := os.WriteFile(publicationCrash, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(isolation, "publication-gh")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "hosts.yml"), []byte("github.com:\n  user: fixture-operator\n  users:\n    fixture-operator: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reject, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"FAKE_CLI_MODE": "ci-revalidation", "FAKE_CLI_REAL_GIT": realGit,
		"FAKE_CLI_REVALIDATION_REMOTE": h.UpstreamDir, "FAKE_CLI_REVALIDATION_BRANCH": branch,
		"FAKE_CLI_REVALIDATION_URL": origin, "FAKE_CLI_REVALIDATION_REJECT": reject,
		"FAKE_CLI_PR_BODY_FILE":       filepath.Join(isolation, "pr-body"),
		"FAKE_CLI_LOG":                filepath.Join(isolation, "forge.log"),
		"FAKE_CLI_BASELINE_LOG":       filepath.Join(isolation, "baseline-count"),
		"FAKE_CLI_PUBLICATION_CONFIG": profile,
		"FAKE_CLI_PUBLICATION_SECRET": strings.Repeat("fixture-credential-", 4),
		"FAKE_CLI_PUBLICATION_LOST":   lostSelection,
		"FAKE_CLI_PUBLICATION_CRASH":  publicationCrash,
		"FAKE_CLI_PUBLICATION_REJECT": publicationReject,
		"FAKE_CLI_PUBLICATION_PROOF":  publicationProof,
	} {
		t.Setenv(key, value)
	}
	if err := os.WriteFile(filepath.Join(isolation, "pr-body"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", origin); err != nil {
		t.Fatalf("set fake origin: %v\n%s", err, output)
	}
	configuration := "allow_repo_commands: true\nauto_fix:\n  ci: 1\ncommands:\n  test: baseline-check\n  lint: lint-check\ncommit:\n  trailers: []\n"
	h.CommitChange("main", ".no-mistakes.yaml", configuration, "configure the disposable CI repair")
	if output, err := h.runGit(t.Context(), h.WorkDir, "push", h.UpstreamDir, "main"); err != nil {
		t.Fatalf("publish trusted fixture config: %v\n%s", err, output)
	}
	if output, err := h.Run("init"); err != nil {
		t.Fatalf("initialize isolated fixture: %v\n%s", err, output)
	}
	submitted := h.CommitChange(branch, "feature.txt", "feature\n", "add the fixture feature")
	caller := h.AddWorktree(branch)
	output, err := h.RunInDir(caller, "axi", "run", "--intent", "retain and revalidate the exact failed-publication candidate", "--wait", "45s")
	if err != nil && !strings.Contains(output, "elapsed while driving the run") {
		t.Fatalf("initial public run: %v\n%s", err, output)
	}
	t.Logf("initial public CLI result:\n%s", output)
	initialDeadline := time.Now().Add(75 * time.Second)
	isCIParked := false
	for time.Now().Before(initialDeadline) {
		active := h.ActiveRun(branch)
		if active == nil || active.Status.Terminal() {
			t.Fatalf("fixture left active validation before CI: %+v", active)
		}
		var gate *ipc.StepResultInfo
		for index := range active.Steps {
			step := &active.Steps[index]
			if step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview {
				gate = step
				break
			}
		}
		if gate == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if gate.StepName == types.StepCI {
			isCIParked = true
			break
		}
		if gate.FindingsJSON == nil {
			t.Fatalf("fixture gate has no recorded findings: %+v", gate)
		}
		findings, parseErr := types.ParseFindingsJSON(*gate.FindingsJSON)
		if parseErr != nil || len(findings.Items) != 0 {
			t.Fatalf("fixture requires an unexpected decision: %v\n%s", parseErr, *gate.FindingsJSON)
		}
		wait := min(45*time.Second, time.Until(initialDeadline))
		output, err = h.RunInDir(caller, "axi", "respond", "--action", "approve", "--wait", wait.String())
		if err != nil && !strings.Contains(output, "elapsed while driving the run") {
			t.Fatalf("approve clean fixture gate: %v\n%s", err, output)
		}
	}
	if !isCIParked {
		t.Fatalf("initial run never reached its preserved CI gate: %+v", h.ActiveRun(branch))
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
	t.Logf("retained failed-publication state: same run %s; public/recorded %s; retained candidate %s", run.ID, submitted, candidate)
	fixerCount := 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "CI checks have failed") {
			fixerCount++
		}
	}
	if fixerCount != 1 {
		t.Fatalf("repair invocations = %d, want the original automatic 1/1 only", fixerCount)
	}
	originalSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalRounds := make(map[string][]*db.StepRound)
	for _, step := range originalSteps {
		originalRounds[step.ID], err = database.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	// The base advances while the retained candidate is held. Recovery must
	// neither integrate it automatically nor invent an approval for it.
	h.CommitChange("main", "feature.txt", "conflicting base change\n", "advance the fixture default branch")
	if output, err := h.runGit(t.Context(), h.WorkDir, "push", h.UpstreamDir, "main"); err != nil {
		t.Fatalf("advance default branch: %v\n%s", err, output)
	}
	crashCIRevalidationDaemon(t, h)
	if output, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("recover disposable daemon: %v\n%s", err, output)
	}
	if active := h.ActiveRun(branch); active == nil || active.ID != run.ID || active.Status != types.RunRunning {
		t.Fatalf("startup lost retained run: %+v", active)
	}
	for _, action := range []string{"approve", "fix"} {
		arguments := []string{"axi", "respond", "--action", action, "--wait", "5s"}
		if action == "fix" {
			arguments = append(arguments, "--findings", "ci-1")
		}
		if output, err := h.RunInDir(caller, arguments...); err == nil {
			t.Fatalf("ordinary %s consumed hold:\n%s", action, output)
		}
	}
	adoptArgs := []string{"axi", "respond", "--action", "revalidate", "--candidate", candidate, "--reason", "adopt these exact unvalidated contents for fresh required validation", "--publication-gh-config-dir", profile, "--publication-login", "fixture-operator", "--wait", "30s"}
	beforeRefusals, err := pipeline.ReadCIRepairSnapshot(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"candidate", "login", "dirty caller"} {
		arguments := append([]string{}, adoptArgs...)
		switch invalid {
		case "candidate":
			arguments[5] = submitted
		case "login":
			arguments[11] = "unselected-account"
		case "dirty caller":
			if err := os.WriteFile(filepath.Join(caller, "uncommitted.txt"), []byte("keep this work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if output, err := h.RunInDir(caller, arguments...); err == nil {
			t.Fatalf("%s adoption was accepted:\n%s", invalid, output)
		}
		if invalid == "dirty caller" {
			if err := os.Remove(filepath.Join(caller, "uncommitted.txt")); err != nil {
				t.Fatal(err)
			}
		}
		after, err := pipeline.ReadCIRepairSnapshot(database, run.ID)
		if err != nil || !reflect.DeepEqual(beforeRefusals, after) || h.UpstreamBranchSHA(branch) != submitted {
			t.Fatalf("%s refusal mutated original state: %v", invalid, err)
		}
		if event, err := database.GetCIRepairAdoption(run.ID); err != nil || event != nil {
			t.Fatalf("%s refusal wrote adoption: %v", invalid, err)
		}
	}
	for _, mutation := range []string{"caller contents", "managed contents", "caller writer", "managed writer", "caller branch", "caller head", "origin", "gate ref"} {
		t.Logf("elapsed %s: begin mutable proof %s", time.Since(started).Round(time.Millisecond), mutation)
		if err := os.WriteFile(publicationProof+".arm", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		type response struct {
			output string
			err    error
		}
		finished := make(chan response, 1)
		go func() {
			output, err := h.RunInDir(caller, adoptArgs...)
			finished <- response{output, err}
		}()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(publicationProof + ".pending"); err == nil {
				break
			}
			select {
			case result := <-finished:
				t.Fatalf("%s proof never reached forge read: %v\n%s", mutation, result.err, result.output)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s proof never reached forge read", mutation)
			}
			time.Sleep(10 * time.Millisecond)
		}
		var changedPath string
		switch mutation {
		case "caller contents", "managed contents", "caller writer", "managed writer":
			directory := caller
			if strings.HasPrefix(mutation, "managed") {
				directory = stored.WorktreePath()
			}
			changedPath = filepath.Join(directory, "during-proof.txt")
			if strings.HasSuffix(mutation, "writer") {
				path, err := h.runGit(t.Context(), directory, "rev-parse", "--git-path", "index.lock")
				if err != nil {
					t.Fatal(err)
				}
				changedPath = strings.TrimSpace(string(path))
				if !filepath.IsAbs(changedPath) {
					changedPath = filepath.Join(directory, changedPath)
				}
			}
			if err := os.WriteFile(changedPath, []byte("preserve this writer\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		case "caller branch":
			if output, err := h.runGit(t.Context(), caller, "switch", "-c", "feature/proof-moved"); err != nil {
				t.Fatalf("move caller branch: %v\n%s", err, output)
			}
		case "caller head":
			if output, err := h.runGit(t.Context(), caller, "commit", "--allow-empty", "-m", "move disposable caller during proof"); err != nil {
				t.Fatalf("move caller head: %v\n%s", err, output)
			}
		case "origin":
			if output, err := h.runGit(t.Context(), caller, "remote", "set-url", "origin", "https://github.com/example/changed.git"); err != nil {
				t.Fatalf("move origin: %v\n%s", err, output)
			}
		case "gate ref":
			if output, err := h.runGit(t.Context(), stored.WorktreePath(), "update-ref", "refs/heads/"+branch, candidate, submitted); err != nil {
				t.Fatalf("move gate ref: %v\n%s", err, output)
			}
		}
		if err := os.WriteFile(publicationProof+".release", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		result := <-finished
		if result.err == nil {
			t.Fatalf("adoption accepted %s changed during its forge read:\n%s", mutation, result.output)
		}
		if changedPath != "" {
			if err := os.Remove(changedPath); err != nil {
				t.Fatal(err)
			}
		}
		switch mutation {
		case "caller branch":
			if output, err := h.runGit(t.Context(), caller, "switch", branch); err != nil {
				t.Fatalf("restore caller branch: %v\n%s", err, output)
			}
		case "caller head":
			if output, err := h.runGit(t.Context(), caller, "update-ref", "refs/heads/"+branch, submitted); err != nil {
				t.Fatalf("restore fixture caller: %v\n%s", err, output)
			}
		case "origin":
			if output, err := h.runGit(t.Context(), caller, "remote", "set-url", "origin", origin); err != nil {
				t.Fatalf("restore fixture origin: %v\n%s", err, output)
			}
		case "gate ref":
			if output, err := h.runGit(t.Context(), stored.WorktreePath(), "update-ref", "refs/heads/"+branch, submitted, candidate); err != nil {
				t.Fatalf("restore fixture gate ref: %v\n%s", err, output)
			}
		}
		if err := os.Remove(publicationProof + ".pending"); err != nil {
			t.Fatal(err)
		}
		after, err := pipeline.ReadCIRepairSnapshot(database, run.ID)
		if err != nil || !reflect.DeepEqual(beforeRefusals, after) || h.UpstreamBranchSHA(branch) != submitted {
			t.Fatalf("%s mutation refusal changed original state: %v", mutation, err)
		}
		if event, err := database.GetCIRepairAdoption(run.ID); err != nil || event != nil {
			t.Fatalf("%s mutation refusal wrote adoption: %v", mutation, err)
		}
	}
	output, err = h.RunInDir(caller, adoptArgs...)
	t.Logf("elapsed %s: initial adoption returned", time.Since(started).Round(time.Millisecond))
	if err != nil && !strings.Contains(output, "elapsed while driving the run") {
		t.Fatalf("explicit same-run adoption is unsupported: %v\n%s", err, output)
	}
	waitCIRevalidationGate(t, h, branch, types.StepPush, 75*time.Second)
	event, err := database.GetCIRepairAdoption(run.ID)
	if err != nil || event == nil || event.CandidateSHA != candidate || event.Operator == "" || !strings.HasPrefix(event.Operator, "authenticated local IPC process ") {
		t.Fatalf("missing actual operator adoption: event=%+v err=%v", event, err)
	}
	fresh, err := database.GetRun(run.ID)
	if err != nil || fresh.ReviewApprovedHeadSHA == nil || *fresh.ReviewApprovedHeadSHA != candidate || fresh.HeadSHA != candidate || h.UpstreamBranchSHA(branch) != submitted {
		t.Fatalf("adoption fabricated validation/publication: %+v err=%v", fresh, err)
	}
	// Replaying adoption must preserve the fresh validation already completed
	// before the publication credential was lost.
	freshSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	output, err = h.RunInDir(caller, adoptArgs...)
	if err != nil {
		t.Fatalf("adoption replay: %v\n%s", err, output)
	}
	afterReplay, err := database.GetStepsByRun(run.ID)
	if err != nil || !reflect.DeepEqual(freshSteps, afterReplay) {
		t.Fatalf("replay changed fresh validation: %v", err)
	}
	for _, step := range afterReplay {
		if step.StepName == types.StepReview || step.StepName == types.StepTest {
			rounds, err := database.GetRoundsByStep(step.ID)
			if err != nil || len(rounds) <= len(originalRounds[step.ID]) || rounds[len(originalRounds[step.ID])].Trigger != "initial" {
				t.Fatalf("cold fresh %s did not run: %v", step.StepName, err)
			}
		}
		if step.StepName == types.StepTest {
			findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil || findings.TestedHeadSHA != candidate || step.ApprovalReason != nil || step.OverrideReason != nil {
				t.Fatalf("old Test authority survived adoption: %+v %v", step, err)
			}
		}
	}
	for _, action := range []string{"approve", "fix"} {
		arguments := []string{"axi", "respond", "--action", action, "--wait", "5s"}
		if action == "fix" {
			arguments = append(arguments, "--findings", pipeline.PublicationSelectionFindingID)
		}
		if output, err := h.RunInDir(caller, arguments...); err == nil {
			t.Fatalf("ordinary %s consumed publication selection hold:\n%s", action, output)
		}
	}
	if err := os.Remove(lostSelection); err != nil {
		t.Fatal(err)
	}
	reselectArgs := append(append([]string{}, adoptArgs...), "--reselect-publication")
	if err := os.WriteFile(publicationReject, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = h.RunInDir(caller, reselectArgs...)
	t.Logf("elapsed %s: selected push rejection returned", time.Since(started).Round(time.Millisecond))
	if err != nil && !strings.Contains(output, "elapsed while driving the run") {
		t.Fatalf("failed selected publication did not remain recoverable: %v\n%s", err, output)
	}
	waitCIRevalidationGate(t, h, branch, types.StepPush, 45*time.Second)
	if _, err := os.Stat(publicationReject); !os.IsNotExist(err) {
		t.Fatal("selected Git publication was never attempted after credential proof")
	}
	fresh, err = database.GetRun(run.ID)
	if err != nil || fresh.Status != types.RunRunning || fresh.AwaitingAgentSince == nil || fresh.PushGeneration == nil || *fresh.PushGeneration != *stored.PushGeneration || h.UpstreamBranchSHA(branch) != submitted {
		t.Fatalf("failed selected push did not preserve the run and old receipt: %+v, %v", fresh, err)
	}
	failedSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index, step := range failedSteps {
		if step.StepName.Order() < types.StepPush.Order() && !reflect.DeepEqual(step, afterReplay[index]) {
			t.Fatalf("failed publication reset completed %s validation", step.StepName)
		}
	}
	// The fake forge accepts only the selected Git environment, then kills
	// this disposable daemon after the remote update but before its SQL receipt.
	_, _ = h.RunInDir(caller, reselectArgs...)
	t.Logf("elapsed %s: publication crash returned", time.Since(started).Round(time.Millisecond))
	fresh, err = database.GetRun(run.ID)
	if err != nil || fresh.PushGeneration == nil || *fresh.PushGeneration != *stored.PushGeneration || fresh.LastPushedSHA == nil || *fresh.LastPushedSHA != submitted || h.UpstreamBranchSHA(branch) != candidate || !fresh.PushActive {
		t.Fatalf("publication crash boundary not reached: %+v %v", fresh, err)
	}
	if output, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("recover after exact publication crash: %v\n%s", err, output)
	}
	t.Logf("elapsed %s: begin final publication reselection", time.Since(started).Round(time.Millisecond))
	output, err = h.RunInDir(caller, reselectArgs...)
	t.Logf("elapsed %s: final reselection returned: %v\n%s", time.Since(started).Round(time.Millisecond), err, output)
	if err != nil && !strings.Contains(output, "elapsed while driving the run") {
		t.Fatalf("retry original publication after crash: %v\n%s", err, output)
	}
	waitCIRevalidationGate(t, h, branch, types.StepCI, 45*time.Second)
	restoredSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index, step := range restoredSteps {
		if step.StepName.Order() < types.StepPush.Order() && !reflect.DeepEqual(step, afterReplay[index]) {
			t.Fatalf("publication reselection reset completed %s validation", step.StepName)
		}
	}
	fresh, err = database.GetRun(run.ID)
	if err != nil || fresh.LastPushedSHA == nil || *fresh.LastPushedSHA != candidate || fresh.ReviewApprovedHeadSHA == nil || *fresh.ReviewApprovedHeadSHA != candidate || h.UpstreamBranchSHA(branch) != candidate {
		t.Fatalf("freshly validated candidate was not published: %+v err=%v\n%s", fresh, err, output)
	}
	if fresh.CIReadyAt != nil {
		t.Fatal("unresolved default-branch conflict was reported as ready")
	}
	if *fresh.PushGeneration != *stored.PushGeneration+1 {
		t.Fatal("publication replay recorded more than one generation")
	}
	if output, err := h.runGit(t.Context(), h.UpstreamDir, "merge-tree", "--write-tree", candidate, "refs/heads/main"); err == nil {
		t.Fatalf("fixture failed to retain a real base conflict:\n%s", output)
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
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) < len(originalRounds[step.ID]) || !reflect.DeepEqual(rounds[:len(originalRounds[step.ID])], originalRounds[step.ID]) {
			t.Fatalf("original %s history changed", step.StepName)
		}
		if step.StepName == types.StepReview {
			if len(rounds) < 2 || rounds[len(originalRounds[step.ID])].Trigger != "initial" {
				t.Fatalf("fresh Review did not run: rounds=%d err=%v\n%s", len(rounds), err, output)
			}
		}
		if step.StepName == types.StepCI {
			selected := 0
			for _, round := range rounds {
				if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceAutoFix {
					selected++
				}
			}
			if selected != 1 || step.AutoFixLimit == nil || *step.AutoFixLimit != 1 {
				t.Fatalf("CI budget renewed: selections=%d limit=%v", selected, step.AutoFixLimit)
			}
		}
	}
	baselines, err := os.ReadFile(filepath.Join(isolation, "baseline-count"))
	if err != nil || !strings.Contains(string(baselines), candidate) || len(strings.Fields(string(baselines))) < 2 {
		t.Fatalf("fresh candidate baseline never ran: %v", err)
	}
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "CI checks have failed") {
			continue
		}
		if strings.Contains(invocation.Prompt, "ci-repair.txt") && strings.Contains(strings.Join(invocation.Args, " "), "--resume") {
			t.Fatal("fresh validation reused the old reviewer session")
		}
	}
	encodedEvent, err := json.Marshal(event)
	if err != nil || strings.Contains(string(encodedEvent), os.Getenv("FAKE_CLI_PUBLICATION_SECRET")) {
		t.Fatal("credential leaked into durable adoption")
	}
}

func crashCIRevalidationDaemon(t *testing.T, harness *Harness) {
	t.Helper()
	pid, err := daemon.ReadPID(paths.WithRoot(harness.NMHome))
	if err != nil || pid <= 0 {
		t.Fatalf("read owned disposable daemon PID: %d %v", pid, err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := e2edaemon.ProcessAlive(pid)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			t.Logf("ended owned disposable daemon PID %d", pid)
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("owned disposable daemon PID %d did not exit", pid)
}

func waitCIRevalidationGate(t *testing.T, harness *Harness, branch string, name types.StepName, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		active := harness.ActiveRun(branch)
		if active == nil || active.Status.Terminal() {
			t.Fatalf("run left active validation before %s: %+v", name, active)
		}
		for _, step := range active.Steps {
			if step.StepName == name && (step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run never reached its preserved %s gate: %+v", name, harness.ActiveRun(branch))
}
