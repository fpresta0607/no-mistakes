package citest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

const endAtGreenLog = "The run ends here under ci_end_at_green"

// endAtGreenRun is a pushed run at its CI step: the feature branch is
// published on a local bare origin at the run's head, and the pull request's
// checks go from pending to green.
type endAtGreenRun struct {
	sctx    *pipeline.StepContext
	dir     string
	headSHA string
	logs    []string
	polls   int
	cancel  context.CancelFunc
}

func newEndAtGreenRun(t *testing.T, checks []string, endAtGreen bool) *endAtGreenRun {
	t.Helper()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	env := stepstest.FakeCIGHSequence(t, "OPEN", checks)

	prURL := "https://github.com/test/repo/pull/42"
	sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Minute
	sctx.Config.CIEndAtGreen = endAtGreen

	r := &endAtGreenRun{sctx: sctx, dir: dir, headSHA: headSHA}
	sctx.Log = func(s string) { r.logs = append(r.logs, s) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sctx.Ctx = ctx
	r.cancel = cancel
	return r
}

// execute runs the CI step and lets it poll at most maxPolls times before
// stopping it, as an operator abort would.
func (r *endAtGreenRun) execute(maxPolls int) (*pipeline.StepOutcome, error) {
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		r.polls++
		if r.polls >= maxPolls {
			r.cancel()
			return ctx.Err()
		}
		return nil
	})
	return step.Execute(r.sctx)
}

func (r *endAtGreenRun) logged(text string) bool {
	for _, line := range r.logs {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func (r *endAtGreenRun) setPRHead(sha string) {
	for i, entry := range r.sctx.Env {
		if strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
			r.sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=" + sha
		}
	}
}

var pendingThenGreen = []string{
	`[{"name":"build","state":"PENDING","bucket":"pending"}]`,
	`[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`,
}

// Under ci_end_at_green the run ends at the first green on the head it
// pushed: the step completes, marked ready, and polls no more.
func TestCIStep_EndAtGreenEndsTheRunAtTheFirstGreenOnThePushedHead(t *testing.T) {
	t.Parallel()
	r := newEndAtGreenRun(t, pendingThenGreen, true)

	outcome, err := r.execute(5)
	if err != nil {
		t.Fatalf("Execute() error = %v, want the step completed at green", err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("outcome = %#v, want a plain completed step", outcome)
	}
	if r.polls != 1 {
		t.Fatalf("polls = %d, want one wait for the pending checks and none after green", r.polls)
	}
	if !r.logged(endAtGreenLog) || !r.logged(r.headSHA[:12]) {
		t.Fatalf("logs = %q, want the end at green and the head it was on", r.logs)
	}
	run, err := r.sctx.DB.GetRun(r.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt == nil {
		t.Fatal("the run ended at green without its CI readiness recorded")
	}
}

// With the setting absent the run keeps watching the pull request after
// green, until it is merged, closed or stopped: today's behaviour.
func TestCIStep_EndAtGreenOffKeepsMonitoringAfterGreen(t *testing.T) {
	t.Parallel()
	r := newEndAtGreenRun(t, pendingThenGreen, false)

	_, err := r.execute(3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want monitoring to go on after green until stopped", err)
	}
	if r.polls != 3 {
		t.Fatalf("polls = %d, want monitoring to go on after green", r.polls)
	}
	if !r.logged("all CI checks passed - still monitoring until merged or closed") || r.logged("ci_end_at_green") {
		t.Fatalf("logs = %q, want today's monitoring line and no word of the setting", r.logs)
	}
}

// A green the forge reports for another head than the one this run pushed,
// such as a commit somebody else pushed to the pull request, never ends a run.
func TestCIStep_EndAtGreenKeepsMonitoringAGreenOnAnotherHead(t *testing.T) {
	t.Parallel()
	r := newEndAtGreenRun(t, pendingThenGreen, true)
	other := strings.Repeat("1", 40)
	r.setPRHead(other)

	_, err := r.execute(3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want monitoring to go on", err)
	}
	if r.logged(endAtGreenLog) {
		t.Fatalf("logs = %q, want no end at a green on another head", r.logs)
	}
	if !r.logged("on 111111111111, the pull request's head, not on " + r.headSHA[:12]) {
		t.Fatalf("logs = %q, want the reason monitoring goes on", r.logs)
	}
}

// The pushed branch must still point at the run's head: a branch moved since
// the push keeps the run watching, as today.
func TestCIStep_EndAtGreenKeepsMonitoringWhenThePushedBranchMoved(t *testing.T) {
	t.Parallel()
	r := newEndAtGreenRun(t, pendingThenGreen, true)
	if err := os.WriteFile(filepath.Join(r.dir, "out-of-band.txt"), []byte("out of band"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, r.dir, "add", "-A")
	stepstest.GitCmd(t, r.dir, "commit", "-m", "out of band commit")
	moved := stepstest.GitCmd(t, r.dir, "rev-parse", "HEAD")
	stepstest.GitCmd(t, r.dir, "push", "origin", "feature")
	stepstest.GitCmd(t, r.dir, "reset", "--hard", r.headSHA)
	r.setPRHead(r.headSHA)

	_, err := r.execute(3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want monitoring to go on", err)
	}
	if r.logged(endAtGreenLog) {
		t.Fatalf("logs = %q, want no end while the branch points elsewhere", r.logs)
	}
	if !r.logged("the pushed branch now points at " + moved[:12]) {
		t.Fatalf("logs = %q, want the moved branch named", r.logs)
	}
}

// A repository that declares no CI is ready with no checks, and under the
// setting that ends the run the same way.
func TestCIStep_EndAtGreenEndsADeclaredNoCIRun(t *testing.T) {
	t.Parallel()
	r := newEndAtGreenRun(t, []string{`[]`}, true)
	r.sctx.Config.NoCI = true

	outcome, err := r.execute(5)
	if err != nil {
		t.Fatalf("Execute() error = %v, want the step completed", err)
	}
	if outcome == nil || outcome.NeedsApproval || r.polls != 0 {
		t.Fatalf("outcome = %#v after %d polls, want a completed step at the first observation", outcome, r.polls)
	}
	if !r.logged("the repository declares no CI (no_ci: true) on " + r.headSHA[:12]) {
		t.Fatalf("logs = %q, want the declared no-CI end named", r.logs)
	}
}
