package steps

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type ordinaryCIHandbackFixture struct {
	context      *pipeline.StepContext
	request      ipc.CIHandbackParams
	caller       string
	remote       string
	gate         *db.StepResult
	databasePath string
}

func newOrdinaryCIHandbackFixture(t *testing.T) *ordinaryCIHandbackFixture {
	t.Helper()
	caller, base, published := setupGitRepo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	remote := filepath.Join(t.TempDir(), "remote.git")
	gate := filepath.Join(t.TempDir(), "gate.git")
	managed := filepath.Join(t.TempDir(), "managed")
	gitCmd(t, caller, "clone", "--bare", caller, remote)
	gitCmd(t, caller, "clone", "--bare", remote, gate)
	gitCmd(t, gate, "worktree", "add", "--detach", managed, published)
	const remoteURL = "https://github.com/test/ordinary-handback.git"
	gitCmd(t, caller, "config", "--global", "url."+filepath.ToSlash(remote)+".insteadOf", remoteURL)
	gitCmd(t, caller, "remote", "add", "origin", remoteURL)
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
	for _, err := range []error{
		database.UpdateRunStatus(run.ID, types.RunRunning),
		database.UpdateRunPRURL(run.ID, "https://github.com/test/ordinary-handback/pull/1"),
		database.UpdateRunPRState(run.ID, "open"),
		database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(remoteURL), Ref: "refs/heads/feature"}),
		database.UpdateRunReviewApprovedHeadSHA(run.ID, published),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	const findings = `{"findings":[{"id":"ci-1","category":"ci-check","severity":"error","action":"auto-fix","check":"tests","description":"tests failed"}]}`
	var ci *db.StepResult
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(step.ID); err != nil {
			t.Fatal(err)
		}
		output := `{"findings":[],"tested_head_sha":"` + published + `"}`
		if name == types.StepCI {
			output = findings
			ci = step
		}
		if _, err := database.InsertStepRound(step.ID, 1, "initial", &output, nil, 10); err != nil {
			t.Fatal(err)
		}
		if name == types.StepCI {
			err = database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 10, &output)
		} else {
			err = database.SetStepFindings(step.ID, output)
			if err == nil {
				err = database.CompleteStep(step.ID, 0, 10, "")
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	checks := `[{"name":"tests","state":"FAILURE","bucket":"fail","link":"https://github.com/test/ordinary-handback/actions/runs/1/job/1"}]`
	environment := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE": "ci-handback", "FAKE_CLI_STATE": "OPEN", "FAKE_CLI_CHECKS": checks,
		"FAKE_CLI_PR_HEAD_SHA": published, "FAKE_CLI_MERGEABLE": "MERGEABLE",
		"FAKE_CLI_PR_LIST_JSON": `[{"number":1,"url":"https://github.com/test/ordinary-handback/pull/1","baseRefName":"main"}]`,
	})
	rounds, err := database.GetRoundsByStep(ci.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &ordinaryCIHandbackFixture{
		context: &pipeline.StepContext{Ctx: t.Context(), DB: database, Run: run, Repo: repo, WorkDir: managed, GateDir: gate, StepResultID: ci.ID, Config: &config.Config{}, Env: environment},
		request: ipc.CIHandbackParams{RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, CallerDir: caller, HeadSHA: published, PublishedHead: published, PushGeneration: *run.PushGeneration, RoundID: rounds[len(rounds)-1].ID},
		caller:  caller, remote: remote, gate: ci, databasePath: databasePath,
	}
}

