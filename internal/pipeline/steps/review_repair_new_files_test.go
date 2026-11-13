package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewRepairRun is one Review fix round: the worktree, the run's evidence
// directory, the fixer's RunOpts, the scratch directory its prompt named and
// what the fixer saw of it, and the step log.
type reviewRepairRun struct {
	dir              string
	evidenceDir      string
	fixOpts          agent.RunOpts
	scratch          string
	isScratchPresent bool
	headBeforeRepair string
	logs             []string
}

// scratchInPrompt reads the scratch directory the repair prompt names for
// disposable output.
var scratchInPrompt = regexp.MustCompile(`logs, in (.+), never in the checkout\.`)

// runReviewRepair drives one Review fix round. Each of untrackedBefore exists
// in the worktree before the round starts; the fixer writes each of created,
// runs `git add` on each of staged, and returns fixOutput.
func runReviewRepair(t *testing.T, untrackedBefore, created, staged []string, fixOutput string) reviewRepairRun {
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
				if match := scratchInPrompt.FindStringSubmatch(opts.Prompt); match != nil {
					run.scratch = match[1]
					info, err := os.Stat(run.scratch)
					run.isScratchPresent = err == nil && info.IsDir()
					writeTestFile(t, filepath.Join(run.scratch, "basetemp", "out.txt"), "disposable\n")
				}
				for _, file := range created {
					writeTestFile(t, filepath.Join(dir, filepath.FromSlash(file)), "written by the repair\n")
				}
				for _, file := range staged {
					gitCmd(t, dir, "add", "--", file)
				}
				return &agent.Result{Output: json.RawMessage(fixOutput)}, nil
			}
			findings := Findings{Items: []Finding{}, Summary: "all clear", RiskLevel: "low", RiskRationale: "all clear", RiskScope: types.FindingsRiskScopeSourceOrExternal, ReviewedPaths: fullReviewCoverage(t, dir, baseSHA)}
			j, _ := json.Marshal(findings)
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	run.evidenceDir = sctx.EvidenceDir
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

// filesUnder lists the regular files below root by slash path.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if entry.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Issue #114: a Review repair pointed pytest's basetemp into the checkout and
// its commit carried 20 test artifacts. Only the new files the repair names
// in new_files enter its commit, whether it staged them or not; every other
// new file is moved out of the checkout, kept whole in the run's evidence
// directory, and the step log names where, so the steps after Review check
// exactly the committed tree.
func TestReviewRepairCommitsOnlyTheNewFilesItNames(t *testing.T) {
	t.Parallel()
	artifacts := []string{".pytest-mirror-review/checkout-1/out.txt", ".pytest-mirror-review/checkout-1/gitconfig"}
	for _, tc := range []struct {
		name            string
		untrackedBefore []string
		created         []string
		staged          []string
		fixOutput       string
		wantAdded       []string
		wantMoved       []string
	}{
		{
			name:      "unnamed_artifacts_are_moved_out",
			created:   append([]string{"review-fix.txt"}, artifacts...),
			fixOutput: `{"summary":"address findings","new_files":["review-fix.txt"]}`,
			wantAdded: []string{"review-fix.txt"},
			wantMoved: artifacts,
		},
		{
			name:      "staged_unnamed_artifacts_are_unstaged_and_moved_out",
			created:   append([]string{"review-fix.txt"}, artifacts...),
			staged:    append([]string{"review-fix.txt"}, artifacts...),
			fixOutput: `{"summary":"address findings","new_files":["review-fix.txt"]}`,
			wantAdded: []string{"review-fix.txt"},
			wantMoved: artifacts,
		},
		{
			name:      "a_named_path_is_matched_however_it_is_spelled",
			created:   []string{"tests/new_test.go"},
			fixOutput: `{"summary":"address findings","new_files":["./tests/new_test.go"]}`,
			wantAdded: []string{"tests/new_test.go"},
		},
		{
			name:            "a_file_held_before_the_round_stays_out_even_when_named",
			untrackedBefore: []string{"tests/held_test.go"},
			created:         []string{"review-fix.txt"},
			fixOutput:       `{"summary":"address findings","new_files":["review-fix.txt","tests/held_test.go"]}`,
			wantAdded:       []string{"review-fix.txt"},
		},
		{
			name:      "omitted_new_files_names_nothing",
			created:   []string{"review-fix.txt"},
			fixOutput: `{"summary":"address findings"}`,
			wantMoved: []string{"review-fix.txt"},
		},
		{
			name:      "unreadable_new_files_names_nothing",
			created:   []string{"review-fix.txt"},
			fixOutput: `{"summary":"address findings","new_files":"review-fix.txt"}`,
			wantMoved: []string{"review-fix.txt"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runReviewRepair(t, tc.untrackedBefore, tc.created, tc.staged, tc.fixOutput)

			if added := filesAddedByRepair(t, run); !slices.Equal(added, tc.wantAdded) {
				t.Errorf("repair commit added %q, want %q", added, tc.wantAdded)
			}
			for _, file := range tc.untrackedBefore {
				if _, err := os.Stat(filepath.Join(run.dir, filepath.FromSlash(file))); err != nil {
					t.Errorf("%s, held before the round, must stay in the worktree: %v", file, err)
				}
			}
			status := gitCmd(t, run.dir, "-c", "core.quotePath=false", "status", "--porcelain", "--untracked-files=all")
			var wantStatus []string
			for _, file := range tc.untrackedBefore {
				wantStatus = append(wantStatus, "?? "+file)
			}
			if got := strings.Join(wantStatus, "\n"); status != got {
				t.Errorf("worktree after the round =\n%s\nwant only what it held before:\n%s", status, got)
			}

			const movedLine = "moved the new files the repair did not name in new_files out of the checkout to "
			line := ""
			for _, logged := range run.logs {
				if strings.Contains(logged, movedLine) {
					line = logged
				}
			}
			if len(tc.wantMoved) == 0 {
				if line != "" {
					t.Errorf("step log reports a file moved out, want none: %s", line)
				}
				return
			}
			if line == "" {
				t.Fatalf("no step log line %q:\n%s", movedLine, strings.Join(run.logs, "\n"))
			}
			destination, _, ok := strings.Cut(strings.SplitN(line, movedLine, 2)[1], fmt.Sprintf(" (%d): ", len(tc.wantMoved)))
			if !ok {
				t.Fatalf("step log line %q does not count %d files", line, len(tc.wantMoved))
			}
			if relative, err := filepath.Rel(run.evidenceDir, destination); err != nil || !filepath.IsLocal(relative) {
				t.Errorf("destination %q is not inside the run's evidence directory %q", destination, run.evidenceDir)
			}
			if moved := filesUnder(t, destination); !slices.Equal(moved, slices.Sorted(slices.Values(tc.wantMoved))) {
				t.Errorf("destination %s holds %q, want %q", destination, moved, tc.wantMoved)
			}
			for _, file := range tc.wantMoved {
				if !strings.Contains(line, file) {
					t.Errorf("step log line %q does not name %s", line, file)
				}
				content, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(file)))
				if err != nil || string(content) != "written by the repair\n" {
					t.Errorf("%s was not kept whole at the destination: %q, %v", file, content, err)
				}
			}
		})
	}
}

