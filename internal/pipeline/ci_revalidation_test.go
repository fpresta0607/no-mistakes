package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func prepareCIAdoptionRecovery(t *testing.T, database *db.DB, run *db.Run, entry types.StepName, status types.StepStatus) (*db.Run, *db.CIRepairAdoption, []types.StepName) {
	t.Helper()
	public := strings.Repeat("a", 40)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: public, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	original, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	event := &db.CIRepairAdoption{ExpectedRun: *original, CandidateSHA: run.HeadSHA}
	if err := database.UpdateRunHeadSHA(run.ID, event.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	names := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.CustomGateStepName(types.StepLint, "fixture"), types.StepPush, types.StepPR, types.StepCI}
	isAfterEntry := false
	for _, name := range names {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.SetStepAutoFixLimit(step.ID, 1); err != nil {
			t.Fatal(err)
		}
		if isAfterEntry {
			continue
		}
		if name == entry {
			isAfterEntry = true
			if status == types.StepStatusPending {
				continue
			}
		}
		if err := database.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
			t.Fatal(err)
		}
		findings := `{"findings":[],"tested_head_sha":"` + event.CandidateSHA + `"}`
		if name == entry && (status == types.StepStatusAwaitingApproval || status == types.StepStatusFixReview) {
			findingID := "operator-decision"
			if entry == types.StepPush {
				findingID = PublicationSelectionFindingID
			}
			findings = `{"findings":[{"id":"` + findingID + `","severity":"warning","action":"ask-user","description":"preserve this decision"}]}`
		}
		if _, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, event.CandidateSHA, 1); err != nil {
			t.Fatal(err)
		}
		if name == entry {
			if err := database.ParkStepForApproval(run.ID, step.ID, status, 0, 1, &findings); err != nil {
				t.Fatal(err)
			}
		} else if name == types.StepReview {
			if err := database.CompleteReviewStep(step.ID, run.ID, event.CandidateSHA, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := database.SetStepFindings(step.ID, findings); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, event, names
}

func TestCIAdoptionInterruptionCauseAndValidationBoundaryMatrix(t *testing.T) {
	entries := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.CustomGateStepName(types.StepLint, "fixture"), types.StepPush}
	for _, entry := range entries {
		for _, mode := range []string{"live gate", "recovered gate", "pending", "executing"} {
			for _, cause := range []string{"shutdown", "cancelled", "abort"} {
				t.Run(string(entry)+"/"+mode+"/"+cause, func(t *testing.T) {
					database, locations, run, repo := setupTest(t)
					run.HeadSHA = strings.Repeat("c", 40)
					if err := database.UpdateRunHeadSHA(run.ID, run.HeadSHA); err != nil {
						t.Fatal(err)
					}
					var event *db.CIRepairAdoption
					var names []types.StepName
					if mode == "recovered gate" || mode == "pending" {
						status := types.StepStatusAwaitingApproval
						if mode == "pending" {
							status = types.StepStatusPending
						}
						run, event, names = prepareCIAdoptionRecovery(t, database, run, entry, status)
					} else {
						event = &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: run.HeadSHA}
						names = entries
					}
					ctx, cancel := context.WithCancelCause(t.Context())
					defer cancel(nil)
					var cancellation error
					if cause == "abort" {
						cancellation = errors.New(types.RunCancelReasonAbortedByUser)
					} else if cause == "shutdown" {
						cancellation = errors.New("daemon shutting down")
					}
					ready := make(chan struct{}, 1)
					var steps []Step
					for _, name := range names {
						steps = append(steps, &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
							if sctx.Ctx.Err() != nil {
								return nil, context.Cause(sctx.Ctx)
							}
							outcome := &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA, Findings: `{"findings":[],"tested_head_sha":"` + run.HeadSHA + `"}`}
							if name == entry {
								outcome.NeedsApproval = true
							}
							return outcome, nil
						}})
					}
					executor := NewExecutor(database, locations, nil, nil, steps, func(observed ipc.Event) {
						if observed.StepName == nil || *observed.StepName != entry {
							return
						}
						if mode == "executing" && observed.Type == ipc.EventStepStarted {
							ready <- struct{}{}
							<-ctx.Done()
						} else if observed.Type == ipc.EventStepCompleted && observed.Status != nil && *observed.Status == string(types.StepStatusAwaitingApproval) {
							ready <- struct{}{}
						}
					})
					executor.adoption = event
					workDir := t.TempDir()
					done := make(chan error, 1)
					if mode == "pending" {
						cancel(cancellation)
					}
					go func() {
						if mode == "recovered gate" || mode == "pending" {
							done <- executor.ResumeCIAdoption(ctx, run, repo, workDir, event, nil, "")
						} else {
							done <- executor.Execute(ctx, run, repo, workDir)
						}
					}()
					if mode != "pending" {
						select {
						case <-ready:
						case err := <-done:
							t.Fatalf("validation never reached its boundary: %v", err)
						case <-time.After(5 * time.Second):
							t.Fatal("validation boundary never became observable")
						}
					}
					before, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					cancel(cancellation)
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("interruption returned success")
						}
					case <-time.After(5 * time.Second):
						t.Fatal("interrupted validation did not return")
					}
					stored, err := database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if cause == "abort" {
						if stored.Status != types.RunCancelled || stored.AwaitingAgentSince != nil || stored.PushActive || stored.Error == nil || *stored.Error != types.RunCancelReasonAbortedByUser {
							t.Fatalf("explicit cancellation stayed held: %+v", stored)
						}
					} else {
						after, err := database.GetStepsByRun(run.ID)
						if err != nil || stored.Status != types.RunRunning || !reflect.DeepEqual(before, after) {
							t.Fatalf("interruption changed retained validation: %+v, %v", stored, err)
						}
						if (mode == "live gate" || mode == "recovered gate") && stored.AwaitingAgentSince == nil {
							t.Fatal("shutdown cleared the parked gate")
						}
					}
				})
			}
		}
	}
}

