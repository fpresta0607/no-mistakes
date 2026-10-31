package daemon

import (
	"context"
	"encoding/json"
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
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRespondWithoutExecutorDistinguishesMissingRunsFromUnreadableOwnership(t *testing.T) {
	for _, state := range []string{"missing", "existing", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			fixture := newEvidenceFixture(t)
			manager := NewRunManager(fixture.db, fixture.p, nil)
			runID := "missing-run"
			if state != "missing" {
				run, err := fixture.db.InsertRun(fixture.repo.ID, "feature", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				runID = run.ID
			}
			want := "no active executor for run " + runID
			if state == "unreadable" {
				if err := fixture.db.Close(); err != nil {
					t.Fatal(err)
				}
				want = "cannot read run ownership; preserve its source"
			}

			for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAbort, types.ActionAnswer} {
				for _, err := range []error{
					manager.HandleRespond(runID, types.StepReview, action, nil),
					manager.HandleRespondWithOverrides(runID, types.StepReview, action, nil, nil, nil, ""),
				} {
					if err == nil || err.Error() != want {
						t.Errorf("%s response = %v, want %q", action, err, want)
					}
				}
			}
		})
	}
}

func TestRespondToMissingRunReportsNoActiveExecutorOverIPC(t *testing.T) {
	locations, _ := startTestDaemon(t)
	client, err := ipc.Dial(locations.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAbort, types.ActionAnswer} {
		err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: "missing-run", Step: types.StepReview, Action: action}, nil)
		if err == nil || !strings.Contains(err.Error(), "no active executor for run missing-run") {
			t.Errorf("%s response to missing run = %v, want no active executor", action, err)
		}
	}
}

