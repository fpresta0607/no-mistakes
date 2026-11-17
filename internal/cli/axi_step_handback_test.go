package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type stepHandbackCLIFixture struct {
	local    string
	database *db.DB
	run      *db.Run
	requests chan ipc.StepHandbackParams
}

// setupStepHandbackCLI parks name on this branch's active run behind a fake
// daemon that records every step handback request it receives.
func setupStepHandbackCLI(t *testing.T, name types.StepName) *stepHandbackCLIFixture {
	t.Helper()
	fixture := &stepHandbackCLIFixture{requests: make(chan ipc.StepHandbackParams, 1)}
	olderDaemonFixture(t, nil, func(server *ipc.Server) {
		server.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
			active := fixture.run
			return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{ID: active.ID, RepoID: active.RepoID, Branch: active.Branch, HeadSHA: active.HeadSHA, Status: active.Status}}, nil
		})
		server.Handle(ipc.MethodStepHandback, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var request ipc.StepHandbackParams
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			fixture.requests <- request
			return &ipc.StepHandbackResult{RunID: request.RunID, Step: name, RoundID: "round", ReceiptID: "step-receipt", CallerDir: request.CallerDir, ReleasedHead: request.HeadSHA, ReleasedRef: "refs/no-mistakes/step-handback/run/round"}, nil
		})
	})
	local, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(root.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repository, err := findRepo(database)
	if err != nil {
		t.Fatal(err)
	}
	head := cliGit(t, local, "rev-parse", "HEAD")
	active, err := database.InsertRun(repository.ID, cliGit(t, local, "branch", "--show-current"), head, head)
	if err != nil {
		t.Fatal(err)
	}
	parkStepForCLIHandback(t, database, active, name)
	fixture.local, fixture.database = local, database
	fixture.run, err = database.GetRun(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func parkStepForCLIHandback(t *testing.T, database *db.DB, run *db.Run, name types.StepName) *db.StepHandback {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"failure-1","severity":"error","action":"auto-fix","description":"failed"}]}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	return &db.StepHandback{RunID: run.ID, StepResultID: step.ID, StepName: name, RoundID: round.ID, CallerDir: "caller", ReleasedHead: run.HeadSHA}
}

func runAxiHandback(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newAxiHandbackCmd()
	command.SetContext(t.Context())
	command.SetArgs(args)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	err := command.Execute()
	return output.String(), err
}

func TestAxiHandbackReleasesAParkedTestOrLintFailure(t *testing.T) {
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		t.Run(string(name), func(t *testing.T) {
			// Arrange
			fixture := setupStepHandbackCLI(t, name)

			// Act
			output, err := runAxiHandback(t)

			// Assert
			if err != nil {
				t.Fatalf("handback: %v\n%s", err, output)
			}
			select {
			case request := <-fixture.requests:
				if request.Resume || request.RunID != fixture.run.ID || request.Branch != fixture.run.Branch || request.HeadSHA != fixture.run.HeadSHA {
					t.Fatalf("CLI sent %#v", request)
				}
			default:
				t.Fatal("CLI never reached the step handback boundary")
			}
			for _, required := range []string{"step: " + string(name), "id: step-receipt", "state: released", "resume_step_handback", "no-mistakes axi handback --resume --receipt step-receipt"} {
				if !strings.Contains(output, required) {
					t.Errorf("release output missing %q:\n%s", required, output)
				}
			}
		})
	}
}

func TestAxiHandbackRefusesAStepReceiptFromAnotherRun(t *testing.T) {
	// Arrange
	fixture := setupStepHandbackCLI(t, types.StepTest)
	other, err := fixture.database.InsertRun(fixture.run.RepoID, "other", fixture.run.HeadSHA, fixture.run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	foreign := parkStepForCLIHandback(t, fixture.database, other, types.StepTest)
	if err := fixture.database.ReleaseStepHandback(foreign); err != nil {
		t.Fatal(err)
	}

	// Act
	output, err := runAxiHandback(t, "--resume", "--receipt", foreign.ID)

	// Assert
	if err == nil || !strings.Contains(output, "does not identify this branch's current run") {
		t.Fatalf("resume with another run's receipt = %v\n%s", err, output)
	}
	select {
	case request := <-fixture.requests:
		t.Fatalf("a foreign receipt reached the daemon: %#v", request)
	default:
	}
}
