package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newAbortQuiescenceFixture drives the public abort surfaces against a fake
// daemon that scripts the exact run-state reads: cancellation is always
// accepted, but what the post-cancel terminal wait observes is controlled per
// test. The registered repo, worktree, socket, and NM_HOME are all real, so
// `axi abort` runs end to end through the CLI.
func newAbortQuiescenceFixture(t *testing.T, getRun func(context.Context, int) (*ipc.RunInfo, error), cancelAfterRequest ...func()) {
	t.Helper()
	newAbortQuiescenceFixtureWithCancel(t, getRun, nil, cancelAfterRequest...)
}

func newAbortQuiescenceFixtureWithCancel(t *testing.T, getRun func(context.Context, int) (*ipc.RunInfo, error), cancelErr error, cancelAfterRequest ...func()) {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)

	root := t.TempDir()
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, local, "add", "file.txt")
	cliGit(t, local, "commit", "-m", "base")
	cliGit(t, local, "checkout", "-b", "feature/abort")

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertRepo(registeredRoot, filepath.Join(root, "remote.git"), "main"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: false}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
			ID: "run-quiesce", Branch: "feature/abort", Status: types.RunRunning,
		}}, nil
	})
	srv.Handle(ipc.MethodCancelRun, func(context.Context, json.RawMessage) (interface{}, error) {
		for _, cancel := range cancelAfterRequest {
			cancel()
		}
		if cancelErr != nil {
			return nil, cancelErr
		}
		return &ipc.CancelRunResult{OK: true}, nil
	})
	calls := 0
	srv.Handle(ipc.MethodGetRun, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		calls++
		run, err := getRun(ctx, calls)
		if err != nil {
			return nil, err
		}
		return &ipc.GetRunResult{Run: run}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client, dialErr := ipc.Dial(p.Socket()); dialErr == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	previousTimeout := abortStateWaitTimeout
	abortStateWaitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { abortStateWaitTimeout = previousTimeout })

	chdir(t, local)
}

func runningRunForever(context.Context, int) (*ipc.RunInfo, error) {
	return &ipc.RunInfo{ID: "run-quiesce", Branch: "feature/abort", Status: types.RunRunning}, nil
}

func assertUnconfirmedAbortOutput(t *testing.T, surface, out string) {
	t.Helper()
	for _, want := range []string{"cancellation was requested", "terminal quiescence", "unconfirmed", "run-quiesce"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s unconfirmed output missing %q:\n%s", surface, want, out)
		}
	}
	for _, forbidden := range []string{"aborted: true", "state: user_owned", "recover_custody", "branch_sync:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s presented %q while the run was not confirmed terminal:\n%s", surface, forbidden, out)
		}
	}
}

