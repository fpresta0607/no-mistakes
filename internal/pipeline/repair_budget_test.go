package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRepairBudgetOutcome(t *testing.T) {
	expiry := errors.New("agent fix reached its invocation budget after 1h0m0s")
	left := &RepairBudgetCutError{Cut: expiry, Left: "uncommitted changes to fix.txt"}
	cases := []struct {
		name      string
		err       error
		wantParks bool
		want      string
		avoid     string
	}{
		{name: "a cut names what it left", err: left, wantParks: true, want: "uncommitted changes to fix.txt"},
		{name: "a wrapped cut still parks", err: fmt.Errorf("step: %w", left), wantParks: true, want: "after 1h0m0s"},
		{name: "a cut that left nothing says so", err: &RepairBudgetCutError{Cut: expiry}, wantParks: true, want: "left no work"},
		{
			name:      "a credential in the cut never reaches the finding",
			err:       &RepairBudgetCutError{Cut: errors.New("fetch https://user:secret@example.com/repo failed")},
			wantParks: true,
			avoid:     "secret",
		},
		{name: "any other error fails as before", err: errors.New("boom")},
		{name: "no error", err: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := RepairBudgetOutcome(tc.err)

			if !tc.wantParks {
				if outcome != nil {
					t.Fatalf("outcome = %+v, want none", outcome)
				}
				return
			}
			if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %+v, want a park for a decision", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].ID != types.FindingIDRepairAgentBudget || findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("findings = %+v, want the one notice", findings.Items)
			}
			if got := findings.Items[0].Description; !strings.Contains(got, tc.want) || (tc.avoid != "" && strings.Contains(got, tc.avoid)) {
				t.Fatalf("description = %q, want %q without %q", got, tc.want, tc.avoid)
			}
		})
	}
}

func TestRepairBudgetCutErrorKeepsItsCause(t *testing.T) {
	cut := &RepairBudgetCutError{Cut: fmt.Errorf("agent fix: %w", ErrAgentTimeout)}

	if !errors.Is(cut, ErrAgentTimeout) || !strings.Contains(cut.Error(), "agent fix") {
		t.Fatalf("cut = %v, want the expiry readable through it", cut)
	}
}