func TestCIRevalidationExecutorFreeAbortRetainsTheAdoptedSourceAndHistory(t *testing.T) {
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME", "CFO_ROOT", "CFO_HOME", "CODE_GOBLINS_ROOT", "GOBLINS_ROOT"} {
		t.Setenv(name, t.TempDir())
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	gate := config.Gate{Name: "fixture", After: types.StepLint, Command: "unused"}
	for _, entry := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, gate.StepName(), types.StepPush, types.StepCI, "recorded-push"} {
		t.Run(string(entry), func(t *testing.T) {
			isRecorded := entry == "recorded-push"
			if isRecorded {
				entry = types.StepPush
			}
			fixture := newEvidenceFixture(t)
			repo, public := setupTestGitRepo(t, fixture.p, fixture.db, "cancel-preserved")
			run, err := fixture.db.InsertRun(repo.ID, "feature", public, public)
			if err != nil {
				t.Fatal(err)
			}
			submitted := public
			gitCmd(t, repo.WorkingPath, "switch", "-c", "feature")
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "prior publication after submission")
			public = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature")
			gitCmd(t, repo.WorkingPath, "remote", "add", "origin", repo.UpstreamURL)
			gateDir := fixture.p.RepoDir(repo.ID)
			gitCmd(t, gateDir, "remote", "set-url", "origin", repo.UpstreamURL)
			workDir := fixture.p.WorktreeDir(repo.ID, run.ID)
			gitCmd(t, gateDir, "worktree", "add", "--detach", workDir, public)
			gitCmd(t, workDir, "config", "user.name", "Test")
			gitCmd(t, workDir, "config", "user.email", "test@test.com")
			if err := os.WriteFile(filepath.Join(workDir, "retained.txt"), []byte("retain the adopted source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, workDir, "add", "retained.txt")
			gitCmd(t, workDir, "commit", "-m", "retain the repair")
			candidate := gitOutput(t, workDir, "rev-parse", "HEAD")
			gates, err := json.Marshal([]config.Gate{gate})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range []error{
				fixture.db.SetRunWorktreeDir(run.ID, workDir),
				fixture.db.UpdateRunStatus(run.ID, types.RunRunning),
				fixture.db.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"),
				fixture.db.UpdateRunPRState(run.ID, "open"),
				fixture.db.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: public, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.UpstreamURL), Ref: "refs/heads/feature"}),
				fixture.db.SetRunGates(run.ID, string(gates)),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			names := types.AllSteps()
			names = append(names[:6:6], append([]types.StepName{gate.StepName()}, names[6:]...)...)
			findings := `{"findings":[{"id":"red","severity":"error","action":"ask-user","description":"publication failed"}]}`
			for _, name := range names {
				step, err := fixture.db.InsertStepResult(run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.db.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
					t.Fatal(err)
				}
				round, err := fixture.db.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.db.CompleteStep(step.ID, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
				if name == types.StepCI {
					selection := `["red"]`
					if err := fixture.db.SetStepRoundUserDecision(round.ID, &selection, db.RoundSelectionSourceAutoFix, nil); err != nil {
						t.Fatal(err)
					}
					if _, err := fixture.db.InsertStepRound(step.ID, 2, "auto_fix", &findings, nil, 1); err != nil {
						t.Fatal(err)
					}
					if err := fixture.db.ParkStepForApproval(run.ID, step.ID, types.StepStatusFixReview, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
				}
			}
			snapshot, err := fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.CandidateSHA, snapshot.CandidateTreeSHA = candidate, gitOutput(t, workDir, "rev-parse", candidate+"^{tree}")
			snapshot.CallerDir, snapshot.CallerHeadSHA, snapshot.ManagedDir = repo.WorkingPath, public, workDir
			snapshot.GateDir, snapshot.GateBranch = gateDir, run.Branch
			snapshot.Operator, snapshot.Reason = "authenticated fixture operator", "validate the retained contents"
			var adoption *db.CIRepairAdoption
			if entry != types.StepCI {
				adoption, _, err = fixture.db.AdoptCIRepairForRevalidation(*snapshot)
				if err != nil {
					t.Fatal(err)
				}
				for _, original := range snapshot.Steps {
					step := original.Snapshot
					if step.StepName.Order() < types.StepReview.Order() {
						continue
					}
					if step.StepName == entry {
						if err := fixture.db.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
							t.Fatal(err)
						}
						break
					} else if err := fixture.db.CompleteStep(step.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			if isRecorded {
				if err := fixture.db.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: candidate, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.UpstreamURL), Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, gateDir, "update-ref", "refs/heads/feature", candidate, public)
			}
			before, err := fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manager := NewRunManager(fixture.db, fixture.p, nil)
			if entry == types.StepPush {
				request := &ipc.CIRevalidationParams{CallerDir: repo.WorkingPath, CallerHeadSHA: public, CandidateSHA: candidate, ReselectPublication: true}
				if _, err := manager.proveCIRepairLocal(t.Context(), before, request, candidate); err != nil {
					t.Fatalf("accepted old-public caller was refused after receipt=%t: %v", isRecorded, err)
				}
				request.CallerHeadSHA = submitted
				if _, err := manager.proveCIRepairLocal(t.Context(), before, request, candidate); err == nil {
					t.Fatal("reselection accepted a different caller head")
				}
			}
			plans, preserved, err := manager.recoverableParkedRuns(t.Context())
			if err != nil || len(plans) != 0 {
				t.Fatalf("preserved recovery unexpectedly created an executor: %v", err)
			}
			if _, ok := preserved[run.ID]; !ok {
				t.Fatal("startup lost adoption ownership")
			}

			if err := manager.HandleCancel(run.ID); err != nil {
				t.Fatal(err)
			}

			stored, err := fixture.db.GetRun(run.ID)
			if err != nil || stored.Status != types.RunCancelled || stored.AwaitingAgentSince != nil || stored.PushActive || stored.HeadSHA != candidate || *stored.LastPushedSHA != *before.ExpectedRun.LastPushedSHA || *stored.PushGeneration != *before.ExpectedRun.PushGeneration {
				t.Fatalf("executor-free cancellation lost the source or publication truth: %+v, %v", stored, err)
			}
			after, err := fixture.db.ReadCIRepairSnapshot(run.ID)
			if err != nil || !reflect.DeepEqual(before.Steps, after.Steps) {
				t.Fatalf("cancellation rewrote validation or spent rounds: %v", err)
			}
			retainedEvent, err := fixture.db.GetCIRepairAdoption(run.ID)
			if err != nil || !reflect.DeepEqual(adoption, retainedEvent) {
				t.Fatalf("cancellation rewrote the adoption receipt: %v", err)
			}
			if head := gitOutput(t, gateDir, "rev-parse", "refs/heads/feature"); isRecorded && head != candidate {
				t.Fatal("cancellation lost its recorded publication")
			}
			if !isRecorded {
				if head := gitOutput(t, gateDir, "rev-parse", "refs/no-mistakes/recover/"+run.ID); head != candidate {
					t.Fatal("cancellation failed to anchor the retained source")
				}
			}
			if _, err := os.Stat(workDir); !os.IsNotExist(err) {
				t.Fatalf("cancellation did not clean its worktree: %v", err)
			}
		})
	}
}

