package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Under side_by_side the review step is offered a context in which another
// step can do its work early. That context must not be able to touch the run:
// its Run is a copy and its Shared is its own.
func TestExecutor_SideLaneContextIsOfferedToTheReviewStepAlone(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{SideBySide: true}
	startHead := run.HeadSHA

	var reviewOffered, testOffered bool
	var lane *StepContext
	var laneOK, reviewLaneOK, absentLaneOK bool
	var reviewShared *RunShared
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		reviewOffered = sctx.SideLaneContext != nil
		if !reviewOffered {
			return &StepOutcome{}, nil
		}
		reviewShared = sctx.Shared
		lane, laneOK = sctx.SideLaneContext(types.StepTest)
		_, reviewLaneOK = sctx.SideLaneContext(types.StepReview)
		_, absentLaneOK = sctx.SideLaneContext(types.StepDocument)
		if laneOK {
			lane.Run.HeadSHA = "moved-by-the-lane"
		}
		return &StepOutcome{}, nil
	}}
	test := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		testOffered = sctx.SideLaneContext != nil
		if sctx.SideLane {
			t.Error("the Test step's own turn ran as a side lane")
		}
		if sctx.Run.HeadSHA != startHead {
			t.Errorf("run head = %q after a side lane moved its own copy, want %q", sctx.Run.HeadSHA, startHead)
		}
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, cfg, nil, []Step{review, test}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if !reviewOffered {
		t.Fatal("the review step was not offered a side lane context under side_by_side")
	}
	if testOffered {
		t.Fatal("the Test step was offered a side lane context, want the review step alone")
	}
	if !laneOK || lane == nil {
		t.Fatal("no side lane context for the Test step")
	}
	if reviewLaneOK {
		t.Fatal("the review step was offered a side lane of itself")
	}
	if absentLaneOK {
		t.Fatal("a side lane was offered for a step this pipeline does not have")
	}
	if !lane.SideLane {
		t.Fatal("the lane context is not marked as a side lane")
	}
	if lane.Shared == nil || lane.Shared == reviewShared {
		t.Fatal("the lane shares the run's in-memory state, want its own")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var testStepID string
	for _, step := range steps {
		if step.StepName == types.StepTest {
			testStepID = step.ID
		}
	}
	if testStepID == "" || lane.StepResultID != testStepID {
		t.Fatalf("lane step result = %q, want the Test step's own %q", lane.StepResultID, testStepID)
	}
	if lane.Config != cfg || lane.Repo != repo || lane.DB != database {
		t.Fatal("the lane context does not carry the run's config, repository and database")
	}
	if run.HeadSHA != startHead {
		t.Fatalf("run head = %q, want %q: a side lane changed the run", run.HeadSHA, startHead)
	}
}

// With the setting absent no step is offered a side lane: today's behavior.
func TestExecutor_NoSideLaneContextWithSideBySideOff(t *testing.T) {
	for name, cfg := range map[string]*config.Config{"off": {}, "no config": nil} {
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			offered := false
			review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				offered = offered || sctx.SideLaneContext != nil
				return &StepOutcome{}, nil
			}}
			test := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				offered = offered || sctx.SideLaneContext != nil
				return &StepOutcome{}, nil
			}}
			executor := NewExecutor(database, p, cfg, nil, []Step{review, test}, nil)
			if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if offered {
				t.Fatal("a step was offered a side lane context with side_by_side off")
			}
		})
	}
}

// A step the operator skipped does no work early either.
func TestExecutor_NoSideLaneForASkippedStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	laneOK := true
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if sctx.SideLaneContext == nil {
			t.Error("the review step was not offered a side lane context under side_by_side")
			return &StepOutcome{}, nil
		}
		_, laneOK = sctx.SideLaneContext(types.StepTest)
		return &StepOutcome{}, nil
	}}
	test := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		t.Error("the skipped Test step ran")
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, &config.Config{SideBySide: true}, nil, []Step{review, test}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepTest})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if laneOK {
		t.Fatal("a side lane was offered for a step this run skips")
	}
}

// What a step registers on the run's shared state is released when the run's
// execution ends, whether it completed or failed, so work started beside the
// review never outlives its run.
func TestExecutor_ClosesTheRunsSharedStateHoweverTheRunEnds(t *testing.T) {
	for name, failure := range map[string]error{"completed": nil, "failed": errors.New("later step failed")} {
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			closed := 0
			review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				sctx.Shared.OnClose(func() { closed++ })
				return &StepOutcome{}, nil
			}}
			test := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
				if closed != 0 {
					t.Error("the shared state was closed while the run was still executing")
				}
				if failure != nil {
					return nil, failure
				}
				return &StepOutcome{}, nil
			}}
			executor := NewExecutor(database, p, &config.Config{}, nil, []Step{review, test}, nil)
			err := executor.Execute(context.Background(), run, repo, t.TempDir())
			if (err != nil) != (failure != nil) {
				t.Fatalf("Execute() error = %v, want failure %v", err, failure)
			}
			if closed != 1 {
				t.Fatalf("closers ran %d time(s), want once", closed)
			}
		})
	}
}

func TestRunShared_BaselineAndClose(t *testing.T) {
	shared := &RunShared{}
	if _, _, recorded := shared.Baseline(); recorded {
		t.Fatal("a new run reports a recorded baseline")
	}
	shared.RecordBaseline("abc", true)
	if head, passed, recorded := shared.Baseline(); head != "abc" || !passed || !recorded {
		t.Fatalf("Baseline() = %q, %v, %v", head, passed, recorded)
	}
	shared.RecordBaseline("def", false)
	if head, passed, _ := shared.Baseline(); head != "def" || passed {
		t.Fatalf("Baseline() = %q, %v, want the latest record", head, passed)
	}
	if first, second := shared.CountPushGuardRestart(), shared.CountPushGuardRestart(); first != 1 || second != 2 {
		t.Fatalf("CountPushGuardRestart() = %d then %d, want 1 then 2", first, second)
	}

	var order []int
	shared.OnClose(func() { order = append(order, 1) })
	shared.OnClose(func() { order = append(order, 2) })
	shared.Close()
	shared.Close()
	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("closers ran as %v, want each once, newest first", order)
	}
	// Registered after the close, a closer runs at once instead of being lost.
	shared.OnClose(func() { order = append(order, 3) })
	if len(order) != 3 || order[2] != 3 {
		t.Fatalf("closers ran as %v, want a late one run at once", order)
	}

	// A nil value is the "no shared state" case some embeddings run with.
	var none *RunShared
	none.OnClose(func() { t.Error("a nil shared state kept a closer") })
	none.Close()
	none.RecordBaseline("x", true)
	if _, _, recorded := none.Baseline(); recorded {
		t.Fatal("a nil shared state recorded a baseline")
	}
}