func TestOnlyRepairBudgetCut(t *testing.T) {
	notice := `{"id":"repair-agent-budget","severity":"warning","action":"ask-user","description":"cut"}`
	other := `{"id":"f-1","severity":"error","action":"auto-fix","description":"bug"}`
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"the notice alone", `{"findings":[` + notice + `]}`, true},
		{"the notice and a finding", `{"findings":[` + notice + `,` + other + `]}`, false},
		{"a finding alone", `{"findings":[` + other + `]}`, false},
		{"nothing selected", `{"findings":[]}`, false},
		{"no findings at all", "", false},
		{"unreadable findings", "{", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OnlyRepairBudgetCut(tc.raw); got != tc.want {
				t.Fatalf("OnlyRepairBudgetCut(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRepairBudgetNoticeIsReserved(t *testing.T) {
	if !IsReservedFindingID(types.FindingIDRepairAgentBudget) {
		t.Fatal("a gate report could claim the repair budget notice")
	}
}

// The park reaches the executor as an error from any step that runs a repair
// turn, and a refused approval leaves it parked for fix to run the step again.
func TestExecutor_RepairCutParksUntilFixRunsTheStepAgain(t *testing.T) {
	for _, stepName := range []types.StepName{types.StepReview, types.StepLint, types.StepName("gate.test.budget")} {
		t.Run(string(stepName), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			calls := 0
			var fixing []bool
			step := &adaptiveCallStep{name: stepName, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				fixing = append(fixing, sctx.Fixing)
				if calls == 1 {
					return nil, &RepairBudgetCutError{Cut: errors.New("agent fix reached its invocation budget"), Left: "uncommitted changes to fix.txt"}
				}
				return &StepOutcome{}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			waitForStepStatus(t, database, run.ID, stepName, types.StepStatusAwaitingApproval)

			approveErr := exec.Respond(stepName, types.ActionApprove, nil)
			fixErr := respondFixPartial(t, exec, stepName, types.FindingIDRepairAgentBudget)
			waitExecutorDone(t, done)

			if approveErr == nil || !strings.Contains(approveErr.Error(), "agent budget") {
				t.Fatalf("approve = %v, want it refused at a cut", approveErr)
			}
			if fixErr != nil {
				t.Fatalf("fix after the refused approval: %v", fixErr)
			}
			if calls != 2 || !fixing[1] {
				t.Fatalf("step executions = %d, fixing %v, want fix to run the step once more", calls, fixing)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != types.RunCompleted {
				t.Fatalf("run = %s, want it completed once a round checked the work", stored.Status)
			}
		})
	}
}

// A cut stays a cut at any other step's failure: only the typed error parks.
func TestExecutor_AnAgentTimeoutThatIsNoCutStillFailsTheRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepLint, fn: func(*StepContext) (*StepOutcome, error) {
		return nil, fmt.Errorf("agent lint: %w", ErrAgentTimeout)
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())

	stored, getErr := database.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if err == nil || stored.Status != types.RunFailed {
		t.Fatalf("Execute() = %v, run %s, want the timeout to fail the run", err, stored.Status)
	}
}

// Keeping and checking a cut turn's work decides no finding, so a response that
// says nothing else is accepted over the findings it leaves out and records no
// selection, whose complement would read as declines. Anything more is an
// ordinary response, held to the ordinary accounting. The auto-fix round before
// the cut decided nothing, so every finding here is still undecided.
func TestExecutor_ARepairCutRecheckDecidesNoOtherFinding(t *testing.T) {
	notice := types.FindingIDRepairAgentBudget
	cases := []struct {
		name        string
		fix         []string
		ignore      []string
		added       []types.Finding
		wantMissing []string
	}{
		{name: "the notice alone", fix: []string{notice}},
		{name: "the notice and a decline", fix: []string{notice}, ignore: []string{"f-1"}, wantMissing: []string{"f-2"}},
		{
			name:        "the notice and an added finding",
			fix:         []string{notice},
			added:       []types.Finding{{Severity: "error", Description: "also this", Action: types.ActionAutoFix}},
			wantMissing: []string{"f-1", "f-2"},
		},
		{name: "a finding without the notice", fix: []string{"f-1"}, wantMissing: []string{"f-2", notice}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			calls := 0
			reran := make(chan struct{})
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				switch calls {
				case 1:
					return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"f-1","severity":"error","file":"a.go","line":1,"description":"one","action":"auto-fix"},{"id":"f-2","severity":"error","file":"b.go","line":1,"description":"two","action":"ask-user"}]}`}, nil
				case 2:
					return nil, &RepairBudgetCutError{Cut: errors.New("agent fix reached its invocation budget")}
				default:
					close(reran)
					return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[]}`}, nil
				}
			}}
			cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}}
			exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
			_, cancel := startExecutor(t, exec, run, repo, t.TempDir())
			defer cancel()
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

			_, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, tc.fix, tc.ignore, nil, tc.added, "")

			if len(tc.wantMissing) > 0 {
				var refusal *RespondRefusal
				if !errors.As(err, &refusal) || !slices.Equal(refusal.Missing, tc.wantMissing) {
					t.Fatalf("fix = %v, want refused for leaving out %v", err, tc.wantMissing)
				}
				return
			}
			if err != nil {
				t.Fatalf("fix: %v", err)
			}
			select {
			case <-reran:
			case <-time.After(30 * time.Second):
				t.Fatal("the step never ran again")
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := database.GetRoundsByStep(steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) < 2 || rounds[1].SelectedFindingIDs != nil {
				t.Fatalf("rounds = %+v, want the cut round left with no recorded selection", rounds)
			}
		})
	}
}
