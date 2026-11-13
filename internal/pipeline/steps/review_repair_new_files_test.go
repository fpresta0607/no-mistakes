package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewRepairRun is one Review fix round: the worktree, the fixer's RunOpts,
// what the fixer saw of its scratch directory, and the step log.
type reviewRepairRun struct {
	dir              string
	fixOpts          agent.RunOpts
	isScratchPresent bool
	headBeforeRepair string
	logs             []string
}

// runReviewRepair drives one Review fix round. Each of untrackedBefore exists
// in the worktree before the round starts; the fixer writes each of created
// and returns fixOutput.
func runReviewRepair(t *testing.T, untrackedBefore, created []string, fixOutput string) reviewRepairRun {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	for _, file := range untrackedBefore {
		writeTestFile(t, filepath.Join(dir, filepath.FromSlash(file)), "left before the round\n")
	}
	run := reviewRepairRun{dir: dir, headBeforeRepair: headSHA}
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				run.fixOpts = opts
				if scratch, ok := envValue(opts.Env, "TMPDIR"); ok {
					info, err := os.Stat(scratch)
					run.isScratchPresent = err == nil && info.IsDir()
				}
				for _, file := range created {
					writeTestFile(t, filepath.Join(dir, filepath.FromSlash(file)), "written by the repair\n")
				}
				return &agent.Result{Output: json.RawMessage(fixOutput)}, nil
			}
			findings := Findings{Items: []Finding{}, Summary: "all clear", RiskLevel: "low", RiskRationale: "all clear", RiskScope: types.FindingsRiskScopeSourceOrExternal, ReviewedPaths: fullReviewCoverage(t, dir, baseSHA)}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var mu sync.Mutex
	sctx.Log = func(line string) {
		mu.Lock()
		defer mu.Unlock()
		run.logs = append(run.logs, line)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","description":"possible nil deref"}],"summary":"1 issue"}`

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	return run
}

// filesAddedByRepair lists the files the repair's commits added.
func filesAddedByRepair(t *testing.T, run reviewRepairRun) []string {
	t.Helper()
	added := gitCmd(t, run.dir, "-c", "core.quotePath=false", "diff", "--name-only", "--diff-filter=A", run.headBeforeRepair, "HEAD")
	if added == "" {
		return nil
	}
	return strings.Split(added, "\n")
}

