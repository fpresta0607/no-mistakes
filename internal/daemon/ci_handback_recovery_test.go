package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
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

func newCIHandbackRecoveryFixture(t *testing.T) *ciHandbackRecoveryFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
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
	run, err := database.InsertRun(repo.ID, "feature", published, baseHead)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
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
	const findings = `{"findings":[{"id":"ci-1","category":"ci-merge-conflict","severity":"error","action":"auto-fix","description":"conflict"}]}`
	for _, step := range sequence {
		result, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(result.ID); err != nil {
			t.Fatal(err)
		}
		clean := `{"findings":[],"summary":"previous validation"}`
		if step.Name() == types.StepCI {
			clean = findings
		}
		round, err := database.InsertStepRound(result.ID, 1, "initial", &clean, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		switch step.Name() {
		case types.StepReview:
			err = database.CompleteReviewStep(result.ID, run.ID, published, 0, 10, "")
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
	for _, checkpoint := range []string{"before movement", "after movement", "accepted before reset", "accepted after reset", "dirty managed worktree", "unknown managed head", "unbound movement", "changed publication", "ambiguous ownership", "dirty caller", "changed caller", "changed target", "changed PR head", "changed gate lane", "active writer", "changed generation", "accepted with duplicate caller"} {
		t.Run(checkpoint, func(t *testing.T) {
			fixture := newCIHandbackRecoveryFixture(t)
			database, manager := fixture.manager.db, fixture.manager
			if checkpoint != "before movement" {
				gitCmd(t, fixture.managed, "merge", "--ff-only", "--no-edit", fixture.followup)
			}
			isAccepted := checkpoint == "accepted before reset" || checkpoint == "accepted after reset" || checkpoint == "accepted with duplicate caller"
			if isAccepted {
				if err := database.AcceptCIHandback(fixture.receipt, fixture.followup, fixture.repo); err != nil {
					t.Fatal(err)
				}
			}
			if checkpoint == "accepted after reset" {
				if err := database.ResetStepsFrom(fixture.run.ID, types.StepReview.Order()); err != nil {
					t.Fatal(err)
				}
				if err := database.CompleteRunAwaitingAgent(fixture.run.ID, 10); err != nil {
					t.Fatal(err)
				}
			}
			shouldRefuse := false
			switch checkpoint {
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
