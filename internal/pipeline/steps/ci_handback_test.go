package steps

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIHandbackRejectedFollowupRetainsRecoverableReservation(t *testing.T) {
	for _, failure := range []string{"post-move deadline", "post-move cancellation", "database CAS rejection", "database commit rejection"} {
		t.Run(failure, func(t *testing.T) {
			caller, base, published := setupGitRepo(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			remote := filepath.Join(t.TempDir(), "remote.git")
			gate := filepath.Join(t.TempDir(), "gate.git")
			managed := filepath.Join(t.TempDir(), "managed")
			gitCmd(t, caller, "clone", "--bare", caller, remote)
			gitCmd(t, caller, "clone", "--bare", remote, gate)
			gitCmd(t, gate, "worktree", "add", managed, "feature")
			const remoteURL = "https://github.com/test/handback.git"
			gitCmd(t, caller, "config", "--global", "url."+filepath.ToSlash(remote)+".insteadOf", remoteURL)
			gitCmd(t, caller, "remote", "add", "origin", remoteURL)
			gitCmd(t, caller, "checkout", "main")
			if err := os.WriteFile(filepath.Join(caller, "base-advance.txt"), []byte("new base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, caller, "add", "base-advance.txt")
			gitCmd(t, caller, "commit", "-m", "advance base")
			baseHead := gitCmd(t, caller, "rev-parse", "HEAD")
			gitCmd(t, caller, "push", "origin", "main")
			gitCmd(t, caller, "checkout", "feature")

			databasePath := filepath.Join(t.TempDir(), "state.sqlite")
			database, err := db.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			repo, err := database.InsertRepo(caller, remoteURL, "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", published, base)
			if err != nil {
				t.Fatal(err)
			}
			const prURL = "https://github.com/test/handback/pull/1"
			for _, err := range []error{
				database.UpdateRunStatus(run.ID, types.RunRunning),
				database.UpdateRunPRURL(run.ID, prURL),
				database.UpdateRunPRState(run.ID, "open"),
				database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(remoteURL), Ref: "refs/heads/feature"}),
				database.UpdateRunReviewApprovedHeadSHA(run.ID, published),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			ci, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(ci.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"ci-1","category":"ci-merge-conflict","severity":"error","action":"auto-fix","description":"conflict"}]}`
			priorRound, err := database.InsertStepRound(ci.ID, 1, "initial", &findings, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			selection := `["ci-1"]`
			if err := database.SetStepRoundSelection(priorRound.ID, &selection, db.RoundSelectionSourceAutoFix); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(ci.ID, 2, "auto_fix", &findings, nil, 20); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, ci.ID, types.StepStatusAwaitingApproval, 0, 30, &findings); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			linkTestBinary(t, binDir, "git")
			marker := filepath.Join(t.TempDir(), "moved")
			prList := `[{"number":1,"url":"https://github.com/test/handback/pull/1","baseRefName":"main"}]`
			environment := fakeCLIEnv(binDir, map[string]string{
				"FAKE_CLI_MODE": "ci-handback", "FAKE_CLI_STATE": "OPEN", "FAKE_CLI_CHECKS": "[]",
				"FAKE_CLI_PR_HEAD_SHA": published, "FAKE_CLI_PR_LIST_JSON": prList,
				"FAKE_CLI_MERGEABLE": "CONFLICTING", "FAKE_CLI_HANDBACK_MOVED": marker,
			})
			sctx := &pipeline.StepContext{Ctx: t.Context(), DB: database, Run: run, Repo: repo, WorkDir: managed, GateDir: gate, StepResultID: ci.ID, Config: &config.Config{}, Env: environment}
			step := &CIStep{}
			request := ipc.CIHandbackParams{RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, CallerDir: caller, HeadSHA: published}
			if _, err := step.HandbackCI(sctx, request); err != nil {
				t.Fatalf("release: %v", err)
			}
			released, err := database.LatestCIHandback(run.ID)
			if err != nil || released == nil || released.BaseHead != baseHead {
				t.Fatalf("released receipt: %#v %v", released, err)
			}
			priorRounds, err := database.GetRoundsByStep(ci.ID)
			if err != nil {
				t.Fatal(err)
			}
			priorGate, err := database.GetStepResult(ci.ID)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, caller, "merge", "--no-ff", "--no-edit", "main")
			followup := gitCmd(t, caller, "rev-parse", "HEAD")
			request.Resume, request.HeadSHA = true, followup
			connection, err := sql.Open("sqlite", databasePath+"?_pragma=foreign_keys(on)")
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			var rejectionSQL string
			switch failure {
			case "database CAS rejection":
				rejectionSQL = `CREATE TRIGGER reject_acceptance BEFORE UPDATE OF head_sha ON runs BEGIN SELECT RAISE(IGNORE); END`
			case "database commit rejection":
				rejectionSQL = `CREATE TABLE rejected_acceptances (run_id TEXT REFERENCES runs(id) DEFERRABLE INITIALLY DEFERRED);
				CREATE TRIGGER reject_acceptance AFTER UPDATE OF accepted_head ON ci_handbacks BEGIN INSERT INTO rejected_acceptances VALUES ('missing-run'); END`
			}
			if rejectionSQL != "" {
				if _, err := connection.Exec(rejectionSQL); err != nil {
					t.Fatal(err)
				}
			}
			timeout := 45 * time.Second
			if failure == "post-move deadline" {
				timeout = 20 * time.Second
				sctx.Env = append(environment, "FAKE_CLI_HANDBACK_PAUSE=head")
			} else if failure == "post-move cancellation" {
				sctx.Env = append(environment, "FAKE_CLI_HANDBACK_PAUSE=merge")
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			sctx.Ctx = ctx
			done := make(chan error, 1)
			go func() {
				_, err := step.HandbackCI(sctx, request)
				done <- err
			}()
			if failure == "post-move cancellation" {
				for {
					if _, err := os.Stat(marker); err == nil {
						if head := gitCmd(t, managed, "rev-parse", "HEAD"); head != followup {
							t.Fatalf("cancel before managed movement: %s", head)
						}
						cancel()
						break
					}
					select {
					case err := <-done:
						t.Fatalf("resume ended before post-move cancellation: %v", err)
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("failed submission was accepted")
				}
				expectedError := map[string]string{
					"post-move deadline":        "managed follow-up head changed during acceptance",
					"post-move cancellation":    "git merge",
					"database CAS rejection":    "binding changed before acceptance",
					"database commit rejection": "FOREIGN KEY constraint failed",
				}[failure]
				if !strings.Contains(err.Error(), expectedError) {
					t.Fatalf("resume failed for an unexpected reason: %v", err)
				}
				t.Logf("rejected follow-up: %v", err)
			case <-time.After(time.Minute):
				t.Fatal("resume failed to finish cleanup")
			}
			if moved, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(moved)) != followup {
				t.Fatalf("failure did not occur after real managed movement: %s %v", moved, err)
			}
			if failure == "post-move deadline" && ctx.Err() != context.DeadlineExceeded {
				t.Fatalf("resume did not reach its deadline: %v", ctx.Err())
			}
			if rejected, err := database.GetRun(run.ID); err != nil || !reflect.DeepEqual(rejected, run) {
				t.Fatalf("failed acceptance left an uncommitted database change: %#v %v", rejected, err)
			}
			if rejectionSQL != "" {
				if _, err := connection.Exec("DROP TRIGGER reject_acceptance"); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			database, err = db.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := database.GetRun(run.ID)
			if err != nil || !reflect.DeepEqual(recovered, run) {
				t.Fatalf("rejected submission changed durable run: %#v %v", recovered, err)
			}
			receipt, err := database.LatestCIHandback(run.ID)
			if err != nil || !reflect.DeepEqual(receipt, released) {
				t.Fatalf("rejected submission changed reservation: %#v %v", receipt, err)
			}
			rounds, err := database.GetRoundsByStep(ci.ID)
			if err != nil || !reflect.DeepEqual(rounds, priorRounds) {
				t.Fatalf("rejected submission changed spent budget or rounds: %#v %v", rounds, err)
			}
			parked, err := database.GetStepResult(ci.ID)
			if err != nil || !reflect.DeepEqual(parked, priorGate) {
				t.Fatalf("rejected submission changed conflict gate: %#v %v", parked, err)
			}
			for _, location := range []struct{ dir, head string }{{managed, published}, {caller, followup}, {remote, published}, {gate, published}} {
				if head := gitCmd(t, location.dir, "rev-parse", "feature"); head != location.head {
					t.Fatalf("rejected submission moved %s to %s, want %s", location.dir, head, location.head)
				}
			}
			if status := gitCmd(t, managed, "status", "--porcelain"); status != "" {
				t.Fatalf("restored worktree is dirty: %s", status)
			}
			if err := pipeline.ValidateRecoveredRun(database, recovered, []pipeline.Step{step}); err != nil {
				t.Fatalf("rejected submission cannot recover its reserved gate: %v", err)
			}
			sctx.Ctx, sctx.DB, sctx.Run, sctx.Env = t.Context(), database, recovered, environment
			result, err := step.HandbackCI(sctx, request)
			if err != nil || result.RunID != run.ID || result.PRURL != prURL || result.AcceptedHead != followup {
				t.Fatalf("retry did not accept the same follow-up: %#v %v", result, err)
			}
			if head := gitCmd(t, managed, "rev-parse", "HEAD"); head != followup {
				t.Fatalf("accepted managed head = %s, want %s", head, followup)
			}
			accepted, err := database.GetRun(run.ID)
			if err != nil || accepted.HeadSHA != followup || accepted.ReviewApprovedHeadSHA != nil || *accepted.LastPushedSHA != published || *accepted.PushGeneration != *run.PushGeneration {
				t.Fatalf("retry did not revoke old validation while preserving publication: %#v %v", accepted, err)
			}
			if err := pipeline.ValidateRecoveredRun(database, accepted, []pipeline.Step{step}); err == nil {
				t.Fatal("accepted follow-up recovered the old gate as approval")
			}
		})
	}
}
