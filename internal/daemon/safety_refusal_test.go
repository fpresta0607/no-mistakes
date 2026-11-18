package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A run held at an unresolved native safety refusal keeps its worktree, step
// logs and evidence when recovery cannot resume it and fails it, as an
// unresolved protected-path refusal keeps its worktree. A newer run on the
// branch ends terminal source retention, so only the refusal keeps the
// worktree here.
func TestSafetyRefusalKeepsWorkWhenRecoveryFailsTheRun(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "safety-crash")
	run, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), workDir, head); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	sr, err := database.InsertStepResult(run.ID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"` + types.FindingIDAgentSafetyRefusal + `","severity":"error","description":"codex exited: This content was flagged for possible cybersecurity risk.","action":"ask-user"}],"summary":"Native agent safety refusal; automatic execution stopped"}`
	if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, 1, &findings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "test.txt"), []byte("refused edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-config.DefaultEvidenceRetention - time.Hour)
	evidenceFile := filepath.Join(p.EvidenceRoot(""), run.ID, "output.txt")
	logFile := filepath.Join(p.RunLogDir(run.ID), "rebase.log")
	for _, file := range []string{evidenceFile, logFile} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("refusal diagnostic"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{file, filepath.Dir(file)} {
			if err := os.Chtimes(path, expired, expired); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitCmd(t, workDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unavailable.git"))
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{&steps.RebaseStep{}} })
	layout, err := validatedWorktreeLayout(database, p, config.DefaultGlobalConfig())
	if err != nil {
		t.Fatal(err)
	}

	recoverOnStartup(database, p, mgr, layout)
	newer, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(newer.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	mgr.removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), workDir, "test")
	policy := evidenceReapPolicy{Retention: time.Hour, MaxRuns: 1}
	reapRunLogs(database, p.LogsDir(), policy, time.Now())
	reapEvidence(database, p.EvidenceRoot(""), policy, time.Now())

	run, err = database.GetRun(run.ID)
	if err != nil || run.Status != types.RunFailed || len(mgr.executors) != 0 {
		t.Fatalf("recovery did not fail the refused run: run=%+v err=%v", run, err)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "test.txt")); err != nil || string(got) != "refused edit\n" {
		t.Errorf("the refused run's worktree was not kept: %q, %v", got, err)
	}
	for name, file := range map[string]string{"evidence": evidenceFile, "step log": logFile} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("the refused run's %s was not kept: %v", name, err)
		}
	}
}
