package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewExecutor_FixSummaryPreservesSourceAndCustody(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		summary          string
		output           json.RawMessage
		commit           config.Commit
		shouldRefuse     bool
		shouldFailCommit bool
		hasAgentCommit   bool
		hasNoOutput      bool
		namesNoNewFiles  bool
		priorFixes       int
	}{
		{name: "normal", summary: "repair source"},
		{name: "ascii at input limit", summary: strings.Repeat("a", 4096)},
		{name: "multibyte at input limit", summary: strings.Repeat("界", 1365)},
		{name: "ascii over input limit", summary: strings.Repeat("a", 4097), shouldRefuse: true},
		{name: "multibyte over input limit", summary: strings.Repeat("界", 1366), shouldRefuse: true},
		{name: "empty", summary: ""},
		{name: "missing output", hasNoOutput: true, namesNoNewFiles: true},
		{name: "missing summary", output: json.RawMessage(`{"new_files":["staged.txt","untracked.txt"]}`)},
		{name: "invalid json", output: json.RawMessage(`{invalid`), namesNoNewFiles: true},
		{name: "invalid utf8", output: json.RawMessage("{\"summary\":\"\xff\"}"), shouldRefuse: true},
		{name: "unsafe suffix", summary: strings.Repeat("a", 73) + "\u202e", shouldRefuse: true},
		{name: "unrelated template expansion", summary: "repair source", shouldRefuse: true, commit: config.Commit{
			FixMessage: strings.Repeat("{{.Branch}}", 15) + "{{.Summary}}", BranchPattern: "(feature)", BranchReplacement: strings.Repeat("b", 300) + "${1}",
		}},
		{name: "commit refusal after prior repair", summary: "repair source", shouldRefuse: true, shouldFailCommit: true, priorFixes: 1},
		{name: "summary refusal after agent commit", summary: strings.Repeat("a", 4097), shouldRefuse: true, hasAgentCommit: true},
		{name: "render refusal after agent commit", summary: "repair source", shouldRefuse: true, hasAgentCommit: true, commit: config.Commit{
			FixMessage: strings.Repeat("{{.Branch}}", 15) + "{{.Summary}}", BranchPattern: "(feature)", BranchReplacement: strings.Repeat("b", 300) + "${1}",
		}},
		{name: "commit refusal after conforming agent commit", summary: "repair source", shouldRefuse: true, shouldFailCommit: true, hasAgentCommit: true},
		// Issue #114: an unnamed new file the repair staged is unstaged only by
		// staging, so a round refused when its message renders keeps it staged.
		{name: "render refusal keeps an unnamed staged file staged", summary: "repair source", shouldRefuse: true, namesNoNewFiles: true, commit: config.Commit{
			FixMessage: strings.Repeat("{{.Branch}}", 15) + "{{.Summary}}", BranchPattern: "(feature)", BranchReplacement: strings.Repeat("b", 300) + "${1}",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workDir, base, head := setupGitRepo(t)
			output := test.output
			if output == nil && !test.hasNoOutput {
				newFiles := []string{"staged.txt", "untracked.txt"}
				if test.namesNoNewFiles {
					newFiles = nil
				}
				var err error
				output, err = json.Marshal(struct {
					Summary  string   `json:"summary"`
					NewFiles []string `json:"new_files"`
				}{Summary: test.summary, NewFiles: newFiles})
				if err != nil {
					t.Fatal(err)
				}
			}
			fixes := 0
			ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if opts.Purpose == "review-fix" {
					fixes++
					if test.hasAgentCommit {
						if err := os.WriteFile(filepath.Join(opts.CWD, "agent-committed.txt"), []byte("committed repair\n"), 0o600); err != nil {
							return nil, err
						}
						gitCmd(t, opts.CWD, "add", "agent-committed.txt")
						gitCmd(t, opts.CWD, "commit", "-m", "agent repair")
					}
					for _, name := range []string{"feature.txt", "staged.txt", "untracked.txt"} {
						if err := os.WriteFile(filepath.Join(opts.CWD, name), []byte("completed repair\n"), 0o600); err != nil {
							return nil, err
						}
					}
					gitCmd(t, opts.CWD, "add", "staged.txt")
					if fixes <= test.priorFixes {
						return &agent.Result{Output: json.RawMessage(`{"summary":"first repair","new_files":["staged.txt","untracked.txt"]}`)}, nil
					}
					if test.shouldFailCommit {
						if err := os.WriteFile(filepath.Join(opts.CWD, "untracked.txt"), []byte("completed second repair\n"), 0o600); err != nil {
							return nil, err
						}
						if test.hasAgentCommit {
							gitCmd(t, opts.CWD, "config", "commit.cleanup", "invalid")
						} else if err := os.WriteFile(filepath.Join(opts.CWD, ".git", "HEAD.lock"), nil, 0o600); err != nil {
							return nil, err
						}
					}
					return &agent.Result{Output: output, Text: "raw fixer response"}, nil
				}
				findings := cleanReviewFindings()
				findings.ReviewedPaths = fullReviewCoverage(t, opts.CWD, base)
				if fixes <= test.priorFixes {
					findings.Items = []Finding{{ID: "repair-1", Severity: "error", File: "feature.txt", Description: "repair the source", Action: "auto-fix", ReviewScope: types.FindingReviewScopeSource}}
				}
				encoded, err := json.Marshal(findings)
				return &agent.Result{Output: encoded}, err
			}}
			sctx := newTestContext(t, ag, workDir, base, head, config.Commands{})
			var err error
			sctx.Repo, err = sctx.DB.InsertRepo(workDir, "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			sctx.Run, err = sctx.DB.InsertRun(sctx.Repo.ID, "feature", head, base)
			if err != nil {
				t.Fatal(err)
			}
			sctx.Config.AutoFix.Review = test.priorFixes + 1
			sctx.Config.Commit = test.commit
			callerDir := ""
			if test.hasAgentCommit {
				callerDir = t.TempDir()
				gitCmd(t, callerDir, "clone", "--branch", "feature", workDir, ".")
				if err := sctx.DB.SetRunWorktreeDir(sctx.Run.ID, workDir); err != nil {
					t.Fatal(err)
				}
			}
			appPaths := paths.WithRoot(t.TempDir())
			executor := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, []pipeline.Step{&ReviewStep{}}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err = executor.Execute(ctx, sctx.Run, sctx.Repo, workDir)

			if (err != nil) != test.shouldRefuse {
				t.Fatalf("Execute error = %v, should refuse = %v", err, test.shouldRefuse)
			}
			if test.hasAgentCommit && test.shouldFailCommit && !strings.Contains(err.Error(), "commit review changes") {
				t.Fatalf("Execute error = %v, want refusal after conforming the agent commit", err)
			}
			if fixes != test.priorFixes+1 {
				t.Fatalf("fix calls = %d, want %d", fixes, test.priorFixes+1)
			}
			snapshot, err := sctx.DB.ReadContinuationSnapshot(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Run.ID != sctx.Run.ID || len(snapshot.Steps) != 1 || len(snapshot.Rounds) != 1 {
				t.Fatal("repair changed run identity or step history")
			}
			runs, err := sctx.DB.GetRunsByRepo(sctx.Repo.ID)
			if err != nil || len(runs) != 1 {
				t.Fatalf("repair created another run: count=%d, error=%v", len(runs), err)
			}
			for _, round := range snapshot.Rounds[0] {
				if round.SelectedFindingIDs != nil {
					if *round.SelectedFindingIDs != `["repair-1"]` || round.SelectionSource == nil || *round.SelectionSource != db.RoundSelectionSourceAutoFix {
						t.Fatal("original finding selection was changed")
					}
				}
			}
			stats, err := sctx.DB.StepRoundStats(snapshot.Steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			spent := stats.FixRounds
			if stats.PendingFixSource != "" {
				spent++
			}
			if spent != test.priorFixes+1 {
				t.Fatalf("spent repair attempts = %d, want %d", spent, test.priorFixes+1)
			}
			log, err := os.ReadFile(filepath.Join(appPaths.RunLogDir(sctx.Run.ID), "review.log"))
			if err != nil || !strings.Contains(string(log), string(output)) || !strings.Contains(string(log), "raw fixer response") {
				t.Errorf("step log lost the raw structured result (%d bytes): %v", len(output), err)
			}
			if test.shouldRefuse {
				if snapshot.Run.Status != types.RunFailed || snapshot.Run.TerminalHeadVerifiedAt != nil || snapshot.Run.CustodyReturnedAt != nil {
					t.Errorf("dirty refusal claimed verified or returned custody: status=%s, verified=%v, returned=%v", snapshot.Run.Status, snapshot.Run.TerminalHeadVerifiedAt, snapshot.Run.CustodyReturnedAt)
				}
				if snapshot.Run.TerminalSourceState == nil || !strings.HasPrefix(*snapshot.Run.TerminalSourceState, "dirty:") {
					t.Error("dirty repair source has no durable observation")
				}
				if snapshot.Steps[0].FindingsJSON == nil || !strings.Contains(*snapshot.Steps[0].FindingsJSON, "repair the source") {
					t.Error("refusal lost original findings")
				}
				service := &branchsync.Service{DB: sctx.DB, Paths: appPaths, Repo: sctx.Repo, WorkDir: workDir, GateDir: filepath.Join(workDir, ".git")}
				if state := service.InspectCached(ctx); state.State != branchsync.StatePipelineOwned || state.Pipeline.RunID != sctx.Run.ID {
					t.Errorf("dirty repair custody = %s, run=%s, safety=%s, error=%s", state.State, state.Pipeline.RunID, state.Safety, state.Error)
				}
				for _, name := range []string{"feature.txt", "staged.txt", "untracked.txt"} {
					content, err := os.ReadFile(filepath.Join(workDir, name))
					want := "completed repair\n"
					if test.shouldFailCommit && name == "untracked.txt" {
						want = "completed second repair\n"
					}
					if err != nil || string(content) != want {
						t.Errorf("repair source %s lost: %v", name, err)
					}
				}
				if staged := gitCmd(t, workDir, "show", ":staged.txt"); staged != "completed repair" {
					t.Errorf("staged source lost: %q", staged)
				}
				if test.hasAgentCommit {
					preserved := gitCmd(t, workDir, "rev-parse", "HEAD")
					if preserved == head || snapshot.Run.HeadSHA != preserved || sctx.Run.HeadSHA != preserved {
						t.Fatalf("terminal head = %s, in-memory head = %s, want preserved descendant %s", snapshot.Run.HeadSHA, sctx.Run.HeadSHA, preserved)
					}
					if anchored := gitCmd(t, workDir, "rev-parse", custody.RecoveryRef(sctx.Run.ID)); anchored != preserved {
						t.Fatalf("recovery anchor = %s, want %s", anchored, preserved)
					}
					if snapshot.Run.TerminalSourceState == nil || *snapshot.Run.TerminalSourceState != "dirty:"+preserved || gitCmd(t, workDir, "show", "HEAD:agent-committed.txt") != "committed repair" {
						t.Fatal("terminal source observation lost the committed repair")
					}
					if test.shouldFailCommit {
						if got := lastCommitMessage(t, workDir); got != "no-mistakes(review): repair source" {
							t.Fatalf("agent commit was not conformed before refusal: %q", got)
						}
						gitCmd(t, workDir, "config", "--unset", "commit.cleanup")
					}
					service.WorkDir = callerDir
					if _, err := service.TerminalSource(ctx, &snapshot.Run); err == nil {
						t.Fatal("dirty source was accepted for continuation")
					}
					gitCmd(t, workDir, "add", "-A")
					gitCmd(t, workDir, "commit", "-m", "preserve remaining repair")
					source, err := service.TerminalSource(ctx, &snapshot.Run)
					if err != nil {
						t.Fatalf("committed repair cannot continue: %v", err)
					}
					if source.RecoveryHead != preserved || source.Head != gitCmd(t, workDir, "rev-parse", "HEAD") {
						t.Fatalf("continuation lost preserved repair: %+v", source)
					}
				}
				return
			}
			if snapshot.Run.Status != types.RunCompleted || gitCmd(t, workDir, "status", "--porcelain") != "" {
				t.Fatal("bounded summary did not commit all repair source")
			}
			committed := []string{"feature.txt", "staged.txt", "untracked.txt"}
			if test.namesNoNewFiles {
				committed = committed[:1]
				if tree := strings.Fields(gitCmd(t, workDir, "ls-tree", "--name-only", "HEAD")); slices.Contains(tree, "staged.txt") || slices.Contains(tree, "untracked.txt") {
					t.Errorf("a repair that named no new file committed one: %v", tree)
				}
			}
			for _, name := range committed {
				if got := gitCmd(t, workDir, "show", "HEAD:"+name); got != "completed repair" {
					t.Errorf("committed source %s = %q", name, got)
				}
			}
			subject := lastCommitMessage(t, workDir)
			if !utf8.ValidString(subject) || len(subject) > 4096 {
				t.Fatal("commit subject exceeded its byte or UTF-8 contract")
			}
			if test.summary == "repair source" && subject != "no-mistakes(review): repair source" {
				t.Errorf("ordinary summary changed: %q", subject)
			}
			if len(test.summary) > 4000 && utf8.RuneCountInString(subject) > 100 {
				t.Error("generated summary was not bounded for a commit subject")
			}
		})
	}
}