func TestCIHandbackOrdinaryFailureReturnsSourceWithoutDecidingTheGate(t *testing.T) {
	fixture := newOrdinaryCIHandbackFixture(t)
	before, err := fixture.context.DB.GetRun(fixture.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := fixture.context.DB.GetRoundsByStep(fixture.gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request)
	if err != nil {
		t.Fatalf("ordinary failed CI cannot return to its author: %v", err)
	}
	if result.ReleasedHead != before.HeadSHA || result.AcceptedHead != "" {
		t.Fatalf("release = %#v", result)
	}
	for index, environment := range fixture.context.Env {
		if strings.HasPrefix(environment, "FAKE_CLI_CHECKS=") {
			fixture.context.Env[index] = "FAKE_CLI_CHECKS=[]"
		}
	}
	if _, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request); err != nil {
		t.Fatalf("exact repeated release must be idempotent: %v", err)
	}
	service := &branchsync.Service{DB: fixture.context.DB, Repo: fixture.context.Repo, WorkDir: fixture.caller, GateDir: fixture.context.GateDir}
	if state := service.InspectCached(t.Context()); state.NextAction == nil || state.NextAction.Code != "resume_ci_handback" {
		t.Fatalf("released ordinary source has no supported next action: %#v", state)
	}
	after, err := fixture.context.DB.GetRun(before.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("release changed unresolved run: %#v, %v", after, err)
	}
	afterRounds, err := fixture.context.DB.GetRoundsByStep(fixture.gate.ID)
	if err != nil || !reflect.DeepEqual(rounds, afterRounds) {
		t.Fatalf("release decided or replaced failure history: %#v, %v", afterRounds, err)
	}
	for _, directory := range []string{fixture.caller, fixture.context.WorkDir, fixture.remote, fixture.context.GateDir} {
		if head := gitCmd(t, directory, "rev-parse", "feature"); head != before.HeadSHA {
			t.Fatalf("release moved %s to %s", directory, head)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.caller, "correction.txt"), []byte("authored correction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.caller, "add", "correction.txt")
	gitCmd(t, fixture.caller, "commit", "-m", "correct failed test")
	fixture.request.Resume = true
	fixture.request.ReceiptID = result.ReceiptID
	fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	result, err = (&CIStep{}).HandbackCI(fixture.context, fixture.request)
	if err != nil || result.AcceptedHead != fixture.request.HeadSHA {
		t.Fatalf("authored correction requires fresh validation: %#v, %v", result, err)
	}
}

func TestCIHandbackOrdinaryRefusalsPreserveSourceAndFailure(t *testing.T) {
	for _, name := range []string{"stale generation", "stale round", "wrong run", "unreviewed head", "retained repair", "incomplete validation", "mismatched Test evidence", "cancelled run", "active writer", "dirty caller", "dirty managed", "protected refusal", "no failed check", "review bot", "cancelled request", "enabled CI repair", "expect repo", "expect run", "expect branch", "expect caller", "expect caller head", "expect current head", "expect published head", "expect generation"} {
		t.Run(name, func(t *testing.T) {
			fixture := newOrdinaryCIHandbackFixture(t)
			sctx := fixture.context
			switch name {
			case "stale generation":
				fixture.request.PushGeneration++
			case "stale round":
				fixture.request.RoundID = "stale"
			case "wrong run":
				fixture.request.RunID = "another-run"
			case "unreviewed head":
				if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, sctx.Run.BaseSHA); err != nil {
					t.Fatal(err)
				}
			case "retained repair":
				if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, sctx.Run.BaseSHA); err != nil {
					t.Fatal(err)
				}
				sctx.Run.HeadSHA = sctx.Run.BaseSHA
			case "incomplete validation", "mismatched Test evidence":
				results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, result := range results {
					if result.StepName != types.StepTest {
						continue
					}
					if name == "incomplete validation" {
						err = sctx.DB.UpdateStepStatus(result.ID, types.StepStatusSkipped)
					} else {
						err = sctx.DB.SetStepFindings(result.ID, `{"findings":[],"tested_head_sha":"`+sctx.Run.BaseSHA+`"}`)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			case "cancelled run":
				if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunCancelled); err != nil {
					t.Fatal(err)
				}
			case "active writer":
				pid := os.Getpid()
				if err := sctx.DB.SetStepAgentActivity(fixture.gate.ID, "active writer", &pid); err != nil {
					t.Fatal(err)
				}
			case "dirty caller", "dirty managed":
				directory := fixture.caller
				if name == "dirty managed" {
					directory = sctx.WorkDir
				}
				if err := os.WriteFile(filepath.Join(directory, "dirty.txt"), []byte("keep dirty work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "protected refusal":
				if err := sctx.DB.SetStepFindings(fixture.gate.ID, `{"findings":[{"id":"protected-path-refusal","severity":"error","action":"ask-user","description":"protected source"}]}`); err != nil {
					t.Fatal(err)
				}
			case "no failed check", "review bot":
				value := "[]"
				if name == "review bot" {
					value = `[{"name":"tests","state":"FAILURE","bucket":"fail","app":"greptile-apps"}]`
				}
				for index, environment := range sctx.Env {
					if strings.HasPrefix(environment, "FAKE_CLI_CHECKS=") {
						sctx.Env[index] = "FAKE_CLI_CHECKS=" + value
					}
				}
			case "cancelled request":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				sctx.Ctx = ctx
			case "enabled CI repair":
				sctx.Config.AutoFix.CI = 1
			case "expect repo", "expect run", "expect branch", "expect caller", "expect caller head", "expect current head", "expect published head", "expect generation":
				expected := &ipc.CIHandbackBinding{RepoID: sctx.Repo.ID, RunID: sctx.Run.ID, Branch: sctx.Run.Branch, CallerDir: fixture.caller, CallerHead: fixture.request.HeadSHA, CurrentHead: sctx.Run.HeadSHA, PublishedHead: fixture.request.PublishedHead, PushGeneration: fixture.request.PushGeneration}
				fixture.request.Expect = expected
				switch name {
				case "expect repo":
					expected.RepoID = "different-repository"
				case "expect run":
					expected.RunID = "different-run"
				case "expect branch":
					expected.Branch = "different-branch"
				case "expect caller":
					expected.CallerDir = t.TempDir()
				case "expect caller head":
					expected.CallerHead = sctx.Run.BaseSHA
				case "expect current head":
					expected.CurrentHead = sctx.Run.BaseSHA
				case "expect published head":
					expected.PublishedHead = sctx.Run.BaseSHA
				case "expect generation":
					expected.PushGeneration++
				}
			}
			before, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := sctx.DB.GetRoundsByStep(fixture.gate.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err == nil {
				t.Fatal("unsafe ordinary return was accepted")
			}
			after, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed run: %#v %v", after, err)
			}
			afterRounds, err := sctx.DB.GetRoundsByStep(fixture.gate.ID)
			if err != nil || !reflect.DeepEqual(rounds, afterRounds) {
				t.Fatalf("refusal changed failure history: %#v %v", afterRounds, err)
			}
			if receipt, err := sctx.DB.LatestCIHandback(sctx.Run.ID); err != nil || receipt != nil {
				t.Fatalf("refusal left a release: %#v %v", receipt, err)
			}
			for _, directory := range []string{fixture.caller, sctx.WorkDir, fixture.remote} {
				if head := gitCmd(t, directory, "rev-parse", "HEAD"); head != fixture.request.PublishedHead {
					t.Fatalf("refusal moved %s to %s", directory, head)
				}
			}
		})
	}
}

func TestCIHandbackOrdinaryDivergencePreservesBothHistories(t *testing.T) {
	fixture := newOrdinaryCIHandbackFixture(t)
	gitCmd(t, fixture.caller, "checkout", "-b", "author-output", fixture.context.Run.BaseSHA)
	if err := os.WriteFile(filepath.Join(fixture.caller, "author.txt"), []byte("retained author work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.caller, "add", "author.txt")
	gitCmd(t, fixture.caller, "commit", "-m", "retain author history")
	author := gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	gitCmd(t, fixture.caller, "branch", "-m", "feature", "published-output")
	gitCmd(t, fixture.caller, "branch", "-m", "feature")
	fixture.request.HeadSHA = author
	released, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request)
	if err != nil {
		t.Fatalf("preserved diverged source cannot return to author: %v", err)
	}
	if head := gitCmd(t, fixture.caller, "rev-parse", "HEAD"); head != author {
		t.Fatal("return overwrote divergent author history")
	}
	service := &branchsync.Service{DB: fixture.context.DB, Repo: fixture.context.Repo, WorkDir: fixture.caller, GateDir: fixture.context.GateDir}
	if state := service.Apply(t.Context()); state.Changed || state.NextAction == nil || state.NextAction.Code != "integrate_ci_handback" {
		t.Fatalf("released divergence must offer additive integration without synchronization: %#v", state)
	}
	anchor := fmt.Sprintf("refs/no-mistakes/ci-handback/%s/%d", fixture.context.Run.ID, fixture.request.PushGeneration)
	for _, source := range []struct{ ref, head string }{{anchor + "/author", author}, {anchor + "/published", fixture.context.Run.HeadSHA}} {
		if head := gitCmd(t, fixture.caller, "rev-parse", source.ref); head != source.head {
			t.Fatalf("source anchor %s = %s", source.ref, head)
		}
	}
	gitCmd(t, fixture.caller, "merge", "--no-ff", "--no-edit", anchor+"/published")
	fixture.request.Resume = true
	fixture.request.ReceiptID = released.ReceiptID
	fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	if _, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request); err != nil {
		t.Fatalf("additive authored follow-up cannot resume: %v", err)
	}
	for _, source := range []string{author, fixture.request.PublishedHead} {
		gitCmd(t, fixture.context.WorkDir, "merge-base", "--is-ancestor", source, fixture.request.HeadSHA)
	}
}

func TestCIHandbackOrdinaryStorageFailurePreservesReleaseAndCorrection(t *testing.T) {
	fixture := newOrdinaryCIHandbackFixture(t)
	connection, err := sql.Open("sqlite", fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Exec(`CREATE TRIGGER reject_release BEFORE INSERT ON ci_handbacks BEGIN SELECT RAISE(ABORT, 'fixture storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.context.DB.GetRun(fixture.context.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request); err == nil || !strings.Contains(err.Error(), "fixture storage failure") {
		t.Fatalf("release did not refuse the failed durable write: %v", err)
	}
	if receipt, err := fixture.context.DB.LatestCIHandback(before.ID); err != nil || receipt != nil {
		t.Fatalf("failed storage reported a release: %#v %v", receipt, err)
	}
	if _, err := connection.Exec("DROP TRIGGER reject_release"); err != nil {
		t.Fatal(err)
	}
	released, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.context.DB.LatestCIHandback(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.caller, "correction.txt"), []byte("keep this authored correction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.caller, "add", "correction.txt")
	gitCmd(t, fixture.caller, "commit", "-m", "correct fixture")
	fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	fixture.request.Resume, fixture.request.ReceiptID = true, released.ReceiptID
	if _, err := connection.Exec(`CREATE TRIGGER reject_acceptance BEFORE UPDATE OF head_sha ON runs BEGIN SELECT RAISE(ABORT, 'fixture acceptance storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request); err == nil || !strings.Contains(err.Error(), "fixture acceptance storage failure") {
		t.Fatalf("acceptance did not reach its failed durable write: %v", err)
	}
	if after, err := fixture.context.DB.GetRun(before.ID); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed acceptance changed source or publication identity: %#v %v", after, err)
	}
	if after, err := fixture.context.DB.LatestCIHandback(before.ID); err != nil || !reflect.DeepEqual(receipt, after) {
		t.Fatalf("failed acceptance changed the durable release: %#v %v", after, err)
	}
	if head := gitCmd(t, fixture.context.WorkDir, "rev-parse", "HEAD"); head != before.HeadSHA {
		t.Fatalf("failed acceptance left unbound managed movement at %s", head)
	}
	if head := gitCmd(t, fixture.caller, "rev-parse", "HEAD"); head != fixture.request.HeadSHA {
		t.Fatal("failed acceptance lost the authored correction")
	}
	if _, err := connection.Exec("DROP TRIGGER reject_acceptance"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.context.DB.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.context.DB, err = db.Open(fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fixture.context.DB.Close() })
	if result, err := (&CIStep{}).HandbackCI(fixture.context, fixture.request); err != nil || result.ReceiptID != receipt.ID || result.AcceptedHead != fixture.request.HeadSHA {
		t.Fatalf("same durable release cannot accept its preserved correction after reopening: %#v %v", result, err)
	}
}
