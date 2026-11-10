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

// A captured repair round must reach the candidate as a live one would. A
// selection still unverified makes a finding reported from another file possibly
// that defect, so it stays scored instead of leaving as a follow-up, which
// scoring never reads. A restart at Review cleared the earlier selection, so
// the same report is a follow-up there and the candidate is not asked about it.
func TestReplayOfARepairRoundCarriesWhatALiveRoundCanClear(t *testing.T) {
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
	const allPaths = `"reviewed_paths":["feature.txt","feature2.txt","feature3.txt"]`
	onlyFirst := `{"findings":[` + firstBug + `],` + record + `,` + allPaths + `}`
	both := `{"findings":[` + firstBug + `,` + secondBug + `],` + record + `,` + allPaths + `}`
	verified := `{"findings":[],` + record + `,` + allPaths + `}`
	withNote := `{"findings":[` + secondBug + `,{"id":"note","severity":"info","file":"feature3.txt","line":1,"description":"worth knowing","action":"no-op"}],` + record + `,` + allPaths + `}`
	firstID, secondID := `["f-1"]`, `["f-2"]`

	cases := []struct {
		name         string
		rounds       []sourceRound
		wantPending  []string
		wantFollowUp bool
	}{
		{
			name: "an earlier selection still awaits verification",
			rounds: []sourceRound{
				{Round: 1, Trigger: "initial", FindingsJSON: &onlyFirst, SelectedFindingIDs: &firstID},
				{Round: 2, Trigger: "auto_fix", FindingsJSON: &both, SelectedFindingIDs: &secondID},
			},
			wantPending: []string{"f-1", "f-2"},
		},
		{
			name: "a restart at Review cleared the earlier selection",
			rounds: []sourceRound{
				{Round: 1, Trigger: "initial", FindingsJSON: &onlyFirst, SelectedFindingIDs: &firstID},
				{Round: 2, Trigger: "auto_fix", FindingsJSON: &verified},
				{Round: 3, Trigger: "initial", FindingsJSON: &both, SelectedFindingIDs: &secondID},
			},
			wantPending:  []string{"f-2"},
			wantFollowUp: true,
		},
		{
			name: "an automatic selection leaves an informational note deferred",
			rounds: []sourceRound{
				{Round: 1, Trigger: "initial", FindingsJSON: &withNote, SelectedFindingIDs: &secondID},
			},
			wantPending:  []string{"f-2"},
			wantFollowUp: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			original := filepath.Join(root, "original")
			if err := os.MkdirAll(original, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(original, "round.json"), sourceRound{Round: len(tc.rounds) + 1, Trigger: "auto_fix"}); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(original, "rounds.json"), tc.rounds); err != nil {
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

			if !reflect.DeepEqual(pending, tc.wantPending) {
				t.Fatalf("pending = %v, want %v", pending, tc.wantPending)
			}
			if got := strings.Contains(reviewer.prompts[0], "f-1: feature.txt:1: original bug"); got == tc.wantFollowUp {
				t.Fatalf("asked about the earlier selection = %v, want %v:\n%s", got, !tc.wantFollowUp, reviewer.prompts[0])
			}
			parsed, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(parsed.FollowUps) == 1 && len(parsed.Items) == 0; got != tc.wantFollowUp {
				t.Fatalf("items = %+v, follow-ups = %+v, want follow-up %v", parsed.Items, parsed.FollowUps, tc.wantFollowUp)
			}
		})
	}
}
