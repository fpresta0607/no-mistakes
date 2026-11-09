package steps

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newRebasedCIHandbackFixture(t *testing.T) *ordinaryCIHandbackFixture {
	t.Helper()
	fixture := newOrdinaryCIHandbackFixture(t)
	sctx := fixture.context
	submitted := *sctx.Run.SubmittedHeadSHA
	gitCmd(t, fixture.caller, "checkout", "main")
	if err := os.WriteFile(filepath.Join(fixture.caller, "main.txt"), []byte("advanced main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.caller, "add", "main.txt")
	gitCmd(t, fixture.caller, "commit", "-m", "advance main")
	gitCmd(t, fixture.remote, "fetch", fixture.caller, "main:main")
	gitCmd(t, fixture.caller, "checkout", "feature")
	gitCmd(t, sctx.WorkDir, "config", "user.name", "test")
	gitCmd(t, sctx.WorkDir, "config", "user.email", "test@example.com")
	sctx.Log = func(message string) { t.Log(message) }
	sctx.LogFile = sctx.Log
	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil || outcome.NeedsApproval || outcome.SkipRemaining {
		t.Fatalf("native rebase did not complete: %#v %v", outcome, err)
	}
	reviewed := sctx.Run.HeadSHA
	if reviewed == submitted {
		t.Fatal("native rebase did not rewrite the submitted commit")
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", submitted, reviewed); err == nil {
		t.Fatal("fixture does not reproduce the submitted ancestry failure")
	}
	results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := sql.Open("sqlite", fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	for _, result := range results {
		switch result.StepName {
		case types.StepReview:
			if _, err := connection.Exec("UPDATE step_rounds SET reviewed_head_sha = ? WHERE step_result_id = ?", reviewed, result.ID); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, reviewed); err != nil {
				t.Fatal(err)
			}
			if _, err := sctx.DB.InsertReviewStepRound(result.ID, 2, "auto_fix", result.FindingsJSON, nil, reviewed, 10); err != nil {
				t.Fatal(err)
			}
		case types.StepTest:
			evidence := `{"findings":[],"tested_head_sha":"` + reviewed + `"}`
			if err := sctx.DB.SetStepFindings(result.ID, evidence); err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Exec("UPDATE step_rounds SET findings_json = ? WHERE step_result_id = ?", evidence, result.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := sctx.DB.InsertStepRound(result.ID, 2, "auto_fix", &evidence, nil, 10); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, phase := range []string{"document", "lint"} {
		if err := os.WriteFile(filepath.Join(sctx.WorkDir, phase+".txt"), []byte(phase+" correction\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, sctx.WorkDir, "add", phase+".txt")
		gitCmd(t, sctx.WorkDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", phase+" correction")
	}
	published := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, sctx.GateDir, "update-ref", "refs/heads/feature", published, submitted)
	gitCmd(t, fixture.remote, "fetch", sctx.WorkDir, "+HEAD:refs/heads/feature")
	gitCmd(t, fixture.caller, "fetch", "--no-tags", sctx.GateDir, published)
	for _, err := range []error{
		sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, published),
		sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: published, TargetKind: "upstream", TargetFingerprint: *sctx.Run.PushTargetFingerprint, Ref: "refs/heads/feature"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index, environment := range sctx.Env {
		if strings.HasPrefix(environment, "FAKE_CLI_PR_HEAD_SHA=") {
			sctx.Env[index] = "FAKE_CLI_PR_HEAD_SHA=" + published
		}
	}
	fixture.request.PublishedHead = published
	fixture.request.PushGeneration = *sctx.Run.PushGeneration
	fixture.request.RevalidatePublication = true
	return fixture
}

func TestCIHandbackRebasedPublicationRevalidatesTheSameRun(t *testing.T) {
	fixture := newRebasedCIHandbackFixture(t)
	sctx := fixture.context
	before, err := sctx.DB.ReadCIHandbackValidation(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := *sctx.Run
	released, err := (&CIStep{}).HandbackCI(sctx, fixture.request)
	if err != nil {
		t.Fatalf("explicit revalidation refused native rebased publication: %v", err)
	}
	after, err := sctx.DB.GetRun(original.ID)
	if err != nil || !reflect.DeepEqual(&original, after) {
		t.Fatalf("release changed source identity: %#v %v", after, err)
	}
	receipt, err := sctx.DB.LatestCIHandback(original.ID)
	if err != nil || receipt.Source.SubmittedHead != *original.SubmittedHeadSHA || !reflect.DeepEqual(before, receipt.Source.Validation) {
		t.Fatalf("release lost historical provenance: %#v %v", receipt, err)
	}
	anchor := fmt.Sprintf("refs/no-mistakes/ci-handback/%s/%d", original.ID, *original.PushGeneration)
	gitCmd(t, fixture.caller, "merge", "--no-ff", "--no-edit", anchor+"/published")
	fixture.request.Resume, fixture.request.ReceiptID = true, released.ReceiptID
	fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
	accepted, err := (&CIStep{}).HandbackCI(sctx, fixture.request)
	if err != nil || accepted.RunID != original.ID || accepted.PRURL != *original.PRURL || accepted.AcceptedHead != fixture.request.HeadSHA {
		t.Fatalf("same-run resume refused: %#v %v", accepted, err)
	}
	after, err = sctx.DB.GetRun(original.ID)
	if err != nil || after.ReviewApprovedHeadSHA != nil || *after.SubmittedHeadSHA != *original.SubmittedHeadSHA || *after.LastPushedSHA != original.HeadSHA || *after.PushGeneration != *original.PushGeneration {
		t.Fatalf("resume replaced source or reused validation: %#v %v", after, err)
	}
	if index, err := pipeline.CIHandbackReviewRestart(sctx.DB, after, AllSteps()); err != nil || index != 2 {
		t.Fatalf("resume did not restart at Review: %d %v", index, err)
	}
	if err := sctx.DB.VerifyCIHandbackValidation(receipt); err != nil {
		t.Fatalf("resume changed historical rounds or spending: %v", err)
	}
	receipt, err = sctx.DB.LatestCIHandback(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverCIHandback(sctx, receipt); err != nil {
		t.Fatalf("restart could not verify the original rebased source: %v", err)
	}
	if err := publishRunHead(sctx, accepted.AcceptedHead, "", nil); err == nil || !strings.Contains(err.Error(), "fresh Review") {
		t.Fatalf("unvalidated correction could publish: %v", err)
	}
	for _, directory := range []string{fixture.caller, sctx.WorkDir} {
		gitCmd(t, directory, "cat-file", "-e", *original.SubmittedHeadSHA+"^{commit}")
	}
	if head := gitCmd(t, fixture.remote, "rev-parse", "feature"); head != original.HeadSHA {
		t.Fatal("resume published before fresh validation")
	}
}

func TestCIHandbackRebasedPublicationRefusesChangedPinnedRounds(t *testing.T) {
	for _, phase := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest} {
		t.Run(string(phase), func(t *testing.T) {
			fixture := newRebasedCIHandbackFixture(t)
			sctx := fixture.context
			if _, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err != nil {
				t.Fatal(err)
			}
			receipt, err := sctx.DB.LatestCIHandback(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for index, step := range receipt.Source.Validation.Steps {
				if step.StepName == phase {
					selection := `["changed"]`
					if err := sctx.DB.SetStepRoundSelection(receipt.Source.Validation.Rounds[index][0].ID, &selection, db.RoundSelectionSourceUser); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err == nil {
				t.Fatal("changed pinned history was accepted")
			}
			if after, err := sctx.DB.GetRun(sctx.Run.ID); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed run: %#v %v", after, err)
			}
			if after, err := sctx.DB.LatestCIHandback(sctx.Run.ID); err != nil || !reflect.DeepEqual(receipt, after) {
				t.Fatalf("refusal changed receipt: %#v %v", after, err)
			}
		})
	}
}

func TestCIHandbackRebasedPublicationRefusesUnprovenSource(t *testing.T) {
	for _, mutation := range []string{"missing Rebase", "foreign Rebase round", "missing Review head", "fabricated Review head", "unrelated submitted source", "Test disagreement", "wrong generation", "wrong round", "protected refusal", "changed forge"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := newRebasedCIHandbackFixture(t)
			sctx := fixture.context
			connection, err := sql.Open("sqlite", fixture.databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			var statement string
			var arguments []interface{}
			switch mutation {
			case "missing Rebase":
				statement = "DELETE FROM step_rounds WHERE step_result_id IN (SELECT id FROM step_results WHERE run_id = ? AND step_name = 'rebase')"
				arguments = []interface{}{sctx.Run.ID}
			case "foreign Rebase round":
				foreign, err := sctx.DB.InsertRun(sctx.Repo.ID, "foreign", sctx.Run.HeadSHA, sctx.Run.BaseSHA)
				if err != nil {
					t.Fatal(err)
				}
				statement = "UPDATE step_results SET run_id = ? WHERE run_id = ? AND step_name = 'rebase'"
				arguments = []interface{}{foreign.ID, sctx.Run.ID}
			case "missing Review head", "fabricated Review head":
				statement = "UPDATE step_rounds SET reviewed_head_sha = ? WHERE step_result_id IN (SELECT id FROM step_results WHERE run_id = ? AND step_name = 'review')"
				var head *string
				if mutation == "fabricated Review head" {
					head = &sctx.Run.BaseSHA
				}
				arguments = []interface{}{head, sctx.Run.ID}
			case "unrelated submitted source":
				gitCmd(t, fixture.caller, "checkout", "-b", "unrelated", sctx.Run.BaseSHA)
				if err := os.WriteFile(filepath.Join(fixture.caller, "unrelated.txt"), []byte("unrelated source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, fixture.caller, "add", "unrelated.txt")
				gitCmd(t, fixture.caller, "commit", "-m", "unrelated change")
				head := gitCmd(t, fixture.caller, "rev-parse", "HEAD")
				gitCmd(t, sctx.WorkDir, "fetch", fixture.caller, head)
				gitCmd(t, fixture.caller, "checkout", "feature")
				statement = "UPDATE runs SET submitted_head_sha = ? WHERE id = ?"
				arguments = []interface{}{head, sctx.Run.ID}
			case "Test disagreement":
				statement = "UPDATE step_results SET findings_json = '{}' WHERE run_id = ? AND step_name = 'test'"
				arguments = []interface{}{sctx.Run.ID}
			case "wrong generation":
				fixture.request.PushGeneration++
			case "wrong round":
				fixture.request.RoundID = "foreign"
			case "protected refusal":
				if err := sctx.DB.SetStepFindings(fixture.gate.ID, `{"findings":[{"id":"protected-path-refusal","severity":"error","action":"ask-user","description":"protected source"}]}`); err != nil {
					t.Fatal(err)
				}
			case "changed forge":
				for index, environment := range sctx.Env {
					if strings.HasPrefix(environment, "FAKE_CLI_PR_HEAD_SHA=") {
						sctx.Env[index] = "FAKE_CLI_PR_HEAD_SHA=" + sctx.Run.BaseSHA
					}
				}
			}
			if statement != "" {
				if _, err := connection.Exec(statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			before, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			validation, err := sctx.DB.ReadCIHandbackValidation(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			refs := make(map[string]string)
			for _, directory := range []string{fixture.caller, sctx.GateDir, fixture.remote} {
				refs[directory] = gitCmd(t, directory, "for-each-ref")
			}
			if _, err := (&CIStep{}).HandbackCI(sctx, fixture.request); err == nil {
				t.Fatal("unproven rebased publication was released")
			}
			if after, err := sctx.DB.GetRun(sctx.Run.ID); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed run: %#v %v", after, err)
			}
			if after, err := sctx.DB.ReadCIHandbackValidation(sctx.Run.ID); err != nil || !reflect.DeepEqual(validation, after) {
				t.Fatalf("refusal changed evidence: %#v %v", after, err)
			}
			if receipt, err := sctx.DB.LatestCIHandback(sctx.Run.ID); err != nil || receipt != nil {
				t.Fatalf("refusal left a receipt: %#v %v", receipt, err)
			}
			for directory, before := range refs {
				if after := gitCmd(t, directory, "for-each-ref"); after != before {
					t.Fatal("refusal mutated refs")
				}
			}
			if head := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD"); head != sctx.Run.HeadSHA {
				t.Fatal("refusal moved managed source")
			}
		})
	}
}

func TestCIHandbackRebasedPublicationRefusesIncompleteRoundHistory(t *testing.T) {
	for _, phase := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest} {
		for _, missingRound := range []int{1, 2} {
			for _, operation := range []string{"release", "resume", "recovery"} {
				t.Run(fmt.Sprintf("%s/missing_round_%d/%s", phase, missingRound, operation), func(t *testing.T) {
					fixture := newRebasedCIHandbackFixture(t)
					sctx := fixture.context
					validation, err := sctx.DB.ReadCIHandbackValidation(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					var stepID string
					for index, step := range validation.Steps {
						if step.StepName != phase {
							continue
						}
						stepID = step.ID
						for number := len(validation.Rounds[index]) + 1; number <= 3; number++ {
							if phase == types.StepReview {
								_, err = sctx.DB.InsertReviewStepRound(step.ID, number, "auto_fix", step.FindingsJSON, nil, *sctx.Run.ReviewApprovedHeadSHA, 10)
							} else {
								_, err = sctx.DB.InsertStepRound(step.ID, number, "auto_fix", step.FindingsJSON, nil, 10)
							}
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					if stepID == "" {
						t.Fatal("fixture has no target step")
					}
					var receipt *db.CIHandback
					if operation != "release" {
						released, err := (&CIStep{}).HandbackCI(sctx, fixture.request)
						if err != nil {
							t.Fatal(err)
						}
						receipt, err = sctx.DB.LatestCIHandback(sctx.Run.ID)
						if err != nil {
							t.Fatal(err)
						}
						anchor := fmt.Sprintf("refs/no-mistakes/ci-handback/%s/%d/published", sctx.Run.ID, receipt.PushGeneration)
						gitCmd(t, fixture.caller, "merge", "--no-ff", "--no-edit", anchor)
						fixture.request.Resume, fixture.request.ReceiptID = true, released.ReceiptID
						fixture.request.HeadSHA = gitCmd(t, fixture.caller, "rev-parse", "HEAD")
						if operation == "recovery" {
							if err := sctx.DB.PrepareCIHandback(receipt, fixture.request.HeadSHA, receipt.BaseHead, "HEAD", sctx.Repo); err != nil {
								t.Fatal(err)
							}
							gitCmd(t, sctx.WorkDir, "fetch", fixture.caller, fixture.request.HeadSHA)
							gitCmd(t, sctx.WorkDir, "merge", "--ff-only", fixture.request.HeadSHA)
						}
					}
					connection, err := sql.Open("sqlite", fixture.databasePath)
					if err != nil {
						t.Fatal(err)
					}
					defer connection.Close()
					result, err := connection.Exec("DELETE FROM step_rounds WHERE step_result_id = ? AND round = ?", stepID, missingRound)
					if err != nil {
						t.Fatal(err)
					}
					if count, err := result.RowsAffected(); err != nil || count != 1 {
						t.Fatalf("did not remove exactly one historical round: %d %v", count, err)
					}
					validation, err = sctx.DB.ReadCIHandbackValidation(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if receipt != nil {
						receipt.Source.Validation = validation
						encoded, err := json.Marshal(receipt.Source)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := connection.Exec("UPDATE ci_handbacks SET source_json = ? WHERE id = ?", string(encoded), receipt.ID); err != nil {
							t.Fatal(err)
						}
					}
					before, err := sctx.DB.GetRun(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					refs := make(map[string]string)
					for _, directory := range []string{fixture.caller, sctx.WorkDir, sctx.GateDir, fixture.remote} {
						refs[directory] = gitCmd(t, directory, "for-each-ref") + "\n" + gitCmd(t, directory, "rev-parse", "HEAD")
					}
					if operation == "recovery" {
						err = RecoverCIHandback(sctx, receipt)
					} else {
						_, err = (&CIStep{}).HandbackCI(sctx, fixture.request)
					}
					if err == nil || !strings.Contains(err.Error(), "round history") {
						t.Fatalf("incomplete %s history did not refuse %s: %v", phase, operation, err)
					}
					if after, err := sctx.DB.GetRun(sctx.Run.ID); err != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("refusal changed run: %#v %v", after, err)
					}
					if after, err := sctx.DB.ReadCIHandbackValidation(sctx.Run.ID); err != nil || !reflect.DeepEqual(validation, after) {
						t.Fatalf("refusal changed evidence or spending: %#v %v", after, err)
					}
					if after, err := sctx.DB.LatestCIHandback(sctx.Run.ID); err != nil || !reflect.DeepEqual(receipt, after) {
						t.Fatalf("refusal changed receipt: %#v %v", after, err)
					}
					for directory, before := range refs {
						if after := gitCmd(t, directory, "for-each-ref") + "\n" + gitCmd(t, directory, "rev-parse", "HEAD"); after != before {
							t.Fatal("refusal moved source or refs")
						}
					}
				})
			}
		}
	}
}
