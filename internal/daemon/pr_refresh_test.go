package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

func TestPRRefreshRunIdentity(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		change    func(*db.Run, *db.Repo, *ipc.RefreshPRParams)
		isRefused bool
	}{
		{"exact", func(*db.Run, *db.Repo, *ipc.RefreshPRParams) {}, false},
		{"wrong-run", func(_ *db.Run, _ *db.Repo, request *ipc.RefreshPRParams) { request.RunID = "another" }, true},
		{"wrong-repo", func(_ *db.Run, _ *db.Repo, request *ipc.RefreshPRParams) { request.RepoID = "another" }, true},
		{"wrong-branch", func(_ *db.Run, _ *db.Repo, request *ipc.RefreshPRParams) { request.Branch = "another" }, true},
		{"wrong-head", func(_ *db.Run, _ *db.Repo, request *ipc.RefreshPRParams) { request.HeadSHA = strings.Repeat("b", 40) }, true},
		{"wrong-pr", func(_ *db.Run, _ *db.Repo, request *ipc.RefreshPRParams) { request.PRURL += "2" }, true},
		{"wrong-generation", func(_ *db.Run, _ *db.Repo, request *ipc.RefreshPRParams) { request.PushGeneration++ }, true},
		{"unpublished", func(run *db.Run, _ *db.Repo, _ *ipc.RefreshPRParams) { run.LastPushedSHA = nil }, true},
		{"active-push", func(run *db.Run, _ *db.Repo, _ *ipc.RefreshPRParams) { run.PushActive = true }, true},
		{"active-run", func(run *db.Run, _ *db.Repo, _ *ipc.RefreshPRParams) { run.Status = types.RunRunning }, true},
		{"closed-record", func(run *db.Run, _ *db.Repo, _ *ipc.RefreshPRParams) { state := "closed"; run.PRState = &state }, true},
		{"changed-target", func(_ *db.Run, repo *db.Repo, _ *ipc.RefreshPRParams) { repo.UpstreamURL += "different" }, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			head, url, reference := strings.Repeat("a", 40), "https://github.com/owner/repo/pull/17", "refs/heads/feature"
			state := "open"
			generation := int64(1)
			repo := &db.Repo{ID: "repo", UpstreamURL: "https://github.com/owner/repo.git"}
			fingerprint := branchsync.TargetFingerprint(repo.PushURL())
			run := &db.Run{ID: "selected", RepoID: repo.ID, Branch: "feature", HeadSHA: head, Status: types.RunCompleted, PRURL: &url, PRState: &state, LastPushedSHA: &head, PushGeneration: &generation, PushRef: &reference, PushTargetFingerprint: &fingerprint}
			request := ipc.RefreshPRParams{RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: head, PRURL: url, PushGeneration: generation}
			testCase.change(run, repo, &request)
			if err := validatePRRefreshIdentity(run, repo, request); (err != nil) != testCase.isRefused {
				t.Fatalf("identity refusal=%v, error=%v", testCase.isRefused, err)
			}
		})
	}
}