func TestCIRevalidationLocalProofRefusesEveryCallerAndManagedWriter(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	fixture := newEvidenceFixture(t)
	repo, head := setupTestGitRepo(t, fixture.p, fixture.db, "writer-proof")
	gateDir := fixture.p.RepoDir(repo.ID)
	gitCmd(t, repo.WorkingPath, "switch", "-c", "feature")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", repo.UpstreamURL)
	gitCmd(t, gateDir, "remote", "set-url", "origin", repo.UpstreamURL)
	gitCmd(t, gateDir, "branch", "feature", head)
	run, err := fixture.db.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	managed := fixture.p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, gateDir, "worktree", "add", "--detach", managed, head)
	if err := fixture.db.SetRunWorktreeDir(run.ID, managed); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.UpstreamURL), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	run, err = fixture.db.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(fixture.db, fixture.p, nil)
	snapshot := &db.CIRepairAdoption{ExpectedRun: *run, ExpectedRepo: *repo}
	request := &ipc.CIRevalidationParams{CallerDir: repo.WorkingPath, CallerHeadSHA: head}
	if gateHead, err := manager.proveCIRepairLocal(t.Context(), snapshot, request, head); err != nil || gateHead != head {
		t.Fatalf("clean local identity was refused: %s, %v", gateHead, err)
	}
	for _, directory := range []string{repo.WorkingPath, managed} {
		for _, marker := range []string{"index.lock", "HEAD.lock", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
			t.Run(filepath.Base(directory)+"/"+marker, func(t *testing.T) {
				path := resolveGitPath(directory, gitOutput(t, directory, "rev-parse", "--git-path", marker))
				if err := os.WriteFile(path, []byte(head+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(path) })

				if _, err := manager.proveCIRepairLocal(t.Context(), snapshot, request, head); err == nil {
					t.Fatal("local identity accepted an existing Git writer or interrupted operation")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("refusal changed the existing writer: %v", err)
				}
			})
		}
	}
}

func TestCIRevalidationStartupPreservesUnreadableCandidateBeforeStaleCleanup(t *testing.T) {
	locations := paths.WithRoot(t.TempDir())
	if err := locations.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(locations.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/retained.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "fix/retained", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"red","severity":"error","action":"ask-user","description":"still red"}]}`
	if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusFixReview, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, locations, nil)
	plans, preserved, err := manager.recoverableParkedRuns(context.Background())
	if err != nil || len(plans) != 0 {
		t.Fatalf("recovery plans = %v, %v", plans, err)
	}
	if _, ok := preserved[run.ID]; !ok {
		t.Fatal("unreadable candidate never entered the startup preserved set")
	}
	if _, err := database.RecoverStaleRunsExcept("crashed", preserved); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning || stored.HeadSHA != run.HeadSHA {
		t.Fatalf("source ownership changed: %+v, %v", stored, err)
	}
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
		if err := manager.HandleRespond(run.ID, types.StepCI, action, nil); err == nil {
			t.Fatalf("%s consumed hold without executor", action)
		}
	}
	if err := manager.refuseCIRepairReplacement(context.Background(), repo.ID, run.Branch); err == nil {
		t.Fatal("replacement consumed the hold")
	}
}

func TestCIRevalidationUserAbortCleansUpRecoveredPublicationHold(t *testing.T) {
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME", "CFO_ROOT", "CFO_HOME", "CODE_GOBLINS_ROOT", "GOBLINS_ROOT"} {
		t.Setenv(name, t.TempDir())
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	fixture := newEvidenceFixture(t)
	repo, head := setupTestGitRepo(t, fixture.p, fixture.db, "abort-adoption")
	run, err := fixture.db.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	workDir := fixture.p.WorktreeDir(repo.ID, run.ID)
	gateDir := fixture.p.RepoDir(repo.ID)
	gitCmd(t, gateDir, "worktree", "add", "--detach", workDir, head)
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workDir, "retained.txt"), []byte("retain this commit on abort\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "retained.txt")
	gitCmd(t, workDir, "commit", "-m", "retain the unpublished change")
	retained := gitOutput(t, workDir, "rev-parse", "HEAD")
	if err := fixture.db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetRunWorktreeDir(run.ID, workDir); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.db.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.StartStepWithAutoFixLimit(result.ID, 0); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-publication-credentials","severity":"warning","action":"ask-user","description":"credentials unavailable"}]}`
	if _, err := fixture.db.InsertStepRound(result.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.ParkStepForApproval(run.ID, result.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	run, err = fixture.db.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(fixture.root, run.ID)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(fixture.db, fixture.p, nil)
	manager.resumeRecoveredRun(recoveredRunPlan{
		run: run, repo: repo, workDir: workDir, gateDir: gateDir,
		cfg: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), agent: recoveredRunTestAgent{},
		steps:    []pipeline.Step{&mockApprovalStep{name: types.StepPush}},
		adoption: &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: head},
	})

	if err := manager.HandleCancel(run.ID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { manager.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("aborted recovered run did not finish cleanup")
	}
	stored, err := fixture.db.GetRun(run.ID)
	if err != nil || stored.Status != types.RunCancelled || stored.Error == nil || *stored.Error != types.RunCancelReasonAbortedByUser || stored.AwaitingAgentSince != nil || stored.HeadSHA != retained {
		t.Fatalf("explicit abort did not settle cancellation: %+v, %v", stored, err)
	}
	for _, directory := range []string{workDir, evidenceDir} {
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Fatalf("cancellation left its disposable directory: %s, %v", directory, err)
		}
	}
	if preserved := gitOutput(t, gateDir, "rev-parse", "refs/no-mistakes/recover/"+run.ID); preserved != retained {
		t.Fatal("abort cleanup lost the unpublished commit")
	}
	if err := manager.refuseCIRepairReplacement(t.Context(), repo.ID, run.Branch); err != nil {
		t.Fatalf("cancelled run still blocks replacement: %v", err)
	}
}