func TestCIAdoptionEveryRecoveryEntryHoldsAPushValidationRestart(t *testing.T) {
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME", "CFO_ROOT", "CFO_HOME", "CODE_GOBLINS_ROOT", "GOBLINS_ROOT"} {
		t.Setenv(name, t.TempDir())
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	binDir := t.TempDir()
	ghName := "gh"
	if runtime.GOOS == "windows" {
		ghName += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(binDir, ghName), "./fakecli")
	winproc.Harden(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake credential service: %v\n%s", err, output)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "hosts.yml"), []byte("github.com:\n  user: fixture-operator\n  users:\n    fixture-operator: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"FAKE_CLI_MODE": "ci-revalidation", "FAKE_CLI_REAL_GIT": realGit, "FAKE_CLI_PUBLICATION_CONFIG": profile, "FAKE_CLI_PUBLICATION_SECRET": "disposable-credential-proof", "FAKE_CLI_PUBLICATION_LOST": filepath.Join(t.TempDir(), "absent")} {
		t.Setenv(key, value)
	}
	publication, err := forgecontext.ResolvePublication(t.Context(), "https://github.com/test/repo", "", forgecontext.PublicationSelection{ConfigDir: profile, ExpectedLogin: "fixture-operator"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.CustomGateStepName(types.StepLint, "fixture"), types.StepPush} {
		for _, status := range []types.StepStatus{types.StepStatusPending, types.StepStatusAwaitingApproval} {
			t.Run(string(entry)+"/"+string(status), func(t *testing.T) {
				database, locations, run, repo := setupTest(t)
				workDir := t.TempDir()
				initGitRepo(t, workDir)
				run.HeadSHA, err = git.HeadSHA(t.Context(), workDir)
				if err != nil {
					t.Fatal(err)
				}
				run, event, names := prepareCIAdoptionRecovery(t, database, run, entry, status)
				var steps []Step
				pushCalls, testCalls := 0, 0
				for _, name := range names {
					steps = append(steps, &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
						if name == types.StepPush {
							pushCalls++
							if _, _, recorded := sctx.Shared.Baseline(); recorded {
								t.Error("recovery fabricated an in-memory Test baseline")
							}
							return &StepOutcome{RestartFrom: types.StepTest}, nil
						}
						if name == types.StepTest {
							testCalls++
							if name.Order() < entry.Order() || testCalls > 1 {
								t.Error("Push reset completed Test validation")
								return &StepOutcome{NeedsApproval: true}, nil
							}
						}
						return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA, Findings: `{"findings":[],"tested_head_sha":"` + run.HeadSHA + `"}`}, nil
					}})
				}
				parked := make(chan types.StepName, 2)
				executor := NewExecutor(database, locations, &config.Config{SideBySide: true}, nil, steps, func(observed ipc.Event) {
					if observed.Type == ipc.EventStepCompleted && observed.StepName != nil && observed.Status != nil && *observed.Status == string(types.StepStatusAwaitingApproval) {
						parked <- *observed.StepName
					}
				})
				before, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					done <- executor.ResumeCIAdoption(ctx, run, repo, workDir, event, publication, "fixture-operator")
				}()
				isEntryGate := status == types.StepStatusAwaitingApproval
				for {
					select {
					case name := <-parked:
						if isEntryGate && name == entry {
							isEntryGate = false
							if entry != types.StepPush {
								if err := executor.Respond(entry, types.ActionApprove, nil); err != nil {
									t.Fatal(err)
								}
							}
							continue
						}
						if name != types.StepPush {
							t.Fatalf("validation restarted at %s", name)
						}
						results, err := database.GetStepsByRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						push := results[len(results)-3]
						if push.FindingsJSON == nil || !hasFindingID(*push.FindingsJSON, PublicationRevalidationFindingID) {
							t.Fatalf("Push did not preserve its validation requirement: %+v", push)
						}
					case err := <-done:
						t.Fatalf("recovered validation escaped its hold: %v", err)
					case <-time.After(10 * time.Second):
						t.Fatal("recovered validation did not reach its Push hold")
					}
					break
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("hold interruption = %v", err)
				}
				after, err := database.GetStepsByRun(run.ID)
				if err != nil || pushCalls != 1 {
					t.Fatalf("publication guard calls = %d, %v", pushCalls, err)
				}
				for index, original := range before {
					if original.Status == types.StepStatusCompleted && !reflect.DeepEqual(original, after[index]) {
						t.Fatalf("Push changed completed %s validation", original.StepName)
					}
				}
			})
		}
	}
}

