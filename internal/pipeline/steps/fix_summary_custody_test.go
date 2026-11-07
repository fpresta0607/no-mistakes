package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
		hasNoOutput      bool
		priorFixes       int
	}{
		{name: "normal", summary: "repair source"},
		{name: "ascii at input limit", summary: strings.Repeat("a", 4096)},
		{name: "multibyte at input limit", summary: strings.Repeat("界", 1365)},
		{name: "ascii over input limit", summary: strings.Repeat("a", 4097), shouldRefuse: true},
		{name: "multibyte over input limit", summary: strings.Repeat("界", 1366), shouldRefuse: true},
		{name: "empty", summary: ""},
		{name: "missing output", hasNoOutput: true},
		{name: "missing summary", output: json.RawMessage(`{}`)},
		{name: "invalid json", output: json.RawMessage(`{invalid`)},
		{name: "invalid utf8", output: json.RawMessage("{\"summary\":\"\xff\"}"), shouldRefuse: true},
		{name: "unsafe suffix", summary: strings.Repeat("a", 73) + "\u202e", shouldRefuse: true},
		{name: "unrelated template expansion", summary: "repair source", shouldRefuse: true, commit: config.Commit{
			FixMessage: strings.Repeat("{{.Branch}}", 15) + "{{.Summary}}", BranchPattern: "(feature)", BranchReplacement: strings.Repeat("b", 300) + "${1}",
		}},
		{name: "commit refusal after prior repair", summary: "repair source", shouldRefuse: true, shouldFailCommit: true, priorFixes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workDir, base, head := setupGitRepo(t)
			output := test.output
			if output == nil && !test.hasNoOutput {
				var err error
				output, err = json.Marshal(commitSummary{Summary: test.summary})
				if err != nil {
					t.Fatal(err)
				}
			}
			fixes := 0
			ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if opts.Purpose == "review-fix" {
					fixes++
					for _, name := range []string{"feature.txt", "staged.txt", "untracked.txt"} {
						if err := os.WriteFile(filepath.Join(opts.CWD, name), []byte("completed repair\n"), 0o600); err != nil {
							return nil, err
						}
					}
					gitCmd(t, opts.CWD, "add", "staged.txt")
					if fixes <= test.priorFixes {
						return &agent.Result{Output: json.RawMessage(`{"summary":"first repair"}`)}, nil
					}
					if test.shouldFailCommit {
						if err := os.WriteFile(filepath.Join(opts.CWD, "untracked.txt"), []byte("completed second repair\n"), 0o600); err != nil {
							return nil, err
						}
						if err := os.WriteFile(filepath.Join(opts.CWD, ".git", "HEAD.lock"), nil, 0o600); err != nil {
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
			appPaths := paths.WithRoot(t.TempDir())
			executor := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, []pipeline.Step{&ReviewStep{}}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err = executor.Execute(ctx, sctx.Run, sctx.Repo, workDir)

			if (err != nil) != test.shouldRefuse {
				t.Fatalf("Execute error = %v, should refuse = %v", err, test.shouldRefuse)
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
				return
			}
			if snapshot.Run.Status != types.RunCompleted || gitCmd(t, workDir, "status", "--porcelain") != "" {
				t.Fatal("bounded summary did not commit all repair source")
			}
			for _, name := range []string{"feature.txt", "staged.txt", "untracked.txt"} {
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
