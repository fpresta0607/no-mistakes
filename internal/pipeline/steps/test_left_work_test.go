package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// leftWorkError is what a turn that requires completed children returns when
// the agent ended with work it started still running: a stopped Monitor's
// tail and grep (issue #319), a server or test run whose stop did not reach
// the processes under its shell (issue #363), or a re-run left waiting for its
// test-line turn (issue #416).
var leftWorkError = fmt.Errorf("claude completion is unverified: background work outlived the result by 10s: %w: agent exited with active owned children: pid 7 (Git usr/bin/tail.exe), pid 8 (Git usr/bin/grep.exe)", agent.ErrCompletionUnverified)

// A Test turn whose agent left its own work running has no result that counts,
// but that is not a code failure: the step parks for a decision with the
// turn's edits kept, instead of failing the run and losing a passed baseline or
// an uncommitted repair.
func TestTestStep_TurnThatLeftItsOwnWorkRunningParks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		isFixing         bool
		isApproveRefused bool
	}{
		{name: "evidence turn after a passing baseline"},
		{name: "repair turn keeps its uncommitted edit", isFixing: true, isApproveRefused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if tc.isFixing {
					if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
						return nil, err
					}
				}
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, leftWorkError
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
			sctx.Fixing = tc.isFixing

			// Act
			outcome, err := (&TestStep{}).Execute(sctx)

			// Assert
			if err != nil {
				t.Fatalf("Execute() error = %v, want the step parked for a decision", err)
			}
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("outcome = %#v, want the Test step parked for a decision", outcome)
			}
			if calls != 1 {
				t.Fatalf("agent calls = %d, want the cut turn alone", calls)
			}
			notice := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentLeftWork)
			if notice.Action != types.ActionAskUser || !strings.Contains(notice.Description, "tail.exe") || !strings.Contains(notice.Description, "result does not count") {
				t.Fatalf("finding = %+v, want an ask-user notice naming the processes and refusing the result", notice)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range findings.Items {
				if item.ID == types.FindingIDTestAgentTimeout {
					t.Fatalf("findings = %+v, want no budget-cut notice for a turn that was not cut at its budget", findings.Items)
				}
			}
			if got := pipeline.HasUnvalidatedWorkRefusal(outcome.Findings); got != tc.isApproveRefused {
				t.Fatalf("approve refused = %t, want %t: only a turn that left unvalidated edits refuses approval", got, tc.isApproveRefused)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
			}
			if tc.isFixing {
				if got := gitCmd(t, dir, "status", "--porcelain", "--", "fix.txt"); got != "?? fix.txt" {
					t.Fatalf("fix.txt status = %q, want the repair kept uncommitted", got)
				}
			}
		})
	}
}

// Fixing only the notice runs validation again with no repair turn, as fixing
// a budget cut alone does: there is nothing for a repair agent to repair.
func TestTestStep_FixOfALeftWorkNoticeAloneRerunsOnlyValidation(t *testing.T) {
	t.Parallel()
	// Arrange
	dir, baseSHA, headSHA := setupGitRepo(t)
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-agent-left-work","severity":"warning","action":"ask-user","description":"left work running"}]}`

	// Act
	outcome, err := (&TestStep{}).Execute(sctx)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the re-run validation to pass", outcome)
	}
	if len(prompts) != 1 || strings.Contains(prompts[0], "Fix the failing tests") {
		t.Fatalf("prompts = %q, want exactly one evidence turn and no repair turn", prompts)
	}
}
