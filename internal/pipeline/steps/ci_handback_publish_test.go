package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublishCIHandbackPreservesAcceptedFollowup(t *testing.T) {
	dir, base, published := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, published, config.Commands{})
	for _, err := range []error{
		sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning),
		sctx.DB.UpdateRunPRURL(sctx.Run.ID, "https://github.com/test/repo/pull/1"),
		sctx.DB.UpdateRunPRState(sctx.Run.ID, "open"),
		sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","category":"ci-merge-conflict"}]}`
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.ParkStepForApproval(sctx.Run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &db.CIHandback{RunID: run.ID, RoundID: round.ID, CallerDir: dir, ReleasedHead: published, PRURL: *run.PRURL, BaseBranch: "main", BaseHead: base, PushGeneration: *run.PushGeneration, TargetFingerprint: "target", PushRef: "refs/heads/feature"}
	if err := sctx.DB.ReleaseCIHandback(receipt, run, sctx.Repo, findings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "followup.txt"), []byte("preserve this follow-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "followup.txt")
	gitCmd(t, dir, "commit", "-m", "resolve conflict")
	accepted := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sctx.DB.PrepareCIHandback(receipt, accepted, base, "feature", sctx.Repo); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.AcceptCIHandback(receipt, accepted, sctx.Repo); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, sctx, published)
	if err := publishRunHead(sctx, published, "", nil); err == nil || !strings.Contains(err.Error(), "accepted CI follow-up") {
		t.Fatalf("publication could discard accepted head %s: %v", accepted, err)
	}
}