// TestAxiAbortRefusesSuccessWhileTerminalQuiescenceUnconfirmed pins the
// accepted abort contract on the ordinary surface: when terminalization is
// delayed past the bounded wait, or the status read fails, abort must exit
// nonzero, state the unconfirmed condition explicitly, include the last
// structured run state when one is available, and never present a completed
// abort or authoritative ownership guidance.
func TestAxiAbortRefusesSuccessWhileTerminalQuiescenceUnconfirmed(t *testing.T) {
	t.Run("terminalization delayed past bounded wait", func(t *testing.T) {
		newAbortQuiescenceFixture(t, runningRunForever)
		out, err := executeCmd("axi", "abort")
		t.Logf("branch-scoped delayed-terminalization CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("abort with delayed terminalization must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort", out)
		if !strings.Contains(out, "status: running") {
			t.Errorf("unconfirmed abort omitted the last structured run state:\n%s", out)
		}
	})
	t.Run("status read failure", func(t *testing.T) {
		newAbortQuiescenceFixture(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return nil, errors.New("scripted status read failure")
		})
		out, err := executeCmd("axi", "abort")
		t.Logf("branch-scoped status-read-failure CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("abort with failing status reads must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort", out)
	})
}

// TestAxiAbortByRunIDRefusesSuccessWhileTerminalQuiescenceUnconfirmed pins the
// same contract on explicit --run cancellation, which previously reported
// success without any terminal wait at all, and proves the confirmed path
// still reports the completed abort with the observed terminal state.
func TestAxiAbortByRunIDRefusesSuccessWhileTerminalQuiescenceUnconfirmed(t *testing.T) {
	t.Run("terminalization delayed past bounded wait", func(t *testing.T) {
		newAbortQuiescenceFixture(t, runningRunForever)
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("explicit-run delayed-terminalization CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("abort --run with delayed terminalization must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
		if !strings.Contains(out, "status: running") {
			t.Errorf("unconfirmed abort --run omitted the last structured run state:\n%s", out)
		}
	})
	t.Run("status read failure", func(t *testing.T) {
		newAbortQuiescenceFixture(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return nil, errors.New("scripted status read failure")
		})
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("explicit-run status-read-failure CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("abort --run with failing status reads must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
	})
	t.Run("confirmed terminal reports completion", func(t *testing.T) {
		newAbortQuiescenceFixture(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return &ipc.RunInfo{ID: "run-quiesce", Branch: "feature/abort", Status: types.RunCancelled}, nil
		})
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("explicit-run confirmed-terminal CLI output:\n%s", out)
		if err != nil {
			t.Fatalf("confirmed abort --run: %v\n%s", err, out)
		}
		for _, want := range []string{"aborted: true", "run-quiesce", "cancelled"} {
			if !strings.Contains(out, want) {
				t.Errorf("confirmed abort --run missing %q:\n%s", want, out)
			}
		}
	})
}

func TestAxiAbortStatusReadStallRespectsBoundedWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "branch scoped", args: []string{"axi", "abort"}},
		{name: "explicit run", args: []string{"axi", "abort", "--run", "run-quiesce"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newAbortQuiescenceFixture(t, func(ctx context.Context, _ int) (*ipc.RunInfo, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
			started := time.Now()
			out, err := executeCmd(tc.args...)
			t.Logf("%s stalled-status-read CLI output:\n%s", tc.name, out)
			if err == nil {
				t.Fatalf("abort with a stalled status read must exit nonzero:\n%s", out)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("stalled status read exceeded bounded wait: %s\n%s", elapsed, out)
			}
			assertUnconfirmedAbortOutput(t, tc.name, out)
			if !strings.Contains(out, "in-flight run state read did not complete") {
				t.Errorf("stalled status read omitted precise timeout reason:\n%s", out)
			}
		})
	}
}

func TestAxiAbortCancelledWaitRefusesSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "branch scoped", args: []string{"axi", "abort"}},
		{name: "explicit run", args: []string{"axi", "abort", "--run", "run-quiesce"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			newAbortQuiescenceFixture(t, runningRunForever, cancel)
			out, err := executeCmdWithContext(ctx, tc.args...)
			t.Logf("%s cancelled-wait CLI output:\n%s", tc.name, out)
			if err == nil {
				t.Fatalf("abort with a cancelled wait must exit nonzero:\n%s", out)
			}
			assertUnconfirmedAbortOutput(t, tc.name, out)
			if !strings.Contains(out, "cancelled") {
				t.Errorf("cancelled wait omitted its precise reason:\n%s", out)
			}
		})
	}
}

// noActiveRunErr mirrors the daemon's cancel_run refusal for an id that is not
// currently active, which is the ambiguous signal the terminal-truth contract
// must resolve through the durable run record instead of trusting blindly.
func noActiveRunErr(id string) error {
	return errors.New("no active run " + id)
}