func TestCIRepairSnapshotBindsOriginalRoundsAndEffectiveLimits(t *testing.T) {
	database, _, run, _ := setupTest(t)
	for _, name := range []types.StepName{types.StepReview, types.StepCI} {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepCI {
			if err := database.SetStepAutoFixLimit(step.ID, 1); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := database.InsertStepRound(step.ID, 1, "initial", nil, nil, 1); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := ReadCIRepairSnapshot(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := CIRepairSnapshotDigest(snapshot)
	if err != nil || len(digest) != 64 || len(snapshot.Steps) != 2 || snapshot.Steps[0].EffectiveAutoFixLimit != 0 || snapshot.Steps[1].EffectiveAutoFixLimit != 1 {
		t.Fatalf("incomplete original snapshot: %+v, %v", snapshot, err)
	}
	for _, original := range snapshot.Steps {
		rounds, err := database.GetRoundsByStep(original.Snapshot.ID)
		if err != nil || len(original.Rounds) != 1 || !reflect.DeepEqual(original.Rounds[0], *rounds[0]) {
			t.Fatalf("round snapshot differs: %+v, %v", original, err)
		}
	}
	if _, err := database.InsertStepRound(snapshot.Steps[1].Snapshot.ID, 2, "auto_fix", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	changed, err := ReadCIRepairSnapshot(database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, err := CIRepairSnapshotDigest(changed)
	if err != nil || changedDigest == digest {
		t.Fatal("new round did not invalidate the original binding")
	}
}

func TestCIPublicationSelectionEndsAtItsDurablePublication(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	binding := db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}
	if err := database.UpdateRunPublication(run.ID, binding); err != nil {
		t.Fatal(err)
	}
	original, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPublication(run.ID, binding); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{ExpectedRun: *original}
	executor.publicationForge = &forgecontext.Context{}
	ordinary := &forgecontext.Context{}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		step := &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.ForgeContext != ordinary {
				t.Error("completed publication selection leaked into a later operation")
			}
			return &StepOutcome{}, nil
		}}
		outcome, err := executor.executeWithPublicationSelection(step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo, ForgeContext: ordinary})
		if err != nil || outcome == nil || outcome.NeedsApproval {
			t.Fatalf("ordinary operation after publication failed: %+v, %v", outcome, err)
		}
	}
}

