package db

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIRepairAdoptionSchemaIsAvailable(t *testing.T) {
	database := openTestDB(t)
	var count int
	if err := database.sql.QueryRow(`SELECT COUNT(*) FROM ci_repair_adoptions`).Scan(&count); err != nil {
		t.Fatalf("CI repair adoption audit is unavailable: %v", err)
	}
	if count != 0 {
		t.Fatalf("fresh adoption count = %d, want 0", count)
	}
}

func seedCIRepairAdoption(t *testing.T) (*DB, CIRepairAdoption) {
	t.Helper()
	database := openTestDB(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.test/upstream.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	publicHead := strings.Repeat("a", 40)
	run, err := database.InsertRunWithIntent(repo.ID, "fix/retained-repair", publicHead, strings.Repeat("b", 40), &RunIntent{Summary: "fix the original failure"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	managedDir := t.TempDir()
	if _, err := database.sql.Exec(`UPDATE runs SET status = 'running', worktree_dir = ?, review_approved_head_sha = head_sha,
		pr_url = 'https://example.test/pull/7', pr_state = 'open', pr_state_observed_at = 100,
		ci_ready_at = 101, ci_ready_no_ci = 1, last_pushed_sha = head_sha,
		push_target_kind = 'upstream', push_target_fingerprint = 'sha256:target', push_ref = 'refs/heads/fix/retained-repair',
		last_pushed_at = 99, push_generation = 3, awaiting_agent_since = 100, parked_ms = 2000, error = 'publication refused' WHERE id = ?`, managedDir, run.ID); err != nil {
		t.Fatal(err)
	}
	limits := []int{0, 1, 0, 2, 1, 3, 0, 0, 2}
	for index, name := range types.AllSteps() {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStepWithAutoFixLimit(step.ID, limits[index]); err != nil {
			t.Fatal(err)
		}
		findings := `{"findings":[{"id":"failure","severity":"error","description":"retained repair needs validation","action":"ask-user"}]}`
		if name != types.StepCI {
			findings = `{"findings":[]}`
		}
		if err := database.SetStepFindings(step.ID, findings); err != nil {
			t.Fatal(err)
		}
		round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 15)
		if err != nil {
			t.Fatal(err)
		}
		selected := `["failure"]`
		if err := database.SetStepRoundUserDecision(round.ID, &selected, RoundSelectionSourceUser, &findings); err != nil {
			t.Fatal(err)
		}
		if err := database.CompleteStep(step.ID, 0, 25, "old-step.log"); err != nil {
			t.Fatal(err)
		}
		if name == types.StepCI {
			summary := "committed a repair; publication refused"
			if _, err := database.InsertStepRound(step.ID, 2, "auto_fix", &findings, &summary, 30); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatus(step.ID, types.StepStatusFixReview); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := database.sql.Exec(`UPDATE step_results SET error = 'old diagnostic', override_reason = 'old waiver', approval_reason = 'old approval' WHERE id = ?`, step.ID); err != nil {
			t.Fatal(err)
		}
	}
	adoption := CIRepairAdoption{
		CandidateSHA: strings.Repeat("c", 40), CandidateTreeSHA: strings.Repeat("d", 40),
		Operator: "operator@example.test", Reason: "validate the exact retained repair from Review",
		CallerDir: repo.WorkingPath, CallerHeadSHA: publicHead, ManagedDir: managedDir,
		GateDir: t.TempDir(), GateBranch: "fix/retained-repair",
	}
	refreshCIRepairAdoption(t, database, &adoption, run.ID)
	return database, adoption
}

func refreshCIRepairAdoption(t *testing.T, database *DB, adoption *CIRepairAdoption, runID string) {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil || run == nil {
		t.Fatalf("get adoption run: %v", err)
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil || repo == nil {
		t.Fatalf("get adoption repository: %v", err)
	}
	adoption.ExpectedRepo, adoption.ExpectedRun = *repo, *run
	adoption.ExpectedGatesJSON, err = database.GetRunGates(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	adoption.Steps = nil
	for _, step := range steps {
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := CIRepairAdoptionStep{Snapshot: *step}
		if step.AutoFixLimit != nil {
			snapshot.EffectiveAutoFixLimit = *step.AutoFixLimit
		}
		for _, round := range rounds {
			snapshot.Rounds = append(snapshot.Rounds, *round)
		}
		adoption.Steps = append(adoption.Steps, snapshot)
	}
}

func TestCIRepairAdoptionRevokesAuthorityAndPreservesHistoryAndLimits(t *testing.T) {
	database, request := seedCIRepairAdoption(t)

	event, isNew, err := database.AdoptCIRepairForRevalidation(request)
	if err != nil || !isNew || event == nil {
		t.Fatalf("adopt = %#v, new=%t, err=%v", event, isNew, err)
	}
	if event.ID == "" || event.CreatedAt == 0 || event.Operator != request.Operator || event.Reason != request.Reason {
		t.Fatalf("adoption lost actual authorization: %#v", event)
	}
	run, err := database.GetRun(request.ExpectedRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectedRun := request.ExpectedRun
	expectedRun.HeadSHA = request.CandidateSHA
	expectedRun.ReviewApprovedHeadSHA, expectedRun.CIReadyAt = nil, nil
	expectedRun.CIReadyNoCI, expectedRun.AwaitingAgentSince, expectedRun.Error = false, nil, nil
	expectedRun.ParkedMS += max(int64(0), event.CreatedAt-*request.ExpectedRun.AwaitingAgentSince) * 1000
	expectedRun.UpdatedAt = event.CreatedAt
	if !reflect.DeepEqual(*run, expectedRun) {
		t.Fatalf("adoption changed run provenance or retained authority:\ngot  %#v\nwant %#v", *run, expectedRun)
	}
	for _, original := range request.Steps {
		step, err := database.GetStepResult(original.Snapshot.ID)
		if err != nil {
			t.Fatal(err)
		}
		if original.Snapshot.StepOrder < types.StepReview.Order() {
			if !reflect.DeepEqual(*step, original.Snapshot) {
				t.Fatalf("adoption changed earlier step %s", step.StepName)
			}
		} else {
			expected := StepResult{ID: step.ID, RunID: step.RunID, StepName: step.StepName, StepOrder: step.StepOrder, Status: types.StepStatusPending, AutoFixLimit: original.Snapshot.AutoFixLimit}
			if !reflect.DeepEqual(*step, expected) {
				t.Fatalf("reset step %s = %#v, want %#v", step.StepName, *step, expected)
			}
		}
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil || len(rounds) != len(original.Rounds) {
			t.Fatalf("round history changed: %v", err)
		}
		for index, round := range rounds {
			if !reflect.DeepEqual(*round, original.Rounds[index]) {
				t.Fatalf("adoption changed %s round %d or its selection", step.StepName, round.Round)
			}
		}
	}
	stored, err := database.GetCIRepairAdoption(run.ID)
	if err != nil || !reflect.DeepEqual(stored, event) || !reflect.DeepEqual(stored.Steps, request.Steps) {
		t.Fatalf("immutable original snapshots/limits = %#v, err=%v", stored, err)
	}
	for index, limit := range []int{0, 1, 0, 2, 1, 3, 0, 0, 2} {
		if stored.Steps[index].EffectiveAutoFixLimit != limit {
			t.Fatalf("original limit for %s = %d, want %d", stored.Steps[index].Snapshot.StepName, stored.Steps[index].EffectiveAutoFixLimit, limit)
		}
	}
}

func TestCIRepairAdoptionRejectsStaleDurableBindings(t *testing.T) {
	changes := map[string]string{
		"head":               `UPDATE runs SET head_sha = 'moved'`,
		"pushed head":        `UPDATE runs SET last_pushed_sha = 'moved'`,
		"generation":         `UPDATE runs SET push_generation = push_generation + 1`,
		"target kind":        `UPDATE runs SET push_target_kind = 'fork'`,
		"target fingerprint": `UPDATE runs SET push_target_fingerprint = 'other'`,
		"ref":                `UPDATE runs SET push_ref = 'refs/heads/other'`,
		"PR":                 `UPDATE runs SET pr_url = 'https://example.test/pull/8'`,
		"branch":             `UPDATE runs SET branch = 'other'`,
		"intent":             `UPDATE runs SET intent = 'other intent'`,
		"worktree":           `UPDATE runs SET worktree_dir = 'other'`,
		"repository URL":     `UPDATE repos SET upstream_url = 'https://example.test/other.git'`,
		"fork URL":           `UPDATE repos SET fork_url = 'https://example.test/fork.git'`,
		"repository path":    `UPDATE repos SET working_path = 'other'`,
		"gate pin":           `UPDATE runs SET gates_json = '[]'`,
		"step findings":      `UPDATE step_results SET findings_json = '{"findings":[]}' WHERE step_name = 'ci'`,
		"step error":         `UPDATE step_results SET error = 'changed' WHERE step_name = 'test'`,
		"step approval":      `UPDATE step_results SET approval_reason = 'changed' WHERE step_name = 'test'`,
		"step limit":         `UPDATE step_results SET auto_fix_limit = 999 WHERE step_name = 'review'`,
		"round findings":     `UPDATE step_rounds SET findings_json = '{"findings":[]}' WHERE round = 2`,
		"round selection":    `UPDATE step_rounds SET selected_finding_ids = '[]' WHERE round = 1`,
		"round source":       `UPDATE step_rounds SET selection_source = 'auto_fix' WHERE round = 1`,
		"round publication":  `UPDATE step_rounds SET repair_published = 1 WHERE round = 2`,
		"round removed":      `DELETE FROM step_rounds WHERE round = 2`,
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			if _, err := database.sql.Exec(change); err != nil {
				t.Fatal(err)
			}
			assertCIRepairAdoptionRefused(t, database, request)
		})
	}
}

func assertCIRepairAdoptionRefused(t *testing.T, database *DB, request CIRepairAdoption) {
	t.Helper()
	before := request
	refreshCIRepairAdoption(t, database, &before, request.ExpectedRun.ID)
	if event, isNew, err := database.AdoptCIRepairForRevalidation(request); err == nil || isNew || event != nil {
		t.Fatalf("unsafe adoption accepted: event=%t, new=%t, err=%v", event != nil, isNew, err)
	}
	after := request
	refreshCIRepairAdoption(t, database, &after, request.ExpectedRun.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refusal partially changed run, steps, or rounds")
	}
	if event, err := database.GetCIRepairAdoption(request.ExpectedRun.ID); err != nil || event != nil {
		t.Fatalf("refusal wrote audit event: %#v, %v", event, err)
	}
}

func TestCIRepairAdoptionRejectsUnsafeCurrentState(t *testing.T) {
	changes := map[string]string{
		"pending run":                `UPDATE runs SET status = 'pending'`,
		"completed run":              `UPDATE runs SET status = 'completed'`,
		"failed run":                 `UPDATE runs SET status = 'failed'`,
		"cancelled run":              `UPDATE runs SET status = 'cancelled'`,
		"closed PR":                  `UPDATE runs SET pr_state = 'closed'`,
		"merged PR":                  `UPDATE runs SET pr_state = 'merged'`,
		"unobserved PR":              `UPDATE runs SET pr_state_observed_at = NULL`,
		"custody returned":           `UPDATE runs SET custody_returned_at = 111`,
		"push active":                `UPDATE runs SET push_active = 1`,
		"no public binding":          `UPDATE runs SET push_generation = NULL`,
		"unparked run":               `UPDATE runs SET awaiting_agent_since = NULL`,
		"unparked CI":                `UPDATE step_results SET status = 'running' WHERE step_name = 'ci'`,
		"active agent":               `UPDATE step_results SET agent_pid = 123 WHERE step_name = 'test'`,
		"writer in another step":     `UPDATE step_results SET status = 'fixing' WHERE step_name = 'review'`,
		"skipped review":             `UPDATE step_results SET status = 'skipped' WHERE step_name = 'review'`,
		"skipped Test":               `UPDATE step_results SET status = 'skipped' WHERE step_name = 'test'`,
		"skipped push":               `UPDATE step_results SET status = 'skipped' WHERE step_name = 'push'`,
		"missing required step":      `DELETE FROM step_results WHERE step_name = 'lint'`,
		"missing round history":      `DELETE FROM step_rounds WHERE step_result_id IN (SELECT id FROM step_results WHERE step_name = 'test')`,
		"ambiguous round history":    `UPDATE step_rounds SET round = 1 WHERE round = 2`,
		"malformed selection":        `UPDATE step_rounds SET selected_finding_ids = 'not-json' WHERE round = 1`,
		"unknown selection source":   `UPDATE step_rounds SET selection_source = 'historical-receipt' WHERE round = 1`,
		"selection without IDs":      `UPDATE step_rounds SET selected_finding_ids = NULL WHERE round = 1`,
		"unknown round trigger":      `UPDATE step_rounds SET trigger_type = 'historical-receipt' WHERE round = 1`,
		"malformed latest findings":  `UPDATE step_rounds SET findings_json = 'not-json' WHERE round = 2`,
		"missing dispatch selection": `UPDATE step_rounds SET selected_finding_ids = NULL, selection_source = NULL WHERE round = 1`,
		"malformed gate pin":         `UPDATE runs SET gates_json = 'not-json'`,
		"already published repair":   `UPDATE step_rounds SET repair_published = 1 WHERE round = 2`,
		"negative original limit":    `UPDATE step_results SET auto_fix_limit = -1 WHERE step_name = 'ci'`,
		"malformed current findings": `UPDATE step_results SET findings_json = 'not-json' WHERE step_name = 'ci'`,
		"wrong step order":           `UPDATE step_results SET step_order = 2 WHERE step_name = 'review'`,
		"duplicate step":             `UPDATE step_results SET step_name = 'review' WHERE step_name = 'test'`,
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			if _, err := database.sql.Exec(change); err != nil {
				t.Fatal(err)
			}
			refreshCIRepairAdoption(t, database, &request, request.ExpectedRun.ID)
			assertCIRepairAdoptionRefused(t, database, request)
		})
	}
}

func TestCIRepairAdoptionRollsBackEveryBoundary(t *testing.T) {
	triggers := map[string]string{
		"audit":              `BEFORE INSERT ON ci_repair_adoptions`,
		"head and authority": `BEFORE UPDATE OF head_sha ON runs`,
		"middle reset":       `BEFORE UPDATE OF status ON step_results WHEN OLD.step_name = 'test'`,
		"last reset":         `AFTER UPDATE OF status ON step_results WHEN OLD.step_name = 'ci'`,
	}
	for name, trigger := range triggers {
		t.Run(name, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			if _, err := database.sql.Exec(`CREATE TEMP TRIGGER abort_adoption ` + trigger + ` BEGIN SELECT RAISE(ABORT, 'injected adoption failure'); END`); err != nil {
				t.Fatal(err)
			}
			assertCIRepairAdoptionRefused(t, database, request)
		})
	}
}

func TestCIRepairAdoptionRefusesIncompleteWrites(t *testing.T) {
	triggers := map[string]string{
		"audit ignored": `BEFORE INSERT ON ci_repair_adoptions`,
		"head ignored":  `BEFORE UPDATE OF head_sha ON runs`,
		"reset ignored": `BEFORE UPDATE OF status ON step_results WHEN OLD.step_name = 'ci'`,
	}
	for name, trigger := range triggers {
		t.Run(name, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			if _, err := database.sql.Exec(`CREATE TEMP TRIGGER ignore_adoption ` + trigger + ` BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			assertCIRepairAdoptionRefused(t, database, request)
		})
	}
}

func TestCIRepairAdoptionRejectsCompetingRunsAndAgents(t *testing.T) {
	cases := []struct {
		name      string
		branch    string
		status    types.RunStatus
		hasAgent  bool
		isRefused bool
	}{
		{name: "pending same branch", branch: "fix/retained-repair", status: types.RunPending, isRefused: true},
		{name: "running same branch", branch: "fix/retained-repair", status: types.RunRunning, isRefused: true},
		{name: "terminal same branch", branch: "fix/retained-repair", status: types.RunCompleted},
		{name: "idle other branch", branch: "fix/other", status: types.RunRunning},
		{name: "agent on another branch", branch: "fix/other", status: types.RunRunning, hasAgent: true, isRefused: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			other, err := database.InsertRun(request.ExpectedRepo.ID, testCase.branch, request.ExpectedRun.HeadSHA, request.ExpectedRun.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(other.ID, testCase.status); err != nil {
				t.Fatal(err)
			}
			if testCase.hasAgent {
				step, err := database.InsertStepResult(other.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				processID := 123
				if err := database.SetStepAgentActivity(step.ID, "agent running", &processID); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.isRefused {
				assertCIRepairAdoptionRefused(t, database, request)
			} else if _, isNew, err := database.AdoptCIRepairForRevalidation(request); err != nil || !isNew {
				t.Fatalf("unrelated non-writer prevented adoption: %v", err)
			}
		})
	}
}

func TestCIRepairAdoptionIncludesPinnedCustomGates(t *testing.T) {
	for _, condition := range []string{"complete", "skipped", "missing"} {
		t.Run(condition, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			pin := `[{"name":"extra-check","after":"review","command":"echo check"}]`
			if err := database.SetRunGates(request.ExpectedRun.ID, pin); err != nil {
				t.Fatal(err)
			}
			var gateID string
			if condition != "missing" {
				gate, err := database.InsertStepResult(request.ExpectedRun.ID, types.CustomGateStepName(types.StepReview, "extra-check"))
				if err != nil {
					t.Fatal(err)
				}
				gateID = gate.ID
				if err := database.CompleteStep(gate.ID, 0, 10, "gate.log"); err != nil {
					t.Fatal(err)
				}
				if _, err := database.InsertStepRound(gate.ID, 1, "initial", nil, nil, 10); err != nil {
					t.Fatal(err)
				}
				if condition == "skipped" {
					if err := database.UpdateStepStatus(gate.ID, types.StepStatusSkipped); err != nil {
						t.Fatal(err)
					}
				}
			}
			refreshCIRepairAdoption(t, database, &request, request.ExpectedRun.ID)
			if condition != "complete" {
				assertCIRepairAdoptionRefused(t, database, request)
				return
			}
			event, isNew, err := database.AdoptCIRepairForRevalidation(request)
			if err != nil || !isNew || event.ExpectedGatesJSON != pin {
				t.Fatalf("adopt custom gate: new=%t, err=%v", isNew, err)
			}
			gate, err := database.GetStepResult(gateID)
			if err != nil || gate.Status != types.StepStatusPending {
				t.Fatalf("custom gate was not reset: %v", err)
			}
		})
	}
}

func TestCIRepairAdoptionReplayNeverRenewsValidationOrLimits(t *testing.T) {
	database, request := seedCIRepairAdoption(t)
	first, isNew, err := database.AdoptCIRepairForRevalidation(request)
	if err != nil || !isNew {
		t.Fatalf("first adoption: %v", err)
	}
	if err := database.UpdateRunHeadSHA(request.ExpectedRun.ID, strings.Repeat("e", 40)); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(request.Steps[2].Snapshot.ID, 99); err != nil {
		t.Fatal(err)
	}
	before := request
	refreshCIRepairAdoption(t, database, &before, request.ExpectedRun.ID)
	for _, replay := range []CIRepairAdoption{request, *first} {
		event, isNew, err := database.AdoptCIRepairForRevalidation(replay)
		if err != nil || isNew || !reflect.DeepEqual(event, first) {
			t.Fatalf("exact replay = %#v, new=%t, err=%v", event, isNew, err)
		}
	}
	conflicts := map[string]func(*CIRepairAdoption){
		"operator":  func(request *CIRepairAdoption) { request.Operator = "another-operator" },
		"reason":    func(request *CIRepairAdoption) { request.Reason = "another reason" },
		"candidate": func(request *CIRepairAdoption) { request.CandidateSHA = strings.Repeat("f", 40) },
		"tree":      func(request *CIRepairAdoption) { request.CandidateTreeSHA = strings.Repeat("f", 40) },
		"caller":    func(request *CIRepairAdoption) { request.CallerDir = "other" },
	}
	for name, change := range conflicts {
		t.Run(name, func(t *testing.T) {
			conflict := request
			change(&conflict)
			if event, isNew, err := database.AdoptCIRepairForRevalidation(conflict); err == nil || isNew || event != nil {
				t.Fatalf("changed replay accepted: %#v, %t, %v", event, isNew, err)
			}
		})
	}
	after := request
	refreshCIRepairAdoption(t, database, &after, request.ExpectedRun.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("replay or conflict reset current validation")
	}
	stored, err := database.GetCIRepairAdoption(request.ExpectedRun.ID)
	if err != nil || !reflect.DeepEqual(stored, first) {
		t.Fatalf("replay changed original authority/limits: %v", err)
	}
}

func TestCIRepairAdoptionConcurrentRequestsApplyOnce(t *testing.T) {
	database, request := seedCIRepairAdoption(t)
	var sequence int
	var path string
	var schema string
	if err := database.sql.QueryRow(`PRAGMA database_list`).Scan(&sequence, &schema, &path); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	var workers sync.WaitGroup
	results := make(chan bool, 6)
	for index := range 6 {
		connection := database
		if index%2 != 0 {
			connection = second
		}
		workers.Go(func() {
			_, isNew, err := connection.AdoptCIRepairForRevalidation(request)
			if err != nil {
				t.Errorf("concurrent adoption: %v", err)
			}
			results <- isNew
		})
	}
	workers.Wait()
	close(results)
	count := 0
	for isNew := range results {
		if isNew {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("new adoptions = %d, want 1", count)
	}
}

func TestCIRepairAdoptionRequiresActualOperatorAndCompleteRequest(t *testing.T) {
	changes := map[string]func(*CIRepairAdoption){
		"operator absent":           func(request *CIRepairAdoption) { request.Operator = "" },
		"reason absent":             func(request *CIRepairAdoption) { request.Reason = " " },
		"candidate absent":          func(request *CIRepairAdoption) { request.CandidateSHA = "" },
		"candidate malformed":       func(request *CIRepairAdoption) { request.CandidateSHA = "retained-head" },
		"same public candidate":     func(request *CIRepairAdoption) { request.CandidateSHA = request.ExpectedRun.HeadSHA },
		"tree absent":               func(request *CIRepairAdoption) { request.CandidateTreeSHA = "" },
		"caller absent":             func(request *CIRepairAdoption) { request.CallerDir = "" },
		"caller head absent":        func(request *CIRepairAdoption) { request.CallerHeadSHA = "" },
		"managed directory absent":  func(request *CIRepairAdoption) { request.ManagedDir = "" },
		"gate directory absent":     func(request *CIRepairAdoption) { request.GateDir = "" },
		"gate branch changed":       func(request *CIRepairAdoption) { request.GateBranch = "other" },
		"snapshots absent":          func(request *CIRepairAdoption) { request.Steps = nil },
		"limit changed":             func(request *CIRepairAdoption) { request.Steps[2].EffectiveAutoFixLimit++ },
		"historical event identity": func(request *CIRepairAdoption) { request.ID = "receipt-from-an-old-run" },
		"historical event time":     func(request *CIRepairAdoption) { request.CreatedAt = 1 },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			database, request := seedCIRepairAdoption(t)
			change(&request)
			assertCIRepairAdoptionRefused(t, database, request)
		})
	}
}

func TestCIRepairAdoptionAuditIsImmutable(t *testing.T) {
	database, request := seedCIRepairAdoption(t)
	event, _, err := database.AdoptCIRepairForRevalidation(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE ci_repair_adoptions SET created_at = created_at + 1`,
		`DELETE FROM ci_repair_adoptions`,
	} {
		if _, err := database.sql.Exec(statement); err == nil {
			t.Fatalf("immutable audit accepted %s", statement)
		}
	}
	stored, err := database.GetCIRepairAdoption(request.ExpectedRun.ID)
	if err != nil || !reflect.DeepEqual(stored, event) {
		t.Fatalf("immutable audit changed: %#v, %v", stored, err)
	}
	if absent, err := database.GetCIRepairAdoption("unknown-run"); err != nil || absent != nil {
		t.Fatalf("unknown audit = %v, %v", absent, err)
	}
}

func TestCIRepairAdoptionRetainsOriginalLimitsAfterResetAndReopen(t *testing.T) {
	database, request := seedCIRepairAdoption(t)
	request.Operator = "operator-123"
	request.Reason = "Explicitly adopt this exact repair.\nStart validation from Review."
	event, _, err := database.AdoptCIRepairForRevalidation(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResetStepsFrom(request.ExpectedRun.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	step, err := database.GetStepResult(request.Steps[3].Snapshot.ID)
	if err != nil || step.AutoFixLimit != nil {
		t.Fatalf("ordinary reset stopped clearing limits: %v", err)
	}
	var sequence int
	var schema, path string
	if err := database.sql.QueryRow(`PRAGMA database_list`).Scan(&sequence, &schema, &path); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	stored, err := reopened.GetCIRepairAdoption(request.ExpectedRun.ID)
	if err != nil || !reflect.DeepEqual(stored, event) || stored.Operator != request.Operator || stored.Reason != request.Reason {
		t.Fatalf("original authorization and limits did not survive reset/reopen: %v", err)
	}
	if _, isNew, err := reopened.AdoptCIRepairForRevalidation(request); err != nil || isNew {
		t.Fatalf("recovered exact replay = new:%t, err:%v", isNew, err)
	}
	step, err = reopened.GetStepResult(request.Steps[3].Snapshot.ID)
	if err != nil || step.AutoFixLimit != nil {
		t.Fatalf("recovered replay changed ordinary reset result: %v", err)
	}
}

func TestCIRepairAdoptionConcurrentConflictingCandidatesHaveOneWinner(t *testing.T) {
	database, first := seedCIRepairAdoption(t)
	second := first
	second.CandidateSHA = strings.Repeat("e", 40)
	second.CandidateTreeSHA = strings.Repeat("f", 40)
	type adoptionResult struct {
		event *CIRepairAdoption
		isNew bool
		err   error
	}
	results := make(chan adoptionResult, 2)
	var workers sync.WaitGroup
	for _, request := range []CIRepairAdoption{first, second} {
		workers.Go(func() {
			event, isNew, err := database.AdoptCIRepairForRevalidation(request)
			results <- adoptionResult{event: event, isNew: isNew, err: err}
		})
	}
	workers.Wait()
	close(results)
	var winner *CIRepairAdoption
	refused := 0
	for result := range results {
		if result.err != nil {
			refused++
			continue
		}
		if !result.isNew || result.event == nil || winner != nil {
			t.Fatal("conflicting concurrent requests did not have exactly one newly accepted event")
		}
		winner = result.event
	}
	if refused != 1 || winner == nil {
		t.Fatalf("conflicting refusals = %d, want 1", refused)
	}
	stored, err := database.GetCIRepairAdoption(first.ExpectedRun.ID)
	if err != nil || !reflect.DeepEqual(stored, winner) {
		t.Fatalf("conflict replaced immutable winner: %v", err)
	}
	run, err := database.GetRun(first.ExpectedRun.ID)
	if err != nil || run.HeadSHA != winner.CandidateSHA {
		t.Fatalf("run did not adopt the winning candidate: %v", err)
	}
}

func TestCIRepairAdoptionKeepsAnOptionalSkippedIntent(t *testing.T) {
	database, request := seedCIRepairAdoption(t)
	if err := database.UpdateStepStatus(request.Steps[0].Snapshot.ID, types.StepStatusSkipped); err != nil {
		t.Fatal(err)
	}
	refreshCIRepairAdoption(t, database, &request, request.ExpectedRun.ID)
	if _, isNew, err := database.AdoptCIRepairForRevalidation(request); err != nil || !isNew {
		t.Fatalf("optional Intent skip prevented full revalidation: %v", err)
	}
	step, err := database.GetStepResult(request.Steps[0].Snapshot.ID)
	if err != nil || !reflect.DeepEqual(*step, request.Steps[0].Snapshot) {
		t.Fatalf("adoption changed optional Intent: %v", err)
	}
}
