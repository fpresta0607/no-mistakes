package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newPostDocumentCIHandbackFixture(t *testing.T) (*ordinaryCIHandbackFixture, string) {
	t.Helper()
	fixture := newOrdinaryCIHandbackFixture(t)
	reviewed := fixture.request.PublishedHead
	if err := os.WriteFile(filepath.Join(fixture.caller, "README.md"), []byte("Document committed accurate usage.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.caller, "add", "README.md")
	gitCmd(t, fixture.caller, "commit", "-m", "document feature usage")
	published := gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	gitCmd(t, fixture.context.WorkDir, "fetch", "--no-tags", fixture.caller, published)
	gitCmd(t, fixture.context.WorkDir, "merge", "--ff-only", published)
	gitCmd(t, fixture.context.GateDir, "update-ref", "refs/heads/feature", published, reviewed)
	gitCmd(t, fixture.remote, "fetch", "--no-tags", fixture.caller, "feature:feature")
	for index, environment := range fixture.context.Env {
		if environment == "FAKE_CLI_PR_HEAD_SHA="+reviewed {
			fixture.context.Env[index] = "FAKE_CLI_PR_HEAD_SHA=" + published
		}
	}
	for _, err := range []error{
		fixture.context.DB.UpdateRunHeadSHA(fixture.request.RunID, published),
		fixture.context.DB.UpdateRunPublication(fixture.request.RunID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: *fixture.context.Run.PushTargetFingerprint, Ref: "refs/heads/feature"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	fixture.context.Run, err = fixture.context.DB.GetRun(fixture.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.request.HeadSHA, fixture.request.PublishedHead = published, published
	fixture.request.PushGeneration = *fixture.context.Run.PushGeneration
	results, err := fixture.context.DB.GetStepsByRun(fixture.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.StepName == types.StepReview {
			if _, err := fixture.context.DB.InsertReviewStepRound(result.ID, 2, "coverage", result.FindingsJSON, nil, reviewed, 11); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fixture, reviewed
}

func TestCIHandbackPostDocumentPublicationRequiresExplicitRevalidation(t *testing.T) {
	fixture, reviewed := newPostDocumentCIHandbackFixture(t)
	sctx := fixture.context
	before, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepsBefore, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err == nil {
		t.Fatal("default handback certified a post-Test descendant")
	}
	if err := json.Unmarshal([]byte(`{"revalidate_publication":true}`), &fixture.request); err != nil {
		t.Fatal(err)
	}
	released, err := (&CIStep{}).HandbackCI(sctx, fixture.request)
	if err != nil {
		t.Fatalf("explicit post-Document custody route refused: %v", err)
	}
	if repeated, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err != nil || repeated.ReceiptID != released.ReceiptID {
		t.Fatalf("release was not one durable receipt: %#v %v", repeated, err)
	}
	after, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("release relabelled old review or changed publication/spending: %#v %v", after, err)
	}
	stepsAfter, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil || !reflect.DeepEqual(stepsBefore, stepsAfter) {
		t.Fatalf("release rewrote old Test or step evidence: %#v %v", stepsAfter, err)
	}
	receipt, err := sctx.DB.LatestCIHandback(sctx.Run.ID)
	if err != nil || receipt.Source == nil || receipt.Source.ReviewHead != reviewed || receipt.ReleasedHead != before.HeadSHA {
		t.Fatalf("receipt lost historical review identity: %#v %v", receipt, err)
	}
	if err := os.WriteFile(filepath.Join(fixture.caller, "correction.txt"), []byte("correct genuine CI defect\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.caller, "add", "correction.txt")
	gitCmd(t, fixture.caller, "commit", "-m", "correct failed CI")
	fixture.request.Resume, fixture.request.ReceiptID = true, released.ReceiptID
	fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	accepted, err := (&CIStep{}).HandbackCI(sctx, fixture.request)
	if err != nil || accepted.AcceptedHead != fixture.request.HeadSHA {
		t.Fatalf("authored correction refused: %#v %v", accepted, err)
	}
	after, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || after.ReviewApprovedHeadSHA != nil || *after.LastPushedSHA != before.HeadSHA || *after.PushGeneration != *before.PushGeneration {
		t.Fatalf("acceptance reused certification or published before revalidation: %#v %v", after, err)
	}
	index, err := pipeline.CIHandbackReviewRestart(sctx.DB, after, AllSteps())
	if err != nil || index != 2 {
		t.Fatalf("correction did not require fresh Review onward: %d %v", index, err)
	}
	if published := gitCmd(t, fixture.remote, "rev-parse", "feature"); published != before.HeadSHA {
		t.Fatal("acceptance published unvalidated work")
	}
	if err := publishRunHead(sctx, accepted.AcceptedHead, "", nil); err == nil || !strings.Contains(err.Error(), "fresh Review") {
		t.Fatalf("unreviewed correction reached publication: %v", err)
	}
	for _, result := range stepsBefore {
		if result.StepName == types.StepReview {
			if _, err := sctx.DB.InsertReviewStepRound(result.ID, 3, "initial", result.FindingsJSON, nil, accepted.AcceptedHead, 12); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.CompleteReviewStep(result.ID, sctx.Run.ID, accepted.AcceptedHead, 0, 12, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := publishRunHead(sctx, accepted.AcceptedHead, "", nil); err == nil || !strings.Contains(err.Error(), "fresh Test") {
		t.Fatalf("correction reused old Test evidence: %v", err)
	}
	for _, result := range stepsBefore {
		if result.StepName == types.StepTest {
			evidence := `{"findings":[],"tested_head_sha":"` + accepted.AcceptedHead + `"}`
			if err := sctx.DB.SetStepFindings(result.ID, evidence); err != nil {
				t.Fatal(err)
			}
			if err := publishRunHead(sctx, accepted.AcceptedHead, "", nil); err == nil || !strings.Contains(err.Error(), "fresh Test") {
				t.Fatalf("relabelled Test summary authorized publication: %v", err)
			}
			if _, err := sctx.DB.InsertStepRound(result.ID, 2, "initial", &evidence, nil, 12); err != nil {
				t.Fatal(err)
			}
		}
	}
	receipt, err = sctx.DB.LatestCIHandback(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCIHandbackPublication(sctx, receipt, accepted.AcceptedHead); err != nil {
		t.Fatalf("fresh exact Review and Test did not authorize publication: %v", err)
	}
	gitCmd(t, sctx.WorkDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "later document commit")
	later := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	if err := publishRunHead(sctx, later, "", nil); err == nil || !strings.Contains(err.Error(), "fresh Review") {
		t.Fatalf("later commit reused exact-head proof: %v", err)
	}
}

func TestCIHandbackPostDocumentRefusesChangedEvidenceAndAuthority(t *testing.T) {
	for _, mutation := range []string{"Test evidence", "review round", "repair selection", "generation", "published head", "active writer", "foreign caller", "receipt", "missing review proof", "unrelated test head"} {
		t.Run(mutation, func(t *testing.T) {
			fixture, reviewed := newPostDocumentCIHandbackFixture(t)
			sctx := fixture.context
			if err := json.Unmarshal([]byte(`{"revalidate_publication":true}`), &fixture.request); err != nil {
				t.Fatal(err)
			}
			released, err := (&CIStep{}).HandbackCI(sctx, fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fixture.caller, "correction.txt"), []byte("authored fix\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, fixture.caller, "add", "correction.txt")
			gitCmd(t, fixture.caller, "commit", "-m", "correct failed check")
			fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
			fixture.request.Resume, fixture.request.ReceiptID = true, released.ReceiptID
			results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "Test evidence", "unrelated test head":
				for _, result := range results {
					if result.StepName == types.StepTest {
						head := fixture.request.PublishedHead
						if mutation == "unrelated test head" {
							head = strings.Repeat("0", 40)
						}
						if err := sctx.DB.SetStepFindings(result.ID, `{"findings":[],"tested_head_sha":"`+head+`"}`); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "review round", "missing review proof":
				for _, result := range results {
					if result.StepName == types.StepReview {
						head := reviewed
						if mutation == "missing review proof" {
							head = ""
						}
						if _, err := sctx.DB.InsertReviewStepRound(result.ID, 3, "coverage", result.FindingsJSON, nil, head, 100); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "repair selection":
				selection := `["ci-1"]`
				if err := sctx.DB.SetStepRoundSelection(fixture.request.RoundID, &selection, db.RoundSelectionSourceAutoFix); err != nil {
					t.Fatal(err)
				}
			case "generation":
				fixture.request.PushGeneration++
			case "published head":
				fixture.request.PublishedHead = reviewed
			case "active writer":
				pid := os.Getpid()
				if err := sctx.DB.SetStepAgentActivity(fixture.gate.ID, "writer", &pid); err != nil {
					t.Fatal(err)
				}
			case "foreign caller":
				fixture.request.CallerDir = t.TempDir()
			case "receipt":
				fixture.request.ReceiptID = "foreign-receipt"
			}
			before, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err == nil {
				t.Fatal("changed evidence or authority accepted")
			}
			if after, err := sctx.DB.GetRun(sctx.Run.ID); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed run: %#v %v", after, err)
			}
			if head := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); head != fixture.request.PublishedHead && mutation != "published head" {
				t.Fatal("refusal moved managed source")
			}
		})
	}
}