func TestCIPublicationReadFailurePreservesValidation(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{ExpectedRun: *run}
	step := newPassStep(types.StepPush)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	outcome, err := executor.executeWithPublicationSelection(step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo})

	if err != nil || outcome == nil || !outcome.NeedsApproval || !hasFindingID(outcome.Findings, PublicationSelectionFindingID) || !executor.publicationSelectionRequired || step.callCount() != 0 {
		t.Fatalf("unreadable publication did not hold validation: %+v, %v", outcome, err)
	}
}

func TestCIPublicationHoldSurvivesLiveAndRecoveredInterruption(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	step := newPassStep(types.StepPush)
	event := &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: run.HeadSHA}
	for _, isRecovered := range []bool{false, true} {
		parked := make(chan struct{}, 1)
		executor := NewExecutor(database, locations, nil, nil, []Step{step}, func(event ipc.Event) {
			if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
				parked <- struct{}{}
			}
		})
		executor.adoption = event
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		workDir := t.TempDir()
		go func() {
			if isRecovered {
				done <- executor.ResumeCIAdoption(ctx, run, repo, workDir, event, nil, "")
			} else {
				done <- executor.Execute(ctx, run, repo, workDir)
			}
		}()
		select {
		case <-parked:
		case err := <-done:
			cancel()
			t.Fatalf("publication hold returned before parking: %v", err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("publication hold never parked")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interruption = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("publication hold did not stop")
		}
		var err error
		run, err = database.GetRun(run.ID)
		if err != nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
			t.Fatalf("interruption terminalized publication: %+v, %v", run, err)
		}
		results, err := database.GetStepsByRun(run.ID)
		if err != nil || results[0].Status != types.StepStatusAwaitingApproval || step.callCount() != 0 {
			t.Fatalf("interruption consumed the durable hold: %+v, %v", results, err)
		}
	}
}