// TestAxiAbortByRunIDResolvesDurableTerminalTruth pins the accepted
// explicit-run terminal-truth contract: a bare `no active run` result from
// cancel_run is never sufficient by itself. The exact run's durable state
// decides the outcome - an already-terminal run returns idempotent success
// with its terminal run_status and no fabricated new cancellation, a
// still-nonterminal run is a nonzero unconfirmed result, an unreadable run is
// unconfirmed, and only a positively proven unknown id keeps the documented
// no-op.
func TestAxiAbortByRunIDResolvesDurableTerminalTruth(t *testing.T) {
	t.Run("known terminal run returns idempotent terminal truth", func(t *testing.T) {
		newAbortQuiescenceFixtureWithCancel(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return &ipc.RunInfo{ID: "run-quiesce", Branch: "feature/abort", Status: types.RunCancelled}, nil
		}, noActiveRunErr("run-quiesce"))
		for round := 0; round < 2; round++ {
			out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
			t.Logf("terminal-truth round %d CLI output:\n%s", round, out)
			if err != nil {
				t.Fatalf("round %d: known terminal run must resolve idempotently: %v\n%s", round, err, out)
			}
			for _, want := range []string{"run-quiesce", "run_status: cancelled"} {
				if !strings.Contains(out, want) {
					t.Errorf("round %d output missing %q:\n%s", round, want, out)
				}
			}
			if strings.Contains(out, "aborted: true") {
				t.Errorf("round %d fabricated a new cancellation:\n%s", round, out)
			}
		}
	})
	t.Run("no-active cancel with still-active run is unconfirmed", func(t *testing.T) {
		newAbortQuiescenceFixtureWithCancel(t, runningRunForever, noActiveRunErr("run-quiesce"))
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("inconsistent-daemon CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("no-active cancel with a still-active run must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
		if !strings.Contains(out, "status: running") {
			t.Errorf("inconsistent-daemon refusal omitted the observed run state:\n%s", out)
		}
	})
	t.Run("unreadable run is unconfirmed", func(t *testing.T) {
		newAbortQuiescenceFixtureWithCancel(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return nil, errors.New("scripted status read failure")
		}, noActiveRunErr("run-quiesce"))
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("unreadable-run CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("no-active cancel with an unreadable run must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
	})
	t.Run("nil run response is unconfirmed", func(t *testing.T) {
		newAbortQuiescenceFixtureWithCancel(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return nil, nil
		}, noActiveRunErr("run-quiesce"))
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("nil-run CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("nil get_run response must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
	})
	t.Run("mismatched run response is unconfirmed", func(t *testing.T) {
		newAbortQuiescenceFixtureWithCancel(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return &ipc.RunInfo{ID: "run-other", Branch: "feature/other", Status: types.RunCancelled}, nil
		}, noActiveRunErr("run-quiesce"))
		out, err := executeCmd("axi", "abort", "--run", "run-quiesce")
		t.Logf("mismatched-run CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("mismatched get_run response must exit nonzero:\n%s", out)
		}
		assertUnconfirmedAbortOutput(t, "abort --run", out)
		if strings.Contains(out, "run_status: cancelled") {
			t.Fatalf("mismatched get_run response supplied terminal truth:\n%s", out)
		}
	})
	t.Run("genuinely unknown id keeps documented no-op", func(t *testing.T) {
		newAbortQuiescenceFixtureWithCancel(t, func(context.Context, int) (*ipc.RunInfo, error) {
			return nil, errors.New("run not found: run-unknown")
		}, noActiveRunErr("run-unknown"))
		out, err := executeCmd("axi", "abort", "--run", "run-unknown")
		t.Logf("unknown-id CLI output:\n%s", out)
		if err != nil {
			t.Fatalf("genuinely unknown id must stay a documented no-op: %v\n%s", err, out)
		}
		for _, want := range []string{"aborted: false", "run-unknown", "no-op"} {
			if !strings.Contains(out, want) {
				t.Errorf("unknown-id no-op missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "run_status") {
			t.Errorf("unknown-id no-op fabricated terminal state:\n%s", out)
		}
	})
}

func TestAxiAbortTerminalRunsThroughDaemon(t *testing.T) {
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	chdir(t, t.TempDir())
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	startTestDaemon(t, p, database)
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	const head = "1111111111111111111111111111111111111111"
	const base = "2222222222222222222222222222222222222222"
	for _, status := range []types.RunStatus{types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunCIMonitorInterrupted} {
		t.Run(string(status), func(t *testing.T) {
			run, err := database.InsertRun(repo.ID, "terminal/"+string(status), head, base)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, status); err != nil {
				t.Fatal(err)
			}
			before, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			for attempt := 0; attempt < 2; attempt++ {
				output, err := executeCmd("axi", "abort", "--run", run.ID)
				if err != nil {
					t.Fatalf("terminal abort attempt %d failed: %v\n%s", attempt, err, output)
				}
				for _, required := range []string{"aborted: false", "run_status: " + string(status), run.ID, "idempotent no-op"} {
					if !strings.Contains(output, required) {
						t.Fatalf("terminal abort omitted %q:\n%s", required, output)
					}
				}
			}

			after, err := database.GetRun(run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal abort changed the run: %#v %v", after, err)
			}
		})
	}
	t.Run("repeated released-reservation abort", func(t *testing.T) {
		run, err := database.InsertRun(repo.ID, "feature/released", head, base)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range []error{
			database.UpdateRunStatus(run.ID, types.RunRunning),
			database.UpdateRunPRURL(run.ID, "https://github.com/example/repo/pull/1"),
			database.UpdateRunPRState(run.ID, "open"),
			database.UpdateRunReviewApprovedHeadSHA(run.ID, head),
			database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "published-target", Ref: "refs/heads/feature/released"}),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
		const findings = `{"findings":[{"id":"ci-1","category":"ci-check","severity":"error","action":"auto-fix","check":"tests","description":"tests failed"}]}`
		receipt := &db.CIHandback{RunID: run.ID, CallerDir: repo.WorkingPath, ReleasedHead: head, PRURL: "https://github.com/example/repo/pull/1", BaseBranch: "main", BaseHead: base, PushGeneration: 1, TargetFingerprint: "published-target", PushRef: "refs/heads/feature/released"}
		history := make(map[string][]*db.StepRound)
		limits := make(map[types.StepName]int)
		for _, name := range types.AllSteps() {
			limits[name] = 0
			step, err := database.InsertStepResult(run.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			output := `{"findings":[]}`
			if name == types.StepCI {
				output = findings
			}
			round, err := database.InsertStepRound(step.ID, 1, "initial", &output, nil, 20)
			if err != nil {
				t.Fatal(err)
			}
			if name == types.StepCI {
				receipt.RoundID = round.ID
				err = database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 20, &output)
			} else {
				err = database.CompleteStep(step.ID, 0, 20, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			history[step.ID], err = database.GetRoundsByStep(step.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		run, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		receipt.Source = &db.CIHandbackSource{CallerHead: head, SubmittedHead: head, ReviewHead: head, PushKind: "upstream", PushedAt: *run.LastPushedAt, Findings: findings, AutoFixLimits: limits}
		if err := database.ReleaseCIHandback(receipt, run, repo, findings); err != nil {
			t.Fatal(err)
		}

		output, err := executeCmd("axi", "abort", "--run", run.ID)
		if err != nil || !strings.Contains(output, "aborted: true") || !strings.Contains(output, "run_status: cancelled") {
			t.Fatalf("released reservation abort failed: %v\n%s", err, output)
		}
		before, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		beforeSteps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			output, err := executeCmd("axi", "abort", "--run", run.ID)
			if err != nil || !strings.Contains(output, "aborted: false") || !strings.Contains(output, "run_status: cancelled") || !strings.Contains(output, "idempotent no-op") {
				t.Fatalf("repeated released reservation abort failed: %v\n%s", err, output)
			}
		}

		after, err := database.GetRun(run.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("repeated abort changed the run: %#v %v", after, err)
		}
		afterSteps, err := database.GetStepsByRun(run.ID)
		if err != nil || !reflect.DeepEqual(beforeSteps, afterSteps) {
			t.Fatalf("repeated abort changed findings or execution spend: %#v %v", afterSteps, err)
		}
		stored, err := database.LatestCIHandback(run.ID)
		if err != nil || !reflect.DeepEqual(receipt, stored) {
			t.Fatalf("abort changed the receipt or repair limits: %#v %v", stored, err)
		}
		for stepID, original := range history {
			rounds, err := database.GetRoundsByStep(stepID)
			if err != nil || !reflect.DeepEqual(original, rounds) {
				t.Fatalf("abort changed validation history: %#v %v", rounds, err)
			}
		}
	})
}

// newDaemonDownAbortFixture prepares NM_HOME with a durable run record and no
// daemon at all, so the daemon-unavailable abort treatment is decided by the
// durable database truth alone.
func newDaemonDownAbortFixture(t *testing.T, status *types.RunStatus) string {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if status == nil {
		return ""
	}
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/abort", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, *status); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// TestAxiAbortByRunIDDaemonUnavailableUsesDurableTruth pins the consistent
// daemon-unavailable treatment: without a daemon nothing can be cancelled, so
// the durable run record alone decides - a recorded terminal run resolves
// idempotently with its terminal status, a recorded nonterminal run is a
// nonzero unconfirmed result (no cancellation was even requested), and an id
// with no durable record keeps the documented no-op without fabricated state.
func TestAxiAbortByRunIDDaemonUnavailableUsesDurableTruth(t *testing.T) {
	t.Run("recorded terminal run resolves idempotently", func(t *testing.T) {
		cancelled := types.RunCancelled
		runID := newDaemonDownAbortFixture(t, &cancelled)
		out, err := executeCmd("axi", "abort", "--run", runID)
		t.Logf("daemon-down terminal CLI output:\n%s", out)
		if err != nil {
			t.Fatalf("daemon-down terminal run must resolve idempotently: %v\n%s", err, out)
		}
		for _, want := range []string{runID, "run_status: cancelled"} {
			if !strings.Contains(out, want) {
				t.Errorf("daemon-down terminal output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "aborted: true") {
			t.Errorf("daemon-down terminal output fabricated a new cancellation:\n%s", out)
		}
	})
	t.Run("recorded nonterminal run is unconfirmed", func(t *testing.T) {
		running := types.RunRunning
		runID := newDaemonDownAbortFixture(t, &running)
		out, err := executeCmd("axi", "abort", "--run", runID)
		t.Logf("daemon-down nonterminal CLI output:\n%s", out)
		if err == nil {
			t.Fatalf("daemon-down nonterminal run must exit nonzero:\n%s", out)
		}
		for _, want := range []string{"terminal quiescence", "unconfirmed", runID} {
			if !strings.Contains(out, want) {
				t.Errorf("daemon-down nonterminal output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "aborted: true") {
			t.Errorf("daemon-down nonterminal output claimed completion:\n%s", out)
		}
	})
	t.Run("unknown id keeps documented no-op", func(t *testing.T) {
		newDaemonDownAbortFixture(t, nil)
		out, err := executeCmd("axi", "abort", "--run", "run-never-existed")
		t.Logf("daemon-down unknown-id CLI output:\n%s", out)
		if err != nil {
			t.Fatalf("daemon-down unknown id must stay a documented no-op: %v\n%s", err, out)
		}
		for _, want := range []string{"aborted: false", "no-op"} {
			if !strings.Contains(out, want) {
				t.Errorf("daemon-down unknown-id no-op missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "run_status") {
			t.Errorf("daemon-down unknown-id no-op fabricated terminal state:\n%s", out)
		}
	})
}