func TestCIRevalidationStartupPreservesTheReceiptUntilPushCompletes(t *testing.T) {
	locations := paths.WithRoot(t.TempDir())
	if err := locations.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(locations.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/retained.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "fix/retained", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		database.UpdateRunStatus(run.ID, types.RunRunning),
		database.UpdateRunPRURL(run.ID, "https://github.com/example/retained/pull/1"),
		database.UpdateRunPRState(run.ID, "open"),
		database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	findings := `{"findings":[{"id":"red","severity":"error","action":"ask-user","description":"still red"}]}`
	for _, name := range types.AllSteps() {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
			t.Fatal(err)
		}
		round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		if name == types.StepCI {
			selected := `["red"]`
			if err := database.SetStepRoundUserDecision(round.ID, &selected, db.RoundSelectionSourceAutoFix, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(step.ID, 2, "auto_fix", &findings, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusFixReview, 0, 1, &findings); err != nil {
				t.Fatal(err)
			}
		}
	}
	snapshot, err := database.ReadCIRepairSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.CandidateSHA, snapshot.CandidateTreeSHA = strings.Repeat("c", 40), strings.Repeat("d", 40)
	snapshot.CallerDir, snapshot.ManagedDir, snapshot.GateDir = repo.WorkingPath, t.TempDir(), locations.RepoDir(repo.ID)
	snapshot.CallerHeadSHA, snapshot.GateBranch = run.HeadSHA, run.Branch
	snapshot.Operator, snapshot.Reason = "authenticated fixture process", "fresh validation of retained contents"
	if _, _, err := database.AdoptCIRepairForRevalidation(*snapshot); err != nil {
		t.Fatal(err)
	}
	var pushID, prID, ciID string
	for _, original := range snapshot.Steps {
		step := original.Snapshot
		if step.StepName.Order() < types.StepPush.Order() {
			if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		} else if step.StepName == types.StepPush {
			pushID = step.ID
			if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
				t.Fatal(err)
			}
		} else if step.StepName == types.StepPR {
			prID = step.ID
		} else if step.StepName == types.StepCI {
			ciID = step.ID
		}
	}
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: snapshot.CandidateSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, locations, nil)
	for _, isPushActive := range []bool{true, false} {
		if err := database.SetRunPushActive(run.ID, isPushActive); err != nil {
			t.Fatal(err)
		}

		plans, preserved, err := manager.recoverableParkedRuns(t.Context())

		if err != nil || len(plans) != 0 {
			t.Fatalf("receipt recovery plans = %v, %v", plans, err)
		}
		if _, ok := preserved[run.ID]; !ok {
			t.Fatalf("post-receipt Push with push_active=%v was not preserved", isPushActive)
		}
		if _, err := database.RecoverStaleRunsExcept("crashed", preserved); err != nil {
			t.Fatal(err)
		}
		stored, err := database.GetRun(run.ID)
		if err != nil || stored.Status != types.RunRunning || stored.HeadSHA != snapshot.CandidateSHA || *stored.PushGeneration != *snapshot.ExpectedRun.PushGeneration+1 {
			t.Fatalf("startup lost the existing receipt: %+v, %v", stored, err)
		}
		if err := manager.refuseCIRepairReplacement(t.Context(), repo.ID, run.Branch); err == nil {
			t.Fatal("replacement consumed an interrupted delivery")
		}
	}
	for _, completedID := range []string{pushID, prID} {
		if err := database.CompleteStep(completedID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		plans, preserved, err := manager.recoverableParkedRuns(t.Context())
		if err != nil || len(plans) != 0 {
			t.Fatalf("pending delivery recovery plans = %v, %v", plans, err)
		}
		if _, ok := preserved[run.ID]; !ok {
			t.Fatal("startup lost a recorded publication before delivery reached CI")
		}
		if _, err := database.RecoverStaleRunsExcept("crashed", preserved); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.StartStepWithAutoFixLimit(ciID, 1); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := manager.ciRepairHeld(t.Context(), stored); err != nil || held {
		t.Fatalf("completed delivery retained the temporary hold: %v, %v", held, err)
	}
}