// What a Review repair moves out of the checkout is kept in the run's
// evidence directory but is not evidence: neither the evidence branch nor
// GitLab's image uploads may ever carry it.
func TestReviewRepairSetAsideIsNeverPublished(t *testing.T) {
	t.Parallel()
	run := runReviewRepair(t, nil, []string{"review-fix.txt", "playwright-report/shot.png"}, nil, `{"summary":"address findings","new_files":["review-fix.txt"]}`)
	kept := filesUnder(t, run.evidenceDir)
	if !slices.ContainsFunc(kept, func(file string) bool { return strings.HasSuffix(file, "playwright-report/shot.png") }) {
		t.Fatalf("the repair set nothing aside in %s: %q", run.evidenceDir, kept)
	}

	images, _, err := gitlabEvidenceImages(run.evidenceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 0 {
		t.Errorf("GitLab image uploads would carry the set-aside files: %+v", images)
	}

	sctx, remote := newEvidencePublishContext(t, "feature/add-login")
	files := map[string]string{"cli-run.txt": "it works\n"}
	for _, file := range kept {
		files[file] = "set aside by the repair\n"
	}
	writeRunEvidence(t, sctx, files)
	if links := publishRunEvidence(sctx); links == nil {
		t.Fatal("the real test evidence beside the set-aside files must still publish")
	}
	tree := gitCmd(t, remote, "ls-tree", "-r", "--name-only", gitCmd(t, remote, "rev-parse", "refs/heads/no-mistakes/evidence"))
	if !strings.Contains(tree, "cli-run.txt") {
		t.Fatalf("the real test evidence did not publish:\n%s", tree)
	}
	if strings.Contains(tree, "shot.png") {
		t.Errorf("the set-aside files reached the evidence branch:\n%s", tree)
	}
}

// The repair prompt names an owned scratch directory outside the checkout for
// disposable output, beside the new_files rule, and the directory is gone once
// the round ends. The turn's environment does not point TMP, TEMP, TMPDIR or
// GOTMPDIR at it: a server-backed adapter reuses its first turn's environment,
// so a later turn would inherit the deleted directory.
func TestReviewRepairNamesAnOwnedScratchDirectory(t *testing.T) {
	t.Parallel()
	run := runReviewRepair(t, nil, []string{"review-fix.txt"}, nil, `{"summary":"address findings","new_files":["review-fix.txt"]}`)

	if run.scratch == "" || !run.isScratchPresent {
		t.Fatalf("repair prompt names scratch %q (present %v), want an existing owned scratch directory:\n%s", run.scratch, run.isScratchPresent, run.fixOpts.Prompt)
	}
	if relative, err := filepath.Rel(run.dir, run.scratch); err == nil && filepath.IsLocal(relative) {
		t.Errorf("scratch %q lies inside the checkout %q", run.scratch, run.dir)
	}
	for _, key := range []string{"TMP", "TEMP", "TMPDIR", "GOTMPDIR"} {
		if value, ok := envValue(run.fixOpts.Env, key); ok && value == run.scratch {
			t.Errorf("repair env %s names the scratch %q", key, value)
		}
	}
	normalized := strings.Join(strings.Fields(run.fixOpts.Prompt), " ")
	for _, want := range []string{`List in "new_files" every file this fix adds`, "delete every file you created in the checkout that is not part of the fix"} {
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
	if _, err := os.Stat(run.scratch); !os.IsNotExist(err) {
		t.Errorf("scratch %q outlived the round: %v", run.scratch, err)
	}
}