// Issue #114: a Review repair pointed pytest's basetemp into the checkout and
// its commit carried 20 test artifacts. Only the new files the repair names
// in new_files enter its commit; every other file it created stays in the
// worktree, untracked, and is named in the step log.
func TestReviewRepairCommitsOnlyTheNewFilesItNames(t *testing.T) {
	t.Parallel()
	artifacts := []string{".pytest-mirror-review/checkout-1/out.txt", ".pytest-mirror-review/checkout-1/gitconfig"}
	for _, tc := range []struct {
		name            string
		untrackedBefore []string
		created         []string
		fixOutput       string
		wantAdded       []string
		wantLeftOut     []string
	}{
		{
			name:        "unnamed_artifacts_stay_out",
			created:     append([]string{"review-fix.txt"}, artifacts...),
			fixOutput:   `{"summary":"address findings","new_files":["review-fix.txt"]}`,
			wantAdded:   []string{"review-fix.txt"},
			wantLeftOut: artifacts,
		},
		{
			name:        "a_named_path_is_matched_however_it_is_spelled",
			created:     []string{"tests/new_test.go"},
			fixOutput:   `{"summary":"address findings","new_files":["./tests/new_test.go"]}`,
			wantAdded:   []string{"tests/new_test.go"},
			wantLeftOut: nil,
		},
		{
			name:            "a_file_an_earlier_round_left_out_enters_once_named",
			untrackedBefore: []string{"tests/forgotten_test.go"},
			created:         []string{"review-fix.txt"},
			fixOutput:       `{"summary":"address findings","new_files":["review-fix.txt","tests/forgotten_test.go"]}`,
			wantAdded:       []string{"review-fix.txt", "tests/forgotten_test.go"},
			wantLeftOut:     nil,
		},
		{
			name:        "omitted_new_files_names_nothing",
			created:     []string{"review-fix.txt"},
			fixOutput:   `{"summary":"address findings"}`,
			wantAdded:   nil,
			wantLeftOut: []string{"review-fix.txt"},
		},
		{
			name:        "unreadable_new_files_names_nothing",
			created:     []string{"review-fix.txt"},
			fixOutput:   `{"summary":"address findings","new_files":"review-fix.txt"}`,
			wantAdded:   nil,
			wantLeftOut: []string{"review-fix.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runReviewRepair(t, tc.untrackedBefore, tc.created, tc.fixOutput)

			if added := filesAddedByRepair(t, run); !slices.Equal(added, tc.wantAdded) {
				t.Errorf("repair commit added %q, want %q", added, tc.wantAdded)
			}
			for _, file := range tc.wantLeftOut {
				if _, err := os.Stat(filepath.Join(run.dir, filepath.FromSlash(file))); err != nil {
					t.Errorf("%s must stay in the worktree as the repair left it: %v", file, err)
				}
				if tracked := gitCmd(t, run.dir, "ls-files", "--", file); tracked != "" {
					t.Errorf("%s was staged: %q", file, tracked)
				}
			}
			const leftOutLine = "left the new files the repair did not name in new_files out of the commit"
			if len(tc.wantLeftOut) == 0 {
				if logsContain(run.logs, leftOutLine) {
					t.Errorf("step log reports a file left out, want none:\n%s", strings.Join(run.logs, "\n"))
				}
				return
			}
			want := fmt.Sprintf("%s (%d): ", leftOutLine, len(tc.wantLeftOut))
			line := ""
			for _, logged := range run.logs {
				if strings.Contains(logged, want) {
					line = logged
				}
			}
			if line == "" {
				t.Fatalf("no step log line %q:\n%s", want, strings.Join(run.logs, "\n"))
			}
			for _, file := range tc.wantLeftOut {
				if !strings.Contains(line, file) {
					t.Errorf("step log line %q does not name %s", line, file)
				}
			}
		})
	}
}

// The repair turn runs with an owned scratch directory outside the checkout as
// its temporary directory, named in its prompt beside the new_files rule, and
// the directory is gone once the round ends.
func TestReviewRepairRunsInAnOwnedScratchDirectory(t *testing.T) {
	t.Parallel()
	run := runReviewRepair(t, nil, []string{"review-fix.txt"}, `{"summary":"address findings","new_files":["review-fix.txt"]}`)

	scratch, ok := envValue(run.fixOpts.Env, "TMPDIR")
	if !ok || !run.isScratchPresent {
		t.Fatalf("repair env TMPDIR = %q (set %v, present %v), want an existing owned scratch directory", scratch, ok, run.isScratchPresent)
	}
	for _, key := range []string{"TMP", "TEMP", "GOTMPDIR"} {
		if value, _ := envValue(run.fixOpts.Env, key); value != scratch {
			t.Errorf("repair env %s = %q, want the scratch %q", key, value, scratch)
		}
	}
	if relative, err := filepath.Rel(run.dir, scratch); err == nil && !strings.HasPrefix(relative, "..") {
		t.Errorf("scratch %q lies inside the checkout %q", scratch, run.dir)
	}
	normalized := strings.Join(strings.Fields(run.fixOpts.Prompt), " ")
	for _, want := range []string{scratch, `List in "new_files" every file this fix adds`, "delete every file you created in the checkout that is not part of the fix"} {
		if !strings.Contains(normalized, want) {
			t.Errorf("repair prompt does not carry %q:\n%s", want, run.fixOpts.Prompt)
		}
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(run.fixOpts.JSONSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema.Properties["new_files"]; !ok {
		t.Errorf("repair schema does not offer new_files: %s", run.fixOpts.JSONSchema)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch %q outlived the round: %v", scratch, err)
	}
}
