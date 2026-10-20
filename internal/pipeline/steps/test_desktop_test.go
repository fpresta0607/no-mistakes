package steps

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

const testDesktopRule = "You run on a hidden desktop of your own that nobody sees"

// passingTestEvidence is a complete evidence turn that passes.
const passingTestEvidence = `{"summary":"repair test failure","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`

// Not parallel: it replaces newTestDesktop for the whole package.
func TestTestStep_StartsNoAgentWithoutAHiddenDesktop(t *testing.T) {
	refused := errors.New("desktop heap exhausted")
	old := newTestDesktop
	newTestDesktop = func() (*winproc.HiddenDesktop, error) { return nil, refused }
	t.Cleanup(func() { newTestDesktop = old })
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","file":"fix.txt","description":"test failed"}]}`

	_, err := (&TestStep{}).Execute(sctx)

	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "so that no program they start shows on the user's screen") {
		t.Fatalf("Execute() error = %v, want the hidden desktop's refusal", err)
	}
	if len(ag.calls) != 0 {
		t.Errorf("the step started %d agent turns without a hidden desktop, want none", len(ag.calls))
	}
}

func TestTestStep_RunsEveryAgentTurnOnOneHiddenDesktop(t *testing.T) {
	t.Parallel()
	isHidden := runtime.GOOS == "windows"
	for _, tc := range []struct {
		name      string
		isFixing  bool
		wantCalls int
	}{
		{"evidence_turn", false, 1},
		{"fix_round", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingTestEvidence)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			if tc.isFixing {
				sctx.Fixing = true
				sctx.PreviousFindings = `{"items":[{"id":"test-1","severity":"error","file":"fix.txt","description":"test failed"}]}`
			}

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatalf("test step failed: %v", err)
			}

			if len(ag.calls) != tc.wantCalls {
				t.Fatalf("agent calls = %d, want %d", len(ag.calls), tc.wantCalls)
			}
			desktop := ag.calls[0].Desktop
			if isHidden != strings.HasPrefix(desktop, "no-mistakes-") {
				t.Fatalf("the first turn ran on desktop %q; a hidden desktop on %s: %v", desktop, runtime.GOOS, isHidden)
			}
			for i, call := range ag.calls {
				if call.Desktop != desktop {
					t.Errorf("turn %d ran on desktop %q, want the step's one desktop %q", i+1, call.Desktop, desktop)
				}
				if strings.Contains(call.Prompt, testDesktopRule) != isHidden {
					t.Errorf("turn %d's prompt states the hidden desktop rule: %v, want %v:\n%s", i+1, !isHidden, isHidden, call.Prompt)
				}
			}
		})
	}
}

func TestReportTestWindowsNamesEachWindowWithoutChangingTheVerdict(t *testing.T) {
	t.Parallel()
	opened := []winproc.Window{
		{PID: 43548, Image: "CodeGoblinsSetup-head-dev.exe", Title: "Code Goblins Setup"},
		{PID: 7, Title: ""},
	}
	for _, tc := range []struct {
		name      string
		more      int
		wantItems int
	}{
		{"every_window_recorded", 0, 2},
		{"more_than_recorded", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged []string
			sctx := &pipeline.StepContext{Log: func(line string) { logged = append(logged, line) }}
			outcome := &pipeline.StepOutcome{Findings: passingTestEvidence}

			got, err := reportTestWindows(sctx, outcome, nil, opened, tc.more)

			if err != nil {
				t.Fatal(err)
			}
			findings, err := types.ParseFindingsJSON(got.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != tc.wantItems || got.NeedsApproval || findings.Verdict != "go" {
				t.Fatalf("findings = %+v, needs approval %v; want %d informational items and the passing verdict kept", findings, got.NeedsApproval, tc.wantItems)
			}
			for i, item := range findings.Items {
				if item.Severity != "info" || item.Action != "no-op" {
					t.Errorf("item %d = %+v, want an informational no-op finding", i, item)
				}
			}
			if !strings.Contains(findings.Items[0].Description, `CodeGoblinsSetup-head-dev.exe (pid 43548) titled "Code Goblins Setup"`) ||
				!strings.Contains(findings.Items[1].Description, "a program (pid 7) with no title") {
				t.Errorf("the findings do not name the windows: %+v", findings.Items)
			}
			if tc.more > 0 && !strings.Contains(findings.Items[2].Description, "opened 3 more windows") {
				t.Errorf("the findings do not count the windows past the recorded ones: %+v", findings.Items[2])
			}
			if len(logged) != tc.wantItems {
				t.Errorf("the step log has %d lines, want one per finding: %q", len(logged), logged)
			}
		})
	}
}

func TestReportTestWindowsLeavesAnOutcomeWithoutWindowsAlone(t *testing.T) {
	t.Parallel()
	var logged []string
	sctx := &pipeline.StepContext{Log: func(line string) { logged = append(logged, line) }}
	outcome := &pipeline.StepOutcome{Findings: passingTestEvidence}
	before := outcome.Findings

	got, err := reportTestWindows(sctx, outcome, nil, nil, 0)

	if err != nil || got.Findings != before || len(logged) != 0 {
		t.Fatalf("an outcome without windows changed: err %v, findings %s, log %q", err, got.Findings, logged)
	}
}
