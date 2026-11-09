package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_FixPromptIncludesSummaryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		template string
		budget   int
	}{
		{"default", "", 72 - len("no-mistakes(ci): ")},
		{"custom prefix", "fix(web): {{.Step}}: {{.Summary}}", 72 - len("fix(web): ci: ")},
		{"repeated summary", "fix: {{.Summary}} / {{.Summary}}", (72 - len("fix:  / ")) / 2},
		{"unicode prefix", "修复({{.Step}}): {{.Summary}}", 72 - utf8.RuneCountInString("修复(ci): ")},
	} {
		for _, mode := range []struct {
			name    string
			targets ciFixTargets
		}{
			{"failing check", ciTargetsFor([]string{"test"}, false)},
			{"check and conflict", ciTargetsFor([]string{"test"}, true)},
			{"conflict only", ciTargetsFor(nil, true)},
			{"behind base", ciFixTargets{BehindBase: true}},
			{"selected finding", ciFixTargetsFromFindings(Findings{Items: []Finding{{
				ID: "ci-1", Category: types.FindingCategoryCIReviewBot,
				Check: "Review", Description: "Repair the selected finding",
			}}})},
		} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				dir, base, head := setupGitRepo(t)
				var prompt string
				ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					prompt = opts.Prompt
					return &agent.Result{Output: json.RawMessage(`{"summary":"external check failure","code_change_needed":false}`)}, nil
				}}
				sctx := newTestContext(t, ag, dir, base, head, config.Commands{})
				sctx.Config.Commit.FixMessage = tc.template
				pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
				repair, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, mode.targets)
				if err != nil {
					t.Fatal(err)
				}
				if repair.HeadAdvanced || gitCmd(t, dir, "rev-parse", "HEAD") != head {
					t.Fatal("no-change conclusion unexpectedly committed a repair")
				}
				if !mode.targets.MergeConflict && !mode.targets.BehindBase && (!repair.NoCodeChangeNeeded || repair.Summary != "external check failure") {
					t.Fatalf("no-code-change conclusion lost: %#v", repair)
				}
				if !strings.Contains(prompt, fixSummaryRules(sctx, types.StepCI)) || !strings.Contains(prompt, budgetRule(tc.budget)) {
					t.Errorf("CI fix prompt missing shared summary rules with budget %d:\n%s", tc.budget, prompt)
				}
				if !strings.Contains(prompt, "Keep detailed diagnosis and verification evidence in your explanatory output") {
					t.Error("CI fix prompt does not preserve diagnosis outside the summary")
				}
			})
		}
	}
}

func TestCIStep_ConciseRepairSummaryPreservesDiagnosticLog(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", head)
	const summary = "correct table count"
	diagnosis := strings.Repeat("The migration adds a table and the test expected the previous count. ", 20)
	var logged strings.Builder
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnChunk(diagnosis)
		if err := os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"` + summary + `","code_change_needed":true}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.CI.RevalidateRepairs = true
	sctx.Config.Commit.FixMessage = "fix(web): {{.Step}}: {{.Summary}}"
	sctx.LogChunk = func(chunk string) { logged.WriteString(chunk) }
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	repair, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false))
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced || !repair.Revalidate || repair.Summary != summary {
		t.Fatalf("repair = %#v, want a locally recorded repair with concise summary", repair)
	}
	if got := gitCmd(t, dir, "log", "-1", "--format=%s"); got != "fix(web): ci: "+summary || utf8.RuneCountInString(got) > 72 {
		t.Fatalf("CI repair subject = %q", got)
	}
	if got := logged.String(); got != diagnosis {
		t.Fatalf("diagnosis log = %q, want original untruncated evidence", got)
	}
}

func TestCIStep_RejectedSummaryPreservesRepairSource(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		output       json.RawMessage
		shouldRefuse bool
		wantSummary  string
	}{
		{name: "unsafe suffix", output: json.RawMessage(`{"summary":"` + strings.Repeat("a", 73) + `\u202e","code_change_needed":true}`), shouldRefuse: true},
		{name: "over byte limit", output: json.RawMessage(`{"summary":"` + strings.Repeat("a", 4097) + `","code_change_needed":true}`), shouldRefuse: true},
		{name: "invalid utf8", output: json.RawMessage("{\"summary\":\"\xff\",\"code_change_needed\":true}"), shouldRefuse: true},
		{name: "normal", output: json.RawMessage(`{"summary":"repair source","code_change_needed":true}`), wantSummary: "repair source"},
		{name: "missing output", wantSummary: "repair failing checks"},
		{name: "invalid json", output: json.RawMessage(`{invalid`), wantSummary: "repair failing checks"},
		{name: "missing decision", output: json.RawMessage(`{"summary":"repair source"}`), wantSummary: "repair failing checks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workDir, base, head := setupGitRepo(t)
			const rawText = "raw CI repair response"
			ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				for _, name := range []string{"feature.txt", "staged.txt", "untracked.txt"} {
					if err := os.WriteFile(filepath.Join(opts.CWD, name), []byte("completed repair\n"), 0o600); err != nil {
						return nil, err
					}
				}
				gitCmd(t, opts.CWD, "add", "staged.txt")
				return &agent.Result{Output: test.output, Text: rawText}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, workDir, base, head, config.Commands{})
			sctx.Config.CI.RevalidateRepairs = true
			logPath := filepath.Join(t.TempDir(), "ci.log")
			log, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { log.Close() })
			sctx.LogFile = func(text string) {
				if _, err := log.WriteString(text + "\n"); err != nil {
					t.Fatal(err)
				}
			}
			pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}

			repair, err := (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false))

			if test.shouldRefuse {
				if !errors.Is(err, errRejectedCommitSummary) || repair.HeadAdvanced || gitCmd(t, workDir, "rev-parse", "HEAD") != head {
					t.Fatalf("rejected summary reached commit: repair=%+v, error=%v", repair, err)
				}
				if staged := gitCmd(t, workDir, "show", ":staged.txt"); staged != "completed repair" {
					t.Fatalf("rejection lost staged source: %q", staged)
				}
			} else if err != nil || !repair.HeadAdvanced || lastCommitMessage(t, workDir) != "no-mistakes(ci): "+test.wantSummary {
				t.Fatalf("ordinary summary or parse fallback changed: repair=%+v, error=%v", repair, err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want one repair turn", len(ag.calls))
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			recorded, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(recorded), rawText+"\n"+string(test.output)+"\n") {
				t.Fatalf("raw CI repair result was not retained exactly: %v", err)
			}
			for _, name := range []string{"feature.txt", "staged.txt", "untracked.txt"} {
				content, err := os.ReadFile(filepath.Join(workDir, name))
				if err != nil || string(content) != "completed repair\n" {
					t.Fatalf("repair source %s lost: %v", name, err)
				}
			}
			recordedRun, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || recordedRun.HeadSHA != sctx.Run.HeadSHA || recordedRun.LastPushedSHA != nil || test.shouldRefuse && recordedRun.HeadSHA != head {
				t.Fatalf("repair changed recorded custody or publication: run=%+v, error=%v", recordedRun, err)
			}
		})
	}
}
