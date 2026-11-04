package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiHandbackBoundedWaitKeepsItsAcceptedReceipt(t *testing.T) {
	command := newRootCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	err := emitAxiWaitElapsed(command, time.Second, "no-mistakes axi run",
		toon.Field{Key: "handback", Value: "accepted"},
		toon.Field{Key: "receipt", Value: toon.NewObject(toon.Field{Key: "id", Value: "native-receipt"})},
	)
	if err == nil {
		t.Fatal("bounded wait reported completion")
	}
	for _, required := range []string{"wait of 1s elapsed", "handback: accepted", "receipt:", "id: native-receipt", "axi run"} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("accepted wait lost %q:\n%s", required, output.String())
		}
	}
}

func TestAxiHandbackExplainsThePublishedConflictContract(t *testing.T) {
	command := newRootCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"axi", "handback", "--help"})
	if err := command.Execute(); err != nil {
		t.Fatalf("operator cannot discover a published CI handback: %v", err)
	}
	for _, required := range []string{"handback", "--resume", "additive", "same run"} {
		if !strings.Contains(output.String(), required) {
			t.Errorf("help omits %q: %s", required, output.String())
		}
	}
}

func TestAxiHandbackStatusKeepsConflictAndScopesFollowupHelp(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/handback")
	chdir(t, repoDir)
	active, err := database.InsertRun(repo.ID, "feature/handback", "published", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		database.UpdateRunStatus(active.ID, types.RunRunning),
		database.UpdateRunPRURL(active.ID, "https://github.com/test/repo/pull/1"),
		database.UpdateRunPRState(active.ID, "open"),
		database.UpdateRunPublication(active.ID, db.PushBinding{HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature/handback"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	step, err := database.InsertStepResult(active.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","category":"ci-merge-conflict","severity":"error","description":"unresolved merge conflict","action":"auto-fix"}]}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(active.ID, step.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
		t.Fatal(err)
	}
	active, err = database.GetRun(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &db.CIHandback{RunID: active.ID, RoundID: round.ID, CallerDir: repoDir, ReleasedHead: "published", PRURL: *active.PRURL, BaseBranch: "main", BaseHead: "base", PushGeneration: *active.PushGeneration, TargetFingerprint: "target", PushRef: "refs/heads/feature/handback"}
	if err := database.ReleaseCIHandback(receipt, active, repo, findings); err != nil {
		t.Fatal(err)
	}
	for _, selectedRun := range []string{"", active.ID} {
		output := axiStatusOutput(t, selectedRun)
		for _, required := range []string{"ci_handback:", "released_head: published", "state: released", "awaiting_approval", "unresolved merge conflict"} {
			if !strings.Contains(output, required) {
				t.Errorf("status missing %q:\n%s", required, output)
			}
		}
		if strings.Contains(output, "axi respond") || (selectedRun == "" && !strings.Contains(output, "axi handback --resume")) || (selectedRun != "" && strings.Contains(output, "axi handback --resume")) {
			t.Fatalf("status supplied an unsafe or missing follow-up command:\n%s", output)
		}
	}
}

func TestAxiHandbackRefusesMalformedExpectationBeforeOpeningTheDaemon(t *testing.T) {
	for _, content := range []string{`{}`, `{"run_id":"stale","unexpected":true}`, `null`, `[]`, `{} {}`} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "expect.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			command := newRootCmd()
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"axi", "handback", "--expect", path})
			if err := command.Execute(); err == nil || !strings.Contains(output.String(), "invalid handback expectation") {
				t.Fatalf("expectation was not rejected before daemon access: %v\n%s", err, output.String())
			}
		})
	}
}
