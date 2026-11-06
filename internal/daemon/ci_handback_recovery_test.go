package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type ciHandbackRecoveryFixture struct {
	manager   *RunManager
	run       *db.Run
	repo      *db.Repo
	receipt   *db.CIHandback
	managed   string
	remote    string
	published string
	followup  string
	rounds    map[string][]*db.StepRound
	push      *mockPassStep
}

func newCIHandbackRecoveryFixture(t *testing.T, isOrdinary, hasDocumentCommit bool) *ciHandbackRecoveryFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	claude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+claude+"\nauto_fix:\n  ci: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := setupTestGitRepo(t, p, database, "handback-recovery")
	caller := repo.WorkingPath
	gitCmd(t, caller, "branch", "-M", "main")
	gitCmd(t, caller, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(caller, "feature.txt"), []byte("published work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, caller, "add", "feature.txt")
	gitCmd(t, caller, "commit", "-m", "published feature")
	reviewed := gitOutput(t, caller, "rev-parse", "HEAD")
	if hasDocumentCommit {
		if err := os.WriteFile(filepath.Join(caller, "README.md"), []byte("document the reviewed feature\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, caller, "add", "README.md")
		gitCmd(t, caller, "commit", "-m", "document reviewed feature")
	}
	published := gitOutput(t, caller, "rev-parse", "HEAD")
	gitCmd(t, caller, "push", "gate", "feature")
	remote := filepath.Join(t.TempDir(), "published.git")
	gitCmd(t, caller, "clone", "--bare", caller, remote)
	gitCmd(t, caller, "config", "--global", "url."+filepath.ToSlash(remote)+".insteadOf", repo.UpstreamURL)
	gitCmd(t, caller, "remote", "add", "origin", repo.UpstreamURL)
	gitCmd(t, p.RepoDir(repo.ID), "remote", "set-url", "origin", repo.UpstreamURL)
	gitCmd(t, caller, "checkout", "main")
	if err := os.WriteFile(filepath.Join(caller, "base.txt"), []byte("base advanced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, caller, "add", "base.txt")
	gitCmd(t, caller, "commit", "-m", "advance base")
	baseHead := gitOutput(t, caller, "rev-parse", "HEAD")
	gitCmd(t, caller, "push", "origin", "main")
	gitCmd(t, caller, "push", "gate", "main")
	gitCmd(t, caller, "checkout", "feature")
	run, err := database.InsertRun(repo.ID, "feature", reviewed, baseHead)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		database.UpdateRunHeadSHA(run.ID, published),
		database.UpdateRunStatus(run.ID, types.RunRunning),
		database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"),
		database.UpdateRunPRState(run.ID, "open"),
		database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.PushURL()), Ref: "refs/heads/feature"}),
		database.UpdateRunIntent(run.ID, db.RunIntent{Summary: "preserve the same published run", Source: db.RunIntentSourceAgent}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	managed := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", managed, published)
	push := &mockPassStep{name: types.StepPush}
	sequence := []pipeline.Step{&mockPassStep{name: types.StepRebase}, &mockApprovalStep{name: types.StepReview}, &mockPassStep{name: types.StepTest}, &mockPassStep{name: types.StepDocument}, &mockPassStep{name: types.StepLint}, push, &mockPassStep{name: types.StepPR}, &mockPassStep{name: types.StepCI}}
	fixture := &ciHandbackRecoveryFixture{run: run, repo: repo, managed: managed, remote: remote, published: published, rounds: make(map[string][]*db.StepRound), push: push}
	fixture.manager = NewRunManager(database, p, func() []pipeline.Step { return sequence })
	t.Cleanup(func() { fixture.manager.db.Close() })
	findings := `{"findings":[{"id":"ci-1","category":"ci-merge-conflict","severity":"error","action":"auto-fix","description":"conflict"}]}`
	if isOrdinary {
		findings = `{"findings":[{"id":"ci-1","category":"ci-check","severity":"error","check":"tests","action":"auto-fix","description":"failed tests"}]}`
	}
	for _, step := range sequence {
		result, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(result.ID); err != nil {
			t.Fatal(err)
		}
		clean := `{"findings":[],"summary":"previous validation"}`
		if step.Name() == types.StepTest {
			clean = `{"findings":[],"tested_head_sha":"` + reviewed + `"}`
		}
		if step.Name() == types.StepCI {
			clean = findings
		}
		round, err := database.InsertReviewStepRound(result.ID, 1, "initial", &clean, nil, reviewed, 10)
		if err != nil {
			t.Fatal(err)
		}
		switch step.Name() {
		case types.StepReview:
			err = database.CompleteReviewStep(result.ID, run.ID, reviewed, 0, 10, "")
		case types.StepCI:
			selection := `["ci-1"]`
			if err := database.SetStepRoundSelection(round.ID, &selection, db.RoundSelectionSourceAutoFix); err != nil {
				t.Fatal(err)
			}
			round, err = database.InsertStepRound(result.ID, 2, "auto_fix", &clean, nil, 20)
			if err != nil {
				t.Fatal(err)
			}
			fixture.receipt = &db.CIHandback{RunID: run.ID, RoundID: round.ID, CallerDir: caller, ReleasedHead: published, PRURL: "https://github.com/test/repo/pull/1", BaseBranch: "main", BaseHead: baseHead, PushGeneration: 1, TargetFingerprint: branchsync.TargetFingerprint(repo.PushURL()), PushRef: "refs/heads/feature"}
			err = database.ParkStepForApproval(run.ID, result.ID, types.StepStatusAwaitingApproval, 0, 30, &clean)
		default:
			if err := database.SetStepFindings(result.ID, clean); err != nil {
				t.Fatal(err)
			}
			err = database.CompleteStepWithStatus(result.ID, types.StepStatusCompleted, 0, 10, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		fixture.rounds[result.ID], err = database.GetRoundsByStep(result.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.run = run
	if isOrdinary {
		limits := make(map[types.StepName]int)
		for _, name := range types.AllSteps() {
			limits[name] = 0
		}
		fixture.receipt.Source = &db.CIHandbackSource{CallerHead: published, SubmittedHead: *run.SubmittedHeadSHA, ReviewHead: reviewed, PushKind: *run.PushTargetKind, PushedAt: *run.LastPushedAt, Findings: findings, AutoFixLimits: limits}
		if hasDocumentCommit {
			fixture.receipt.Source.Validation, err = database.ReadCIHandbackValidation(run.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		anchor := "refs/no-mistakes/ci-handback/" + run.ID + "/1"
		for _, directory := range []string{caller, p.RepoDir(repo.ID)} {
			gitCmd(t, directory, "update-ref", anchor+"/published", published)
			gitCmd(t, directory, "update-ref", anchor+"/author", published)
		}
	}
	if err := database.ReleaseCIHandback(fixture.receipt, run, repo, findings); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, caller, "merge", "--no-ff", "--no-edit", "main")
	fixture.followup = gitOutput(t, caller, "rev-parse", "HEAD")
	gitCmd(t, managed, "fetch", "--no-tags", "--no-write-fetch-head", caller, fixture.followup)
	if err := database.PrepareCIHandback(fixture.receipt, fixture.followup, baseHead, "HEAD", repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLI_MODE", "ci-gh")
	t.Setenv("FAKE_CLI_STATE", "OPEN")
	t.Setenv("FAKE_CLI_CHECKS", "[]")
	t.Setenv("FAKE_CLI_PR_HEAD_SHA", published)
	t.Setenv("FAKE_CLI_PR_LIST_JSON", `[{"number":1,"url":"https://github.com/test/repo/pull/1","baseRefName":"main"}]`)
	return fixture
}

func TestManagerCancelReleasedCIHandbackAfterRecoveryFailure(t *testing.T) {
	for _, release := range []struct {
		name       string
		isOrdinary bool
	}{
		{"ordinary", true},
		{"conflict", false},
	} {
		t.Run(release.name, func(t *testing.T) {
			fixture := newCIHandbackRecoveryFixture(t, release.isOrdinary, false)
			manager := fixture.manager
			if err := manager.db.ClearCIHandbackCandidate(fixture.receipt); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Close(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(manager.paths.DB())
			if err != nil {
				t.Fatal(err)
			}
			manager = NewRunManager(database, manager.paths, manager.steps)
			fixture.manager = manager
			if err := os.Rename(fixture.remote, fixture.remote+".offline"); err != nil {
				t.Fatal(err)
			}
			if plan, err := manager.prepareRecoveredRun(t.Context(), fixture.run); err == nil || !strings.Contains(err.Error(), "failed to fetch or resolve trusted default branch") {
				if plan != nil {
					plan.agent.Close()
				}
				t.Fatalf("recovery did not fail at the trusted-config boundary: %v", err)
			}
			plans, preserved := manager.recoverableParkedRuns(t.Context())
			if len(plans) != 0 {
				for _, plan := range plans {
					plan.agent.Close()
				}
				t.Fatal("startup installed a plan without trusted config")
			}
			if _, ok := preserved[fixture.run.ID]; !ok {
				t.Fatal("startup lost the released reservation")
			}
			if _, err := database.RecoverStaleRunsExcept("daemon restarted", preserved); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(fixture.remote+".offline", fixture.remote); err != nil {
				t.Fatal(err)
			}
			before, err := database.GetRun(fixture.run.ID)
			if err != nil || !reflect.DeepEqual(before, fixture.run) {
				t.Fatalf("failed recovery changed the reservation: %#v %v", before, err)
			}
			beforeSteps, err := database.GetStepsByRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			refs := make(map[string]string)
			for _, directory := range []string{fixture.repo.WorkingPath, fixture.managed, fixture.remote, manager.paths.RepoDir(fixture.repo.ID)} {
				refs[directory] = gitOutput(t, directory, "for-each-ref", "--format=%(refname) %(objectname)")
			}
			callerHead := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD")
			managedHead := gitOutput(t, fixture.managed, "rev-parse", "HEAD")
			subscription, err := manager.Subscribe(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer subscription.Close()
			eventContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			subscription.Next(eventContext)

			if err := manager.HandleCancel(fixture.run.ID); err != nil {
				t.Fatalf("explicit abort cannot end the released reservation: %v", err)
			}

			after, err := database.GetRun(fixture.run.ID)
			if err != nil || after.Status != types.RunCancelled || after.Error == nil || *after.Error != types.RunCancelReasonAbortedByUser || after.AwaitingAgentSince != nil || after.ParkedMS < before.ParkedMS {
				t.Fatalf("aborted reservation = %#v %v", after, err)
			}
			expected := *before
			expected.Status, expected.Error, expected.AwaitingAgentSince = types.RunCancelled, after.Error, nil
			expected.ParkedMS, expected.UpdatedAt = after.ParkedMS, after.UpdatedAt
			if !reflect.DeepEqual(&expected, after) {
				t.Fatalf("abort changed source or validation identity: %#v", after)
			}
			stored, err := database.LatestCIHandback(fixture.run.ID)
			if err != nil || !reflect.DeepEqual(stored, fixture.receipt) {
				t.Fatalf("abort changed the receipt or its repair policy: %#v %v", stored, err)
			}
			afterSteps, err := database.GetStepsByRun(fixture.run.ID)
			if err != nil || len(afterSteps) != len(beforeSteps) {
				t.Fatalf("abort changed the step sequence: %#v %v", afterSteps, err)
			}
			for index, step := range afterSteps {
				previous := *beforeSteps[index]
				if step.StepName == types.StepCI {
					if step.Status != types.StepStatusFailed || step.Error == nil || *step.Error != types.RunCancelReasonAbortedByUser || step.CompletedAt == nil {
						t.Fatalf("aborted CI gate = %#v", step)
					}
					previous.Status, previous.Error, previous.CompletedAt = step.Status, step.Error, step.CompletedAt
					previous.LastActivityAt, previous.LastActivity = step.LastActivityAt, step.LastActivity
				}
				if !reflect.DeepEqual(&previous, step) {
					t.Fatalf("abort changed findings, actual execution spend or policy: %#v", step)
				}
				rounds, err := database.GetRoundsByStep(step.ID)
				if err != nil || !reflect.DeepEqual(rounds, fixture.rounds[step.ID]) {
					t.Fatalf("abort changed validation history: %#v %v", rounds, err)
				}
			}
			event, ok := subscription.Next(eventContext)
			if !ok || event.Type != ipc.EventRunCompleted || event.Status == nil || *event.Status != string(types.RunCancelled) || event.Error == nil || *event.Error != types.RunCancelReasonAbortedByUser {
				t.Fatalf("abort event = %#v, open = %v", event, ok)
			}
			if _, ok := subscription.Next(eventContext); ok {
				t.Fatal("aborted reservation left its subscription open")
			}
			if err := manager.HandleCancel(fixture.run.ID); err == nil {
				t.Fatal("abort accepted an already terminal reservation")
			}
			if _, err := manager.prepareRecoveredRun(t.Context(), after); err == nil {
				t.Fatal("recovery resurrected an explicitly aborted reservation")
			}
			plans, preserved = manager.recoverableParkedRuns(t.Context())
			if len(plans) != 0 || len(preserved) != 0 {
				t.Fatal("startup resurrected an explicitly aborted reservation")
			}
			if err := database.PrepareCIHandback(stored, fixture.followup, stored.BaseHead, "HEAD", fixture.repo); err == nil {
				t.Fatal("follow-up resumed after explicit abort")
			}
			for directory, original := range refs {
				if got := gitOutput(t, directory, "for-each-ref", "--format=%(refname) %(objectname)"); got != original {
					t.Fatalf("abort changed preserved refs in %s: %s", directory, got)
				}
			}
			if got := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD"); got != callerHead {
				t.Fatalf("abort changed author source: %s", got)
			}
			if got := gitOutput(t, fixture.managed, "rev-parse", "HEAD"); got != managedHead {
				t.Fatalf("abort changed managed source: %s", got)
			}
		})
	}
}

func TestManagerCancelReleasedCIHandbackRefusesWritersAndTransitions(t *testing.T) {
	for _, state := range []string{"push writer", "agent writer", "active validation", "candidate", "accepted", "tracked executor"} {
		t.Run(state, func(t *testing.T) {
			fixture := newCIHandbackRecoveryFixture(t, true, false)
			manager, database := fixture.manager, fixture.manager.db
			if state != "candidate" && state != "accepted" {
				if err := database.ClearCIHandbackCandidate(fixture.receipt); err != nil {
					t.Fatal(err)
				}
			}
			results, err := database.GetStepsByRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "push writer":
				err = database.SetRunPushActive(fixture.run.ID, true)
			case "agent writer":
				processID := os.Getpid()
				err = database.SetStepAgentActivity(results[len(results)-1].ID, "active agent", &processID)
			case "active validation":
				err = database.UpdateStepStatus(results[1].ID, types.StepStatusRunning)
			case "accepted":
				err = database.AcceptCIHandback(fixture.receipt, fixture.followup, fixture.repo)
			case "tracked executor":
				manager.executors[fixture.run.ID] = &pipeline.Executor{}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := database.GetRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeSteps, err := database.GetStepsByRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}

			if err := manager.HandleCancel(fixture.run.ID); err == nil {
				t.Fatal("cancel treated an unverified reservation as writer-free")
			}

			after, err := database.GetRun(fixture.run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refused abort changed the run: %#v %v", after, err)
			}
			afterSteps, err := database.GetStepsByRun(fixture.run.ID)
			if err != nil || !reflect.DeepEqual(beforeSteps, afterSteps) {
				t.Fatalf("refused abort changed the steps: %#v %v", afterSteps, err)
			}
		})
	}
}

func TestManagerCancelUsesLiveCancellation(t *testing.T) {
	manager := NewRunManager(nil, nil, nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	manager.cancels["live"] = cancel

	if err := manager.HandleCancel("live"); err != nil {
		t.Fatal(err)
	}

	if cause := context.Cause(ctx); cause == nil || cause.Error() != types.RunCancelReasonAbortedByUser {
		t.Fatalf("live cancellation cause = %v", cause)
	}
}

func TestManagerCIHandbackCrashRecovery(t *testing.T) {
	binDir := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	command := exec.Command("go", "build", "-o", filepath.Join(binDir, name), "../pipeline/fakecli")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake forge: %v\n%s", err, output)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, checkpoint := range []string{"before movement", "after movement", "accepted before reset", "accepted after reset", "dirty managed worktree", "unknown managed head", "unbound movement", "changed publication", "ambiguous ownership", "dirty caller", "changed caller", "changed target", "changed PR head", "changed gate lane", "active writer", "changed generation", "accepted with duplicate caller", "ordinary after movement", "ordinary accepted before reset", "ordinary source anchor changed", "ordinary validation identity changed", "ordinary stale managed origin", "ordinary changed clone source", "ordinary document after movement", "ordinary document accepted before reset", "ordinary document accepted after reset", "ordinary document modified evidence"} {
		t.Run(checkpoint, func(t *testing.T) {
			fixture := newCIHandbackRecoveryFixture(t, strings.HasPrefix(checkpoint, "ordinary "), strings.Contains(checkpoint, "document"))
			database, manager := fixture.manager.db, fixture.manager
			if checkpoint != "before movement" {
				gitCmd(t, fixture.managed, "merge", "--ff-only", "--no-edit", fixture.followup)
			}
			isAccepted := checkpoint == "accepted before reset" || checkpoint == "accepted after reset" || checkpoint == "accepted with duplicate caller" || checkpoint == "ordinary accepted before reset" || strings.Contains(checkpoint, "document accepted")
			if isAccepted {
				if err := database.AcceptCIHandback(fixture.receipt, fixture.followup, fixture.repo); err != nil {
					t.Fatal(err)
				}
			}
			if checkpoint == "accepted after reset" || checkpoint == "ordinary document accepted after reset" {
				if err := database.ResetStepsFrom(fixture.run.ID, types.StepReview.Order()); err != nil {
					t.Fatal(err)
				}
				if err := database.CompleteRunAwaitingAgent(fixture.run.ID, 10); err != nil {
					t.Fatal(err)
				}
			}
			shouldRefuse := false
			switch checkpoint {
			case "ordinary document modified evidence":
				shouldRefuse = true
				results, err := database.GetStepsByRun(fixture.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, result := range results {
					if result.StepName == types.StepTest {
						if err := database.SetStepFindings(result.ID, `{"findings":[],"tested_head_sha":"`+fixture.published+`"}`); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "ordinary stale managed origin":
				staleSource := filepath.Join(t.TempDir(), "stale.git")
				gitCmd(t, fixture.repo.WorkingPath, "init", "--bare", staleSource)
				gitCmd(t, fixture.managed, "remote", "set-url", "origin", staleSource)
			case "ordinary changed clone source":
				shouldRefuse = true
				gitCmd(t, fixture.repo.WorkingPath, "remote", "set-url", "origin", "https://github.com/changed/source.git")
			case "ordinary validation identity changed":
				shouldRefuse = true
				if err := database.UpdateRunReviewApprovedHeadSHA(fixture.run.ID, fixture.run.BaseSHA); err != nil {
					t.Fatal(err)
				}
			case "ordinary source anchor changed":
				shouldRefuse = true
				gitCmd(t, fixture.repo.WorkingPath, "update-ref", "-d", "refs/no-mistakes/ci-handback/"+fixture.run.ID+"/1/author")
			case "dirty managed worktree":
				shouldRefuse = true
				if err := os.WriteFile(filepath.Join(fixture.managed, "dirty.txt"), []byte("preserve this"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unknown managed head":
				shouldRefuse = true
				gitCmd(t, fixture.managed, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "unknown writer")
			case "unbound movement":
				shouldRefuse = true
				if err := database.ClearCIHandbackCandidate(fixture.receipt); err != nil {
					t.Fatal(err)
				}
			case "changed publication":
				shouldRefuse = true
				gitCmd(t, fixture.remote, "fetch", "--no-tags", "--no-write-fetch-head", fixture.repo.WorkingPath, fixture.followup)
				gitCmd(t, fixture.remote, "update-ref", "refs/heads/feature", fixture.followup, fixture.published)
			case "ambiguous ownership":
				shouldRefuse = true
				if _, err := database.InsertRun(fixture.repo.ID, fixture.run.Branch, fixture.published, fixture.run.BaseSHA); err != nil {
					t.Fatal(err)
				}
			case "dirty caller":
				shouldRefuse = true
				if err := os.WriteFile(filepath.Join(fixture.repo.WorkingPath, "dirty.txt"), []byte("preserve caller edits"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed caller":
				shouldRefuse = true
				gitCmd(t, fixture.repo.WorkingPath, "commit", "--allow-empty", "-m", "later caller commit")
			case "changed target":
				shouldRefuse = true
				if _, err := database.ReplaceRepoURLs(fixture.repo.ID, "https://github.com/changed/repo", ""); err != nil {
					t.Fatal(err)
				}
			case "changed PR head":
				shouldRefuse = true
				t.Setenv("FAKE_CLI_PR_HEAD_SHA", fixture.followup)
			case "changed gate lane":
				shouldRefuse = true
				gitCmd(t, manager.paths.RepoDir(fixture.repo.ID), "update-ref", "refs/heads/feature", fixture.run.BaseSHA, fixture.published)
			case "active writer":
				shouldRefuse = true
				if err := database.SetRunPushActive(fixture.run.ID, true); err != nil {
					t.Fatal(err)
				}
			case "changed generation":
				shouldRefuse = true
				if err := database.UpdateRunPublication(fixture.run.ID, db.PushBinding{HeadSHA: fixture.published, TargetKind: "upstream", TargetFingerprint: fixture.receipt.TargetFingerprint, Ref: fixture.receipt.PushRef}); err != nil {
					t.Fatal(err)
				}
			case "accepted with duplicate caller":
				shouldRefuse = true
				gitCmd(t, fixture.repo.WorkingPath, "worktree", "add", "--force", filepath.Join(t.TempDir(), "duplicate"), "feature")
			}
			before, err := database.GetRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			managedHead := gitOutput(t, fixture.managed, "rev-parse", "HEAD")
			remoteHead := gitOutput(t, fixture.remote, "rev-parse", "feature")
			gateHead := gitOutput(t, manager.paths.RepoDir(fixture.repo.ID), "rev-parse", "feature")
			callerHead := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD")
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			manager.db, err = db.Open(manager.paths.DB())
			if err != nil {
				t.Fatal(err)
			}
			database = manager.db
			plans, preserved := manager.recoverableParkedRuns(t.Context())
			if _, ok := preserved[fixture.run.ID]; !ok {
				t.Fatal("interrupted handback fell through to stale-run cleanup")
			}
			if _, err := database.RecoverStaleRunsExcept("fixture process exited", preserved); err != nil {
				t.Fatal(err)
			}
			after, err := database.GetRun(fixture.run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("startup changed the bound run or its budgets: %#v %v", after, err)
			}
			if shouldRefuse {
				if len(plans) != 0 {
					for _, plan := range plans {
						plan.agent.Close()
					}
					t.Fatal("startup guessed recovery for changed or ambiguous evidence")
				}
				if got := gitOutput(t, fixture.managed, "rev-parse", "HEAD"); got != managedHead {
					t.Fatalf("refused recovery moved managed head to %s", got)
				}
			} else {
				if len(plans) != 1 || plans[0].run.ID != fixture.run.ID {
					t.Fatalf("bound handback was not planned for the same run: %#v", plans)
				}
				plan := plans[0]
				defer plan.agent.Close()
				if fixture.receipt.Source != nil && !plan.repo.URLsVerified {
					t.Fatal("ordinary recovery lost its verified repository authority")
				}
				expectedHead := fixture.published
				if isAccepted {
					expectedHead = fixture.followup
				}
				if got := gitOutput(t, fixture.managed, "rev-parse", "HEAD"); got != expectedHead {
					t.Fatalf("recovered managed head = %s, want %s", got, expectedHead)
				}
				if isAccepted {
					executor := pipeline.NewExecutor(database, manager.paths, plan.cfg, nil, plan.steps, nil)
					ctx, cancel := context.WithCancel(t.Context())
					done := make(chan error, 1)
					go func() { done <- executor.Resume(ctx, plan.run, plan.repo, plan.workDir) }()
					defer func() {
						cancel()
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("fixture executor did not stop")
						}
					}()
					deadline := time.Now().Add(15 * time.Second)
					for {
						results, err := database.GetStepsByRun(fixture.run.ID)
						if err != nil {
							t.Fatal(err)
						}
						if results[1].Status == types.StepStatusAwaitingApproval {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("accepted candidate did not enter fresh Review: %#v", results[1])
						}
						time.Sleep(10 * time.Millisecond)
					}
					if fixture.push.execCnt.Load() != 0 {
						t.Fatal("accepted candidate published before fresh Review")
					}
					fresh, err := database.GetRun(fixture.run.ID)
					if err != nil || fresh.ReviewApprovedHeadSHA != nil || fresh.CIReadyAt != nil || fresh.CIReadyNoCI {
						t.Fatalf("accepted candidate reused released-head readiness: %#v %v", fresh, err)
					}
				}
			}
			for stepID, original := range fixture.rounds {
				rounds, err := database.GetRoundsByStep(stepID)
				if err != nil || len(rounds) < len(original) || !reflect.DeepEqual(rounds[:len(original)], original) {
					t.Fatalf("startup lost or decided prior validation rounds: %#v %v", rounds, err)
				}
			}
			if got := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD"); got != callerHead {
				t.Fatalf("startup changed caller commits: %s", got)
			}
			if got := gitOutput(t, manager.paths.RepoDir(fixture.repo.ID), "rev-parse", "feature"); got != gateHead {
				t.Fatalf("startup changed the published gate lane: %s", got)
			}
			if got := gitOutput(t, fixture.remote, "rev-parse", "feature"); got != remoteHead {
				t.Fatalf("startup changed published commits: %s", got)
			}
		})
	}
}
