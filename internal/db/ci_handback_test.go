package db

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIHandbackKeepsConflictAndBudgetHistory(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepo("fixture", "https://github.com/example/fixture.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "topic", "published", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		database.UpdateRunStatus(run.ID, types.RunRunning),
		database.UpdateRunPRURL(run.ID, "https://github.com/example/fixture/pull/1"),
		database.UpdateRunPRState(run.ID, "open"),
		database.UpdateRunPublication(run.ID, PushBinding{HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/topic"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","category":"ci-merge-conflict","severity":"error","action":"auto-fix","description":"conflict"}],"summary":"conflict"}`
	first, err := database.InsertStepRound(ci.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepRoundSelection(first.ID, strPtr(`["ci-1"]`), RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	last, err := database.InsertStepRound(ci.ID, 2, "auto_fix", &findings, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, ci.ID, types.StepStatusAwaitingApproval, 0, 30, &findings); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetRoundsByStep(ci.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &CIHandback{RunID: run.ID, RoundID: last.ID, CallerDir: "caller", ReleasedHead: run.HeadSHA, PRURL: *run.PRURL, BaseBranch: "main", BaseHead: "new-base", PushGeneration: *run.PushGeneration, TargetFingerprint: "target", PushRef: "refs/heads/topic"}
	if err := database.ReleaseCIHandback(receipt, run, repo, findings); err != nil {
		t.Fatal(err)
	}
	stored, err := database.LatestCIHandback(run.ID)
	if err != nil || !reflect.DeepEqual(receipt, stored) {
		t.Fatalf("receipt = %#v %v", stored, err)
	}
	if err := database.AcceptCIHandback(receipt, "additive", repo); err == nil {
		t.Fatal("accepted an uncheckpointed follow-up")
	}
	if err := database.PrepareCIHandback(receipt, "additive", "new-base", "HEAD", repo); err != nil {
		t.Fatal(err)
	}
	if err := database.PrepareCIHandback(receipt, "different", "new-base", "HEAD", repo); err == nil {
		t.Fatal("replaced an unfinished candidate checkpoint")
	}
	stored, err = database.LatestCIHandback(run.ID)
	if err != nil || !reflect.DeepEqual(receipt, stored) {
		t.Fatalf("candidate checkpoint = %#v %v", stored, err)
	}
	if _, err := database.ReplaceRepoURLs(repo.ID, "https://github.com/other/fixture.git", ""); err != nil {
		t.Fatal(err)
	}
	if err := database.AcceptCIHandback(receipt, "additive", repo); err == nil {
		t.Fatal("accepted a follow-up after the repository target changed")
	}
	if _, err := database.ReplaceRepoURLs(repo.ID, repo.UpstreamURL, repo.ForkURL); err != nil {
		t.Fatal(err)
	}
	if err := database.AcceptCIHandback(receipt, "additive", repo); err != nil {
		t.Fatal(err)
	}
	if err := database.AcceptCIHandback(receipt, "replay", repo); err == nil {
		t.Fatal("receipt accepted twice")
	}
	if err := database.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	after, err := database.GetRoundsByStep(ci.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rounds changed: %#v %v", after, err)
	}
	updated, err := database.GetRun(run.ID)
	if err != nil || updated.HeadSHA != "additive" || updated.PRURL == nil || *updated.PRURL != *run.PRURL || *updated.LastPushedSHA != "published" || *updated.PushGeneration != *run.PushGeneration || updated.ReviewApprovedHeadSHA != nil || updated.CIReadyAt != nil {
		t.Fatalf("accepted run = %#v %v", updated, err)
	}
	stored, err = database.LatestCIHandback(run.ID)
	if err != nil || stored.AcceptedHead == nil || *stored.AcceptedHead != "additive" || stored.ReleasedHead != "published" {
		t.Fatalf("accepted receipt = %#v %v", stored, err)
	}
}

func TestCIHandbackValidationRejectsEvidenceChangesAtEachWrite(t *testing.T) {
	for _, boundary := range []string{"release", "checkpoint", "acceptance"} {
		t.Run(boundary, func(t *testing.T) {
			database := openTestDB(t)
			repo, err := database.InsertRepo("fixture", "https://github.com/example/fixture.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "topic", "published", "base")
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range []error{
				database.UpdateRunStatus(run.ID, types.RunRunning),
				database.UpdateRunReviewApprovedHeadSHA(run.ID, "reviewed"),
				database.UpdateRunPRURL(run.ID, "https://github.com/example/fixture/pull/1"),
				database.UpdateRunPRState(run.ID, "open"),
				database.UpdateRunPublication(run.ID, PushBinding{HeadSHA: "published", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/topic"}),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			ci, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"ci-1","category":"ci-check","severity":"error","action":"auto-fix","check":"tests"}]}`
			round, err := database.InsertStepRound(ci.ID, 1, "initial", &findings, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, ci.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			validation, err := database.ReadCIHandbackValidation(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt := &CIHandback{RunID: run.ID, RoundID: round.ID, CallerDir: "caller", ReleasedHead: "published", PRURL: *run.PRURL, BaseBranch: "main", BaseHead: "base", PushGeneration: *run.PushGeneration, TargetFingerprint: "target", PushRef: "refs/heads/topic",
				Source: &CIHandbackSource{CallerHead: "published", SubmittedHead: *run.SubmittedHeadSHA, ReviewHead: "reviewed", PushKind: "upstream", PushedAt: *run.LastPushedAt, Findings: findings, AutoFixLimits: map[types.StepName]int{types.StepCI: 0}, Validation: validation}}
			if boundary != "release" {
				if err := database.ReleaseCIHandback(receipt, run, repo, findings); err != nil {
					t.Fatal(err)
				}
				stored, err := database.LatestCIHandback(run.ID)
				if err != nil || !reflect.DeepEqual(receipt, stored) {
					t.Fatalf("durable receipt lost historical evidence: %#v %v", stored, err)
				}
			}
			if boundary == "acceptance" {
				if err := database.PrepareCIHandback(receipt, "correction", "base", "HEAD", repo); err != nil {
					t.Fatal(err)
				}
			}
			selected := `["ci-1"]`
			if err := database.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceAutoFix); err != nil {
				t.Fatal(err)
			}
			switch boundary {
			case "release":
				err = database.ReleaseCIHandback(receipt, run, repo, findings)
			case "checkpoint":
				err = database.PrepareCIHandback(receipt, "correction", "base", "HEAD", repo)
			case "acceptance":
				err = database.AcceptCIHandback(receipt, "correction", repo)
			}
			if err == nil {
				t.Fatal("changed spending was accepted")
			}
			if after, err := database.GetRun(run.ID); err != nil || !reflect.DeepEqual(run, after) {
				t.Fatalf("rejected write changed publication or old review: %#v %v", after, err)
			}
		})
	}
}
