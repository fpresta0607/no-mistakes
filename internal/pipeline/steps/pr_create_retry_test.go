package steps

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	ghDNSFailure   = "error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"
	ghResetFailure = `Post "https://api.github.com/graphql": read tcp 10.0.0.2:50000->140.82.112.6:443: read: connection reset by peer`
)

var testPRCreateRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

// prCreateFailureEnv makes the fake gh fail the first failures creates with
// message, and returns the file that counts the creates gh received.
func prCreateFailureEnv(t *testing.T, env []string, failures int, message string, lands bool) ([]string, string) {
	t.Helper()
	countPath := filepath.Join(t.TempDir(), "pr-create-count")
	env = append(env,
		"FAKE_CLI_PR_CREATE_COUNT_PATH="+countPath,
		"FAKE_CLI_PR_CREATE_FAILURES="+strconv.Itoa(failures),
		"FAKE_CLI_PR_CREATE_FAILURE="+message,
	)
	if lands {
		env = append(env, "FAKE_CLI_PR_CREATE_LANDS=1")
	}
	return env, countPath
}

func readPRCreateCount(t *testing.T, countPath string) int {
	t.Helper()
	raw, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// A run whose push landed used to fail at the PR step on one passing
// api.github.com outage, and no route re-ran only that step (issue #291). The
// step now tries again, and a create whose answer was lost is found on the
// branch rather than opened a second time, so the run goes on to CI.
func TestPRStep_ConnectionErrorOnFirstCreateStillReachesCI(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		failure     string
		lands       bool
		wantCreates int
	}{
		{name: "forge unreachable", failure: ghDNSFailure, wantCreates: 2},
		{name: "answer lost after the pull request opened", failure: ghResetFailure, lands: true, wantCreates: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			database, p, run, repo, dir, statePath, env := setupCIGateReconcileTest(t)
			env, countPath := prCreateFailureEnv(t, env, 1, test.failure, test.lands)

			// Act
			gate := runPRStepToCIGate(t, database, p, run, repo, dir, env, &PRStep{createRetryDelays: testPRCreateRetryDelays})
			if err := os.WriteFile(statePath, []byte("MERGED\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := <-gate.done

			// Assert
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := readPRCreateCount(t, countPath); got != test.wantCreates {
				t.Fatalf("gh pr create ran %d times, want %d", got, test.wantCreates)
			}
			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != types.RunCompleted || persisted.PRURL == nil || *persisted.PRURL != "https://github.com/test/repo/pull/42" {
				t.Fatalf("run status=%s PR=%v, want completed with pull/42", persisted.Status, deref(persisted.PRURL))
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if step.Status != types.StepStatusCompleted {
					t.Fatalf("step %s is %s, want completed", step.StepName, step.Status)
				}
			}
		})
	}
}

func TestPRStep_ConnectionErrorRetriesAreBounded(t *testing.T) {
	t.Parallel()
	// Arrange
	_, _, run, _, dir, _, env := setupCIGateReconcileTest(t)
	env, countPath := prCreateFailureEnv(t, env, 99, ghDNSFailure, false)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, run.BaseSHA, run.HeadSHA, config.Commands{})
	publishValidationTextForTest(sctx)
	sctx.Env = env

	// Act
	_, err := (&PRStep{createRetryDelays: testPRCreateRetryDelays}).Execute(sctx)

	// Assert
	if !errors.Is(err, scm.ErrConnection) || !strings.Contains(err.Error(), "error connecting to api.github.com") {
		t.Fatalf("Execute() error = %v, want gh's connection error", err)
	}
	if got, want := readPRCreateCount(t, countPath), len(testPRCreateRetryDelays)+1; got != want {
		t.Fatalf("gh pr create ran %d times, want %d", got, want)
	}
}