func TestCIPublicationUserAbortCancelsLiveAndRecoveredHolds(t *testing.T) {
	for _, mode := range []string{"live", "recovered"} {
		t.Run(mode, func(t *testing.T) {
			database, locations, run, repo := setupTest(t)
			step := newPassStep(types.StepPush)
			event := &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: run.HeadSHA}
			if mode == "recovered" {
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				result, err := database.InsertStepResult(run.ID, types.StepPush)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStepWithAutoFixLimit(result.ID, 0); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[{"id":"ci-publication-credentials","severity":"warning","action":"ask-user","description":"credentials unavailable"}]}`
				if _, err := database.InsertStepRound(result.ID, 1, "initial", &findings, nil, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.ParkStepForApproval(run.ID, result.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			parked := make(chan struct{}, 1)
			executor := NewExecutor(database, locations, nil, nil, []Step{step}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					parked <- struct{}{}
				}
			})
			executor.adoption = event
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			done := make(chan error, 1)
			workDir := t.TempDir()
			go func() {
				if mode == "recovered" {
					done <- executor.ResumeCIAdoption(ctx, run, repo, workDir, event, nil, "")
				} else {
					done <- executor.Execute(ctx, run, repo, workDir)
				}
			}()
			select {
			case <-parked:
			case err := <-done:
				t.Fatalf("hold returned before abort: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("hold never parked")
			}
			cancel(errors.New(types.RunCancelReasonAbortedByUser))
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("explicit abort returned success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("explicit abort did not finish")
			}
			stored, err := database.GetRun(run.ID)
			if err != nil || stored.Status != types.RunCancelled || stored.Error == nil || *stored.Error != types.RunCancelReasonAbortedByUser || stored.AwaitingAgentSince != nil || stored.PushActive {
				t.Fatalf("abort left a retained running hold: %+v, %v", stored, err)
			}
			if step.callCount() != 0 {
				t.Fatal("aborting credentials launched publication")
			}
		})
	}
}

func TestCIPublicationUserAbortCannotBecomeACredentialHold(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: run.HeadSHA}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New(types.RunCancelReasonAbortedByUser)
	cancel(cause)
	step := newPassStep(types.StepPush)

	outcome, err := executor.executeWithPublicationSelection(step, &StepContext{Ctx: ctx, Run: run, Repo: repo})

	if outcome != nil || !errors.Is(err, cause) || step.callCount() != 0 {
		t.Fatalf("abort became another publication hold: %+v, %v", outcome, err)
	}
}

func TestCIPublicationChangedCandidateRequiresANewDecision(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: strings.Repeat("c", 40)}
	step := newPassStep(types.StepPush)

	outcome, err := executor.executeWithPublicationSelection(step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo})

	if err != nil || outcome == nil || !outcome.NeedsApproval || !hasFindingID(outcome.Findings, PublicationRevalidationFindingID) || !executor.publicationSelectionRequired || step.callCount() != 0 {
		t.Fatalf("changed candidate was treated as a credential retry: %+v, %v", outcome, err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.HeadSHA != run.HeadSHA || stored.PushGeneration != nil {
		t.Fatalf("candidate refusal changed publication state: %+v, %v", stored, err)
	}
}

func TestCIAdoptionReceiptReplayRefusesChangedPublicationBinding(t *testing.T) {
	for _, name := range []string{"head", "target kind", "target fingerprint", "ref", "generation"} {
		t.Run(name, func(t *testing.T) {
			database, locations, run, repo := setupTest(t)
			binding := db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}
			if err := database.UpdateRunPublication(run.ID, binding); err != nil {
				t.Fatal(err)
			}
			original, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := &db.CIRepairAdoption{ExpectedRun: *original, CandidateSHA: strings.Repeat("c", 40)}
			binding.HeadSHA = event.CandidateSHA
			switch name {
			case "head":
				binding.HeadSHA = strings.Repeat("d", 40)
			case "target kind":
				binding.TargetKind = "fork"
			case "target fingerprint":
				binding.TargetFingerprint = "other"
			case "ref":
				binding.Ref = "refs/heads/other"
			case "generation":
				if err := database.UpdateRunPublication(run.ID, binding); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.UpdateRunPublication(run.ID, binding); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, locations, nil, nil, nil, nil)
			executor.adoption, executor.publicationOnlyResume = event, true
			step := newPassStep(types.StepPush)

			outcome, err := executor.executeWithPublicationSelection(step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo})

			if err != nil || outcome == nil || !outcome.NeedsApproval || step.callCount() != 0 {
				t.Fatalf("changed %s receipt was consumed: %+v, %v", name, outcome, err)
			}
		})
	}
}

func TestCIAdoptionResumesRecordedPushWithoutAnotherPublication(t *testing.T) {
	for _, boundary := range []string{"active push", "inactive push", "pending pr", "pending ci"} {
		t.Run(boundary, func(t *testing.T) {
			database, locations, run, repo := setupTest(t)
			binding := db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}
			if err := database.UpdateRunPublication(run.ID, binding); err != nil {
				t.Fatal(err)
			}
			original, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := &db.CIRepairAdoption{ExpectedRun: *original, CandidateSHA: strings.Repeat("c", 40)}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			var steps []Step
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepPush, types.StepPR, types.StepCI} {
				step, err := database.InsertStepResult(run.ID, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
					t.Fatal(err)
				}
				if name == types.StepReview {
					if err := database.CompleteReviewStep(step.ID, run.ID, event.CandidateSHA, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				} else if name == types.StepTest {
					if err := database.SetStepFindings(step.ID, `{"findings":[],"tested_head_sha":"`+event.CandidateSHA+`"}`); err != nil {
						t.Fatal(err)
					}
					if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				} else if name != types.StepPush {
					if err := database.UpdateStepStatus(step.ID, types.StepStatusPending); err != nil {
						t.Fatal(err)
					}
				}
				if (name == types.StepPush && (boundary == "pending pr" || boundary == "pending ci")) || (name == types.StepPR && boundary == "pending ci") {
					if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				}
				steps = append(steps, &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
					if name.Order() <= types.StepPush.Order() || (name == types.StepPR && boundary == "pending ci") {
						t.Errorf("recorded publication replayed %s", name)
					}
					if sctx.ForgeContext != nil {
						t.Error("temporary credentials survived the receipt")
					}
					return &StepOutcome{}, nil
				}})
			}
			binding.HeadSHA = event.CandidateSHA
			if err := database.UpdateRunPublication(run.ID, binding); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunPushActive(run.ID, boundary == "active push"); err != nil {
				t.Fatal(err)
			}
			interrupted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, locations, nil, nil, steps, nil)

			if err := executor.ResumeCIAdoption(t.Context(), interrupted, repo, t.TempDir(), event, &forgecontext.Context{}, "operator"); err != nil {
				t.Fatal(err)
			}

			stored, err := database.GetRun(run.ID)
			if err != nil || stored.Status != types.RunCompleted || stored.PushGeneration == nil || *stored.PushGeneration != *original.PushGeneration+1 || stored.PushActive || executor.publicationForge != nil {
				t.Fatalf("recorded receipt was not settled exactly once: %+v, %v", stored, err)
			}
			after, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatalf("receipt recovery changed completed validation: %v", err)
			}
			for index, step := range before {
				if step.Status == types.StepStatusCompleted && !reflect.DeepEqual(step, after[index]) {
					t.Fatalf("receipt recovery repeated completed %s", step.StepName)
				}
			}
		})
	}
}

func TestCIAdoptionRevokesOldScopesAndDurableSessions(t *testing.T) {
	database, locations, run, _ := setupTest(t)
	for _, role := range []SessionRole{SessionRoleReviewer, SessionRoleFixer} {
		if err := database.UpsertRunAgentSession(run.ID, string(role), "fake", "old-session"); err != nil {
			t.Fatal(err)
		}
	}
	executor := NewExecutor(database, locations, &config.Config{SessionReuse: true}, nil, nil, nil)
	executor.initializeRunScopes(run.ID)
	previousShared := executor.shared
	previousShared.RecordBaseline(run.HeadSHA, true)
	previousShared.SetSideLanes("old lane")
	wasClosed := false
	previousShared.OnClose(func() { wasClosed = true })
	executor.stepRecords = map[types.StepName]*db.StepResult{types.StepTest: {ID: "old-test"}}
	approved, ready := run.HeadSHA, int64(1)
	run.ReviewApprovedHeadSHA, run.CIReadyAt, run.TerminalHeadVerifiedAt = &approved, &ready, &ready
	adoption := &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: strings.Repeat("c", 40)}

	executor.acceptCIAdoption(run, adoption, nil, "operator")

	if !wasClosed || executor.shared == previousShared || executor.shared.SideLanes() != nil || executor.stepRecords != nil {
		t.Fatal("adoption reused the earlier side lanes or step results")
	}
	if _, _, recorded := executor.shared.Baseline(); recorded {
		t.Fatal("old Test baseline survived adoption")
	}
	if run.HeadSHA != adoption.CandidateSHA || run.ReviewApprovedHeadSHA != nil || run.CIReadyAt != nil || run.TerminalHeadVerifiedAt != nil {
		t.Fatal("adoption reused old head authority")
	}
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("old reviewer or fixer session survived: %+v, %v", sessions, err)
	}
}

func TestCIAdoptionFailedReservationLeavesTheOriginalGate(t *testing.T) {
	database, locations, _, _ := setupTest(t)
	executor := NewExecutor(database, locations, nil, nil, nil, nil)
	executor.waiting, executor.waitingStep = true, types.StepCI
	failure := errors.New("stale candidate")
	err := executor.AdoptCIRepair(func() (*db.CIRepairAdoption, error) { return nil, failure }, nil, "operator")
	if !errors.Is(err, failure) || !executor.waiting || executor.ciAdoptionReserved || len(executor.approvalCh) != 0 {
		t.Fatalf("failed adoption consumed the original gate: %v", err)
	}
}

func TestCIPublicationReselectionCannotConsumeFreshValidationHold(t *testing.T) {
	for _, findingID := range []string{PublicationSelectionFindingID, PublicationRevalidationFindingID} {
		t.Run(findingID, func(t *testing.T) {
			database, locations, run, _ := setupTest(t)
			step, err := database.InsertStepResult(run.ID, types.StepPush)
			if err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"` + findingID + `","severity":"warning","description":"preserve this gate","action":"ask-user"}]}`
			if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, locations, nil, nil, nil, nil)
			executor.adoption, executor.runID = &db.CIRepairAdoption{}, run.ID
			executor.waiting, executor.waitingStep, executor.publicationSelectionRequired = true, types.StepPush, true
			for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
				if err := executor.Respond(types.StepPush, action, nil); err == nil {
					t.Fatalf("ordinary %s consumed the hold", action)
				}
			}
			err = executor.ReselectPublication(&forgecontext.Context{}, "operator")
			if findingID == PublicationSelectionFindingID {
				if err != nil || executor.waiting || len(executor.approvalCh) != 1 {
					t.Fatalf("explicit credential reselection refused: %v", err)
				}
			} else if err == nil || !executor.waiting || len(executor.approvalCh) != 0 {
				t.Fatal("credential reselection consumed a validation requirement")
			}
		})
	}
}