func TestRepairResult_RenderRefusalPreservesRawOutput(t *testing.T) {
	t.Parallel()
	for _, step := range []types.StepName{types.StepLint, types.StepCI} {
		t.Run(string(step), func(t *testing.T) {
			t.Parallel()
			workDir, base, head := setupGitRepo(t)
			output := json.RawMessage(`{"findings":[],"summary":"repair source","code_change_needed":true}`)
			const rawText = "raw structured repair explanation"
			ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("completed repair\n"), 0o600); err != nil {
					return nil, err
				}
				return &agent.Result{Output: output, Text: rawText}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, workDir, base, head, config.Commands{})
			sctx.Config.Commit = config.Commit{
				FixMessage: strings.Repeat("{{.Branch}}", 15) + "{{.Summary}}", BranchPattern: "(feature)", BranchReplacement: strings.Repeat("b", 300) + "${1}",
			}
			logPath := filepath.Join(t.TempDir(), string(step)+".log")
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
			var visibleLog strings.Builder
			sctx.Log = func(text string) { visibleLog.WriteString(text + "\n") }

			if step == types.StepLint {
				_, err = (&LintStep{}).Execute(sctx)
			} else {
				pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
				_, err = (&CIStep{}).autoFixCI(sctx, &forgejoLogTestHost{}, pr, ciTargetsFor([]string{"test"}, false))
			}

			if err == nil || !strings.Contains(err.Error(), "must not render to more than 4096 bytes") {
				t.Fatalf("repair error = %v, want rendered-message refusal", err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want one repair turn", len(ag.calls))
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			recorded, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(recorded), string(output)) || !strings.Contains(string(recorded), rawText) {
				t.Fatalf("file-only log lost the raw repair result: %q, error=%v", recorded, err)
			}
			if strings.Contains(visibleLog.String(), string(output)) || strings.Contains(visibleLog.String(), rawText) {
				t.Fatal("raw repair result escaped the file-only log")
			}
			content, err := os.ReadFile(filepath.Join(workDir, "feature.txt"))
			if err != nil || string(content) != "completed repair\n" || gitCmd(t, workDir, "rev-parse", "HEAD") != head {
				t.Fatalf("rendering refusal lost completed source or moved HEAD: %v", err)
			}
		})
	}
}
