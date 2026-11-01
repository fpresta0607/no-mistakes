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

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiTerminalCustodyContinuation(t *testing.T) {
	scenario := branchSyncScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: scenario, GlobalConfigExtra: "agent_config:\n  codex: {model: fixture, effort: xhigh}\nagent_args_override:\n  codex: ['-c', 'service_tier=\"default\"']\n"})
	for name, directory := range map[string]string{"USERPROFILE": h.HomeDir, "APPDATA": filepath.Join(h.HomeDir, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(h.HomeDir, "AppData", "Local")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, directory)
	}
	h.CommitChange("init-terminal", "seed.txt", "seed\n", "seed terminal fixture")
	initializer := h.AddWorktree("init-terminal")
	if out, err := h.RunInDir(initializer, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("feature/terminal", "feature.txt", "unsafe\n", "feature requiring correction")
	caller := h.AddWorktree("feature/terminal")
	profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := launchassert.Expectation{TrustedSHA: strings.TrimSpace(h.WorktreeRefSHA("main")), Profiles: map[string][]agentcfg.Selection{"primary": {profile}, "reviewer": {profile}, "fixer": {profile}}}
	assertionPath := filepath.Join(t.TempDir(), "launch.json")
	payload, err := json.Marshal(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assertionPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunInDir(caller, "axi", "run", "--intent", "validate preserved correction without replacing its run", "--launch-nonce", "terminal-fixture", "--validation-generation", "frozen-fixture-policy", "--launch-assertion", assertionPath)
	if err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("initial gate: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("create retained correction: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(caller, "axi", "abort"); err != nil {
		t.Fatalf("terminalize original run: %v\n%s", err, out)
	}
	run := h.WaitForRun("feature/terminal", 30*time.Second)
	p := paths.WithRoot(h.NMHome)
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	before, err := database.ReadTerminalSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDeadline := time.Now().Add(15 * time.Second)
	for before.Run.TerminalSourceState == nil && time.Now().Before(cleanupDeadline) {
		time.Sleep(50 * time.Millisecond)
		before, err = database.ReadTerminalSnapshot(run.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if before.Run.Status != types.RunCancelled || before.Run.TerminalSourceState == nil || *before.Run.TerminalSourceState != "clean:"+before.Run.HeadSHA {
		t.Fatalf("terminal source = %+v", before.Run)
	}
	if out, err := h.Run("daemon", "restart"); err != nil || !strings.Contains(out, "daemon restarted") {
		t.Fatalf("real isolated daemon restart: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(caller, "local-only.txt"), []byte("retained local source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = h.RunInDir(caller, "axi", "sync", "--recover", "--continue", "--reason", "validate preserved correction")
	if err != nil || !strings.Contains(out, "adopted_unvalidated: true") || !strings.Contains(out, run.ID) {
		t.Fatalf("public continuation: %v\n%s", err, out)
	}
	deadline := time.Now().Add(30 * time.Second)
	var current *db.TerminalSnapshot
	for time.Now().Before(deadline) {
		current, err = database.ReadTerminalSnapshot(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Run.AwaitingAgentSince != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if current.Run.AwaitingAgentSince == nil || current.Run.HeadSHA != before.Run.HeadSHA || current.Run.ReviewApprovedHeadSHA != nil || current.Run.TerminalHeadVerifiedAt != nil {
		t.Fatalf("fresh Review was bypassed: %+v", current.Run)
	}
	if !reflect.DeepEqual(current.Run.LaunchAssertionProof, before.Run.LaunchAssertionProof) || !reflect.DeepEqual(current.Run.LaunchValidationGeneration, before.Run.LaunchValidationGeneration) {
		t.Fatal("original native launch receipt changed")
	}
	for index, original := range before.Steps {
		if original.StepName == types.StepReview {
			if len(current.Rounds[index]) != len(before.Rounds[index])+1 {
				t.Fatal("fresh Review did not append exactly one turn")
			}
		} else if !reflect.DeepEqual(current.Rounds[index], before.Rounds[index]) {
			t.Fatalf("%s history changed before Review approval", original.StepName)
		}
	}
	if data, err := os.ReadFile(filepath.Join(caller, "local-only.txt")); err != nil || string(data) != "retained local source\n" {
		t.Fatal("caller untracked source changed")
	}
	out, err = h.RunInDir(caller, "axi", "sync", "--recover", "--continue", "--reason", "validate preserved correction")
	if err != nil || !strings.Contains(out, "started: false") {
		t.Fatalf("independent repeat: %v\n%s", err, out)
	}
	if len(h.Runs()) != 1 {
		t.Fatal("continuation created a replacement run")
	}
	content, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.NewReplacer("old: \"unsafe\"", "old: \"safe\"", "new: \"safe\"", "new: \"safer\"").Replace(string(content)))
	if err := os.WriteFile(scenario, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(caller, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("repair continued source: %v\n%s", err, out)
	}
	managed := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
	repaired := gitIn(t, h, managed, "rev-parse", "HEAD")
	if repaired == before.Run.HeadSHA {
		t.Fatal("continuation fixture did not create a later correction")
	}
	if out, err := h.RunInDir(caller, "axi", "abort"); err != nil {
		t.Fatalf("end fixture run: %v\n%s", err, out)
	}
	if preserved := gitIn(t, h, managed, "rev-parse", "HEAD"); preserved != repaired {
		t.Fatal("later committed correction was discarded during terminal cleanup")
	}
}