func TestCIRetainedHeadUnreadablePreservesGateAndRefusesOrdinaryResponses(t *testing.T) {
	database, paths, run, _ := setupTest(t)
	public := strings.Repeat("a", 40)
	generation := int64(1)
	run.HeadSHA, run.LastPushedSHA, run.PushGeneration = public, &public, &generation
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	held, err := RetainedCIHead(context.Background(), run, t.TempDir())
	if !held || err == nil {
		t.Fatalf("unreadable retained head = held %v, error %v", held, err)
	}
	executor := NewExecutor(database, paths, nil, nil, nil, nil)
	executor.workDir, executor.runID = t.TempDir(), run.ID
	executor.waiting, executor.waitingStep = true, types.StepCI
	// An unreadable binding must be preserved, including when the DB read fails.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
		if err := executor.Respond(types.StepCI, action, nil); err == nil {
			t.Fatalf("%s consumed an unreadable CI hold", action)
		}
		if !executor.waiting || len(executor.approvalCh) != 0 {
			t.Fatal("refusal changed the waiting gate")
		}
	}
}

func TestCIAdoptionReservationPreventsOldGateReconciliation(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	step.resolved.Store(true)
	executor := NewExecutor(database, paths, nil, nil, []Step{step}, nil)
	executor.ciAdoptionReserved = true
	resolved, err := executor.reconcileApprovalGate(t.Context(), step, &StepContext{Ctx: t.Context(), Run: run, Repo: repo}, `{"findings":[]}`)
	if err != nil || resolved || step.calls.Load() != 0 {
		t.Fatalf("old CI reconciled after adoption reservation: resolved=%v error=%v calls=%d", resolved, err, step.calls.Load())
	}
}