func TestHandleRefreshPR(t *testing.T) {
	for _, testCase := range []string{"exact", "publish-error", "cancelled", "active-executor", "replaced-run", "wrong-request", "missing-native-pr", "missing-test-exception"} {
		t.Run(testCase, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo, err := database.InsertRepo(t.TempDir(), "https://github.com/owner/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range []error{
				database.UpdateRunStatus(run.ID, types.RunCompleted),
				database.UpdateRunPRURL(run.ID, "https://github.com/owner/repo/pull/17"),
				database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, Ref: "refs/heads/feature", TargetFingerprint: branchsync.TargetFingerprint(repo.PushURL())}),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			if testCase != "missing-native-pr" {
				step, err := database.InsertStepResult(run.ID, types.StepPR)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.CompleteStep(step.ID, 0, 47, ""); err != nil {
					t.Fatal(err)
				}
			}
			if testCase != "missing-test-exception" {
				step, err := database.InsertStepResult(run.ID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				for _, err := range []error{
					database.SetStepFindings(step.ID, `{"verdict":"inconclusive","findings":[]}`),
					database.UpdateStepStatus(step.ID, types.StepStatusAwaitingApproval),
					database.SetTestApprovalReason(step.ID, "Accept retained checks; seven repair contracts remain untested"),
					database.CompleteStep(step.ID, 0, 55, ""),
				} {
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manager := NewRunManager(database, p, nil)
			publishCalls := 0
			manager.refreshPR = func(stepContext *pipeline.StepContext) (bool, error) {
				publishCalls++
				deadline, hasDeadline := stepContext.Ctx.Deadline()
				if stepContext.Run.ID != run.ID || stepContext.Run.HeadSHA != run.HeadSHA || stepContext.Repo.ID != repo.ID || stepContext.DB != database || stepContext.Agent != nil || stepContext.Fixing || !hasDeadline || time.Until(deadline) > 30*time.Second {
					t.Error("publication context lost exact identity, bypassed the bound or launched validation")
				}
				if testCase == "publish-error" {
					return false, errors.New("uncertain publication outcome")
				}
				return true, nil
			}
			request := ipc.RefreshPRParams{RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, HeadSHA: run.HeadSHA, PRURL: *run.PRURL, PushGeneration: *run.PushGeneration}
			switch testCase {
			case "active-executor":
				manager.executors[run.ID] = &pipeline.Executor{}
			case "replaced-run":
				if _, err := database.InsertRun(repo.ID, run.Branch, run.HeadSHA, run.BaseSHA); err != nil {
					t.Fatal(err)
				}
			case "wrong-request":
				request.HeadSHA = strings.Repeat("c", 40)
			}
			beforeSteps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase == "cancelled" {
				cancel()
			}
			result, err := manager.HandleRefreshPR(ctx, request)
			if testCase == "exact" {
				if err != nil || result == nil || !result.Updated || result.RunID != run.ID || publishCalls != 1 {
					t.Fatalf("exact refresh failed: result=%+v calls=%d error=%v", result, publishCalls, err)
				}
			} else if err == nil || result != nil {
				t.Fatalf("unsafe refresh accepted: result=%+v error=%v", result, err)
			} else if testCase == "publish-error" {
				if publishCalls != 1 || !strings.Contains(err.Error(), "uncertain publication") {
					t.Fatalf("uncertain write retried or hidden: calls=%d error=%v", publishCalls, err)
				}
			} else {
				want := map[string]string{"cancelled": "canceled", "active-executor": "active executor", "replaced-run": "latest run", "wrong-request": "identity changed", "missing-native-pr": "native PR", "missing-test-exception": "recorded Test exception"}[testCase]
				if publishCalls != 0 || !strings.Contains(err.Error(), want) {
					t.Fatalf("wrong refusal: wanted %q, got %v", want, err)
				}
			}
			after, err := database.GetRun(run.ID)
			if err != nil || !reflect.DeepEqual(run, after) {
				t.Fatalf("refused refresh changed the run: error=%v", err)
			}
			afterSteps, err := database.GetStepsByRun(run.ID)
			if err != nil || !reflect.DeepEqual(beforeSteps, afterSteps) {
				t.Fatalf("refresh changed validation evidence, spending or budget records: error=%v", err)
			}
		})
	}
}

func TestRefreshPRRPCRefusesUnknownRun(t *testing.T) {
	p, _ := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, testCase := range []struct {
		name    string
		params  json.RawMessage
		message string
	}{
		{"unknown-run", json.RawMessage(`{"run_id":"missing"}`), "run was not found"},
		{"malformed-request", json.RawMessage(`"not an object"`), "invalid params"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var result ipc.RefreshPRResult
			if err := client.CallWithContext(context.Background(), ipc.MethodRefreshPR, testCase.params, &result, 5*time.Second); err == nil || !strings.Contains(err.Error(), testCase.message) || result.Updated {
				t.Fatalf("RPC did not refuse before publication: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestHandleRefreshPR_RefusesBusyBranch(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := NewRunManager(database, p, nil)
	request := ipc.RefreshPRParams{RunID: "selected", RepoID: "repo", Branch: "feature"}
	lock := manager.branchMutex(request.RepoID, request.Branch)
	lock.Lock()
	isLocked := true
	defer func() {
		if isLocked {
			lock.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := manager.HandleRefreshPR(context.Background(), request)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "branch is busy") {
			t.Fatalf("busy branch was not refused: %v", err)
		}
	case <-time.After(2 * time.Second):
		lock.Unlock()
		isLocked = false
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("refresh did not stop after releasing the fixture lock")
		}
		t.Fatal("refresh waited for another branch operation instead of refusing")
	}
}
