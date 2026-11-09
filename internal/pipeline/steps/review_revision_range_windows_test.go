package steps

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func windowsReviewRevisionRepo(t *testing.T) (string, string, string) {
	t.Helper()
	if testGitErr != nil {
		t.Fatal(testGitErr)
	}
	t.Setenv("PATH", filepath.Dir(testGitExecutable)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	source, baseSHA, headSHA := setupGitRepo(t)
	parent := t.TempDir()
	padding := 180 - len(parent) - 1
	if padding < 1 {
		t.Fatalf("temporary parent length %d cannot host a 180-character repository", len(parent))
	}
	dir := filepath.Join(parent, strings.Repeat("r", padding))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyDirContents(source, dir); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "config", "core.longpaths", "false")
	if actual := gitCmd(t, dir, "config", "--get", "core.longpaths"); actual != "false" {
		t.Fatalf("core.longpaths = %q, want false", actual)
	}
	if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if len(path) >= 260 {
			t.Errorf("actual fixture path has length %d: %s", len(path), path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	probeLength := len(filepath.Join(dir, baseSHA+".."+headSHA))
	if probeLength <= 260 {
		t.Fatalf("revision filename probe length = %d, want greater than 260", probeLength)
	}
	t.Logf("real Git=%s; cwd_length=%d; revision_probe_length=%d; core.longpaths=false; base=%s; head=%s", testGitExecutable, len(dir), probeLength, baseSHA, headSHA)
	return dir, baseSHA, headSHA
}

func TestReviewStep_WindowsRevisionRangeLongPath(t *testing.T) {
	dir, baseSHA, headSHA := windowsReviewRevisionRepo(t)
	output, err := json.Marshal(cleanReviewFindings())
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: output}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, reviewer, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ReviewStep{}).Execute(sctx)

	if err != nil {
		t.Fatalf("Execute() error = %v; reviewer_calls=%d", err, len(reviewer.calls))
	}
	if len(reviewer.calls) != 1 || !strings.Contains(reviewer.calls[0].Prompt, "feature.txt") {
		t.Fatalf("reviewer calls = %+v, want one review of feature.txt", reviewer.calls)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.ReviewApprovedHeadSHA != headSHA {
		t.Fatalf("Execute() outcome = %+v, want positively covered head %s", outcome, headSHA)
	}
	if len(outcome.ReviewablePaths) != 1 || outcome.ReviewablePaths[0] != "feature.txt" {
		t.Fatalf("reviewable paths = %v, want [feature.txt]", outcome.ReviewablePaths)
	}
}

func TestReviewStep_WindowsInvalidRevisionRefuses(t *testing.T) {
	dir, baseSHA, _ := windowsReviewRevisionRepo(t)
	for _, testCase := range []struct {
		name string
		head string
	}{
		{name: "invalid syntax", head: "HEAD^{"},
		{name: "missing commit", head: strings.Repeat("1", 40)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reviewer := &mockAgent{name: "test"}
			sctx := newTestContextWithDBRecords(t, reviewer, dir, baseSHA, testCase.head, config.Commands{})

			outcome, err := (&ReviewStep{}).Execute(sctx)

			if err == nil || !strings.Contains(err.Error(), "get changed files:") {
				t.Fatalf("Execute() error = %v, want changed-file revision refusal", err)
			}
			if outcome != nil || len(reviewer.calls) != 0 {
				t.Fatalf("outcome=%+v, reviewer_calls=%d; invalid revision must not reach review", outcome, len(reviewer.calls))
			}
		})
	}
}