func TestCIRetainedGuardLeavesProtectedPathRetryOwnership(t *testing.T) {
	database, locations, run, repo := setupTest(t)
	if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
		t.Fatal(err)
	}
	step := &protectedPathReconcilerStep{reconcilingApprovalStep: reconcilingApprovalStep{name: types.StepCI}}
	parked := make(chan struct{}, 2)
	executor := NewExecutor(database, locations, nil, nil, []Step{step}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
			parked <- struct{}{}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	workDir := t.TempDir()
	go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("protected-path executor did not stop")
		}
	})
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("protected-path refusal did not park")
	}
	if err := executor.Respond(types.StepCI, types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "use fix") {
		t.Fatalf("retained guard displaced the protected-path decision: %v", err)
	}
	if err := executor.Respond(types.StepCI, types.ActionFix, nil); err != nil {
		t.Fatalf("retained guard displaced the protected-path retry: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("original protected-path retry did not execute")
	}
}

func TestCIAdoptionLimitsAndReviewCarrySurviveOrdinaryReset(t *testing.T) {
	database, paths, run, _ := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"old","description":"old finding","file":"source.go","severity":"error","action":"auto-fix"}]}`
	first, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["old"]`
	if err := database.SetStepRoundUserDecision(first.ID, &selected, db.RoundSelectionSourceAutoFix, nil); err != nil {
		t.Fatal(err)
	}
	original, err := database.GetRoundsByStep(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	configuration := &config.Config{}
	configuration.AutoFix.Review = 9
	executor := NewExecutor(database, paths, configuration, nil, nil, nil)
	executor.adoption = &db.CIRepairAdoption{Steps: []db.CIRepairAdoptionStep{{Snapshot: *step, Rounds: []db.StepRound{*original[0]}, EffectiveAutoFixLimit: 1}}}
	state, err := executor.durableExecutionState(step.ID)
	if err != nil || state.autoFixAttempts != 1 || state.roundNum != 1 || state.outstandingFindings != "" || len(state.selectedOutstandingIDs) != 0 {
		t.Fatalf("adopted state renews budget or reuses review: %+v, %v", state, err)
	}
	if limit := executor.autoFixLimit(types.StepReview); limit != 1 {
		t.Fatalf("original limit changed to %d", limit)
	}
}

