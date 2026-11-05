package eval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type scriptedReviewer struct {
	output  string
	prompts []string
}

func (*scriptedReviewer) Name() string { return "claude" }
func (*scriptedReviewer) Close() error { return nil }
func (r *scriptedReviewer) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	r.prompts = append(r.prompts, opts.Prompt)
	return &agent.Result{Output: []byte(r.output)}, nil
}

// A captured repair round must reach the candidate as a live one would: f-1 was
// selected a round earlier and is still unverified, so a finding reported from
// another file may be that defect and stays scored instead of leaving as a
// follow-up, which scoring never reads.
func TestReplayOfARepairRoundKeepsAnEarlierSelectionAwaitingVerification(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	commitAll := func(message string) string {
		t.Helper()
		for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-m", message}} {
			if _, err := git.Run(ctx, work, args...); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
		head, err := git.HeadSHA(ctx, work)
		if err != nil {
			t.Fatal(err)
		}
		return head
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git.Run(ctx, work, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	write("README.md", "base\n")
	base := commitAll("base")
	if _, err := git.Run(ctx, work, "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	write("feature.txt", "feature one\n")
	write("feature2.txt", "feature two\n")
	write("feature3.txt", "feature three\n")
	start := commitAll("feature")
	write("feature2.txt", "second fix\n")
	head := commitAll("repair f-2")

	const firstBug = `{"id":"f-1","severity":"error","file":"feature.txt","line":1,"description":"original bug","action":"auto-fix"}`
	const secondBug = `{"id":"f-2","severity":"error","file":"feature2.txt","line":1,"description":"second bug","action":"auto-fix"}`
	const record = `"summary":"s","risk_level":"medium","risk_rationale":"r","risk_scope":"source-or-external"`
	const paths3 = `"reviewed_paths":["feature.txt","feature2.txt","feature3.txt"]`
	roundOne := `{"findings":[` + firstBug + `],` + record + `,` + paths3 + `}`
	roundTwo := `{"findings":[` + firstBug + `,` + secondBug + `],` + record + `,` + paths3 + `}`
	onlySecond := `{"findings":[` + secondBug + `],` + record + `}`
	firstID, secondID := `["f-1"]`, `["f-2"]`
	root := t.TempDir()
	original := filepath.Join(root, "original")
	if err := os.MkdirAll(original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(original, "round.json"), sourceRound{Round: 3, Trigger: "user_fix"}); err != nil {
		t.Fatal(err)
	}
	rounds := []sourceRound{
		{Round: 1, Trigger: "initial", FindingsJSON: &roundOne, SelectedFindingIDs: &firstID},
		{Round: 2, Trigger: "auto_fix", FindingsJSON: &roundTwo, UserFindingsJSON: &onlySecond, SelectedFindingIDs: &secondID},
	}
	if err := writeJSON(filepath.Join(original, "rounds.json"), rounds); err != nil {
		t.Fatal(err)
	}
	c := Case{Dir: root}
	c.DefaultBranch = "main"
	c.Branch = "feature"
	c.ReviewedHeadSHA = head
	c.BaseSHA = base
	reviewer := &scriptedReviewer{output: `{"findings":[{"id":"r-1","severity":"error","file":"feature3.txt","line":1,"description":"the same defect reaches this caller","action":"auto-fix"}],"closed_findings":["f-2"],` + record + `,"reviewed_paths":["feature.txt","feature2.txt"]}`}

	database, stepResultID, fixing, previous, deferred, reviewed, pending, err := replayRoundContext(paths.WithRoot(t.TempDir()), c, work)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	outcome, err := (&steps.ReviewStep{}).Execute(&pipeline.StepContext{
		Ctx:                    ctx,
		Run:                    &db.Run{ID: "eval-pending", Branch: c.Branch, HeadSHA: head, BaseSHA: base},
		Repo:                   &db.Repo{ID: "eval", WorkingPath: work, DefaultBranch: "main"},
		WorkDir:                work,
		Agent:                  reviewer,
		Config:                 &config.Config{Agent: types.AgentClaude},
		DB:                     database,
		StepResultID:           stepResultID,
		Fixing:                 fixing,
		SkipFixExecution:       fixing,
		EvalReplay:             true,
		ReviewStartingHeadSHA:  start,
		PreviousFindings:       previous,
		DeferredFindings:       deferred,
		PreviousReviewedPaths:  reviewed,
		PendingVerificationIDs: pending,
		Log:                    func(string) {},
		LogChunk:               func(string) {},
		LogFile:                func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(pending, []string{"f-1", "f-2"}) {
		t.Fatalf("pending = %v, want both selections still outstanding", pending)
	}
	if !strings.Contains(reviewer.prompts[0], "f-1: feature.txt:1: original bug") {
		t.Fatalf("the replayed round must be asked about the earlier selection:\n%s", reviewer.prompts[0])
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].File != "feature3.txt" || len(parsed.FollowUps) != 0 {
		t.Fatalf("items = %+v, follow-ups = %+v, want the finding scored", parsed.Items, parsed.FollowUps)
	}
}