func TestCIAdoptionRestartsOnlyAnExactlyReviewedInterruptedPush(t *testing.T) {
	for _, name := range []string{"exact push", "inactive push", "parked handoff", "agent writer", "review missing", "interrupted test", "untested head", "validation hold", "protected hold", "interrupted pr"} {
		t.Run(name, func(t *testing.T) {
			database, locations, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "bound", Ref: "refs/heads/" + run.Branch}); err != nil {
				t.Fatal(err)
			}
			run, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			event := &db.CIRepairAdoption{ExpectedRun: *run, CandidateSHA: run.HeadSHA}
			run.Status, run.PushActive = types.RunRunning, true
			if name == "inactive push" || name == "parked handoff" {
				run.PushActive = false
			}
			head := run.HeadSHA
			run.ReviewApprovedHeadSHA = &head
			var steps []Step
			for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepPush, types.StepPR, types.StepCI} {
				step, err := database.InsertStepResult(run.ID, stepName)
				if err != nil {
					t.Fatal(err)
				}
				steps = append(steps, newPassStep(stepName))
				if stepName == types.StepReview || stepName == types.StepTest {
					if stepName == types.StepTest {
						if err := database.SetStepFindings(step.ID, `{"findings":[],"tested_head_sha":"`+head+`"}`); err != nil {
							t.Fatal(err)
						}
					}
					if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
				}
				if stepName == types.StepPush || (name == "interrupted test" && stepName == types.StepTest) {
					if err := database.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
						t.Fatal(err)
					}
				}
				if name == "agent writer" && stepName == types.StepPush {
					pid := 123
					if err := database.SetStepAgentActivity(step.ID, "active writer", &pid); err != nil {
						t.Fatal(err)
					}
				}
				if stepName == types.StepTest && name == "untested head" {
					if err := database.SetStepFindings(step.ID, `{"findings":[],"tested_head_sha":"other"}`); err != nil {
						t.Fatal(err)
					}
				}
				if stepName == types.StepPush && (name == "parked handoff" || name == "validation hold" || name == "protected hold") {
					findingID := PublicationSelectionFindingID
					if name == "validation hold" {
						findingID = PublicationRevalidationFindingID
					} else if name == "protected hold" {
						findingID = "protected-path-refusal"
					}
					findings := `{"findings":[{"id":"` + findingID + `","description":"preserved hold","severity":"warning","action":"ask-user"}]}`
					if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
						t.Fatal(err)
					}
					if err := database.CompleteRunAwaitingAgent(run.ID, 0); err != nil {
						t.Fatal(err)
					}
				}
				if stepName == types.StepPR && name == "interrupted pr" {
					if err := database.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "review missing" {
				run.ReviewApprovedHeadSHA = nil
			}
			index, err := CIAdoptionPendingIndex(database, run, steps, event)
			if name == "exact push" || name == "inactive push" || name == "parked handoff" {
				if err != nil || index != 2 {
					t.Fatalf("exact publication replay refused: %d, %v", index, err)
				}
				before, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				parked := make(chan struct{}, 1)
				executor := NewExecutor(database, locations, nil, nil, steps, func(event ipc.Event) {
					if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepPush && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
						parked <- struct{}{}
					}
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				workDir := t.TempDir()
				go func() { done <- executor.ResumeCIAdoption(ctx, run, repo, workDir, event, nil, "") }()
				select {
				case <-parked:
				case err := <-done:
					t.Fatalf("publication recovery did not reach its credential hold: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("publication recovery did not park")
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("recovery interruption = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("publication recovery did not stop")
				}
				after, err := database.GetStepsByRun(run.ID)
				if err != nil || !reflect.DeepEqual(before[:2], after[:2]) {
					t.Fatalf("publication recovery reset completed validation: %v", err)
				}
				stored, err := database.GetRun(run.ID)
				if err != nil || stored.Status != types.RunRunning || stored.AwaitingAgentSince == nil || *stored.PushGeneration != *event.ExpectedRun.PushGeneration {
					t.Fatalf("publication recovery lost its original hold or generation: %+v, %v", stored, err)
				}
			} else if err == nil {
				t.Fatal("interrupted validation or writer was replayable")
			}
		})
	}
}
