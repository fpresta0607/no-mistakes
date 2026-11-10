package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// answerPhase is one state the fake run is served in for a number of run reads
// after the answer. The last phase holds forever.
type answerPhase struct {
	reads int
	run   func() *ipc.RunInfo
}

// answerFollow records what the fake daemon saw of the answer and the follow.
type answerFollow struct {
	answeredAt atomic.Int64
	getRuns    atomic.Int32
}

// sinceAnswer is the time since the daemon recorded the answer. A wait bound
// is measured from there: what comes before it is the command's own setup (the
// database, two git launches, the daemon connection), whose cost follows the
// host's load and is not what any bound here is about.
func (f *answerFollow) sinceAnswer(t *testing.T) time.Duration {
	t.Helper()
	at := f.answeredAt.Load()
	if at == 0 {
		t.Fatal("the answer never reached the daemon")
	}
	return time.Since(time.Unix(0, at))
}

// newAnswerFollowFixture serves an answer result and then walks the run
// through phases, each served for its number of run reads after the answer. A
// ticking event stream stands in for the daemon's state events, so the follow
// keeps reading. Phases advance on what the follow has read, never on the
// clock: a loaded host can space the follow's reads further apart than any
// fixed window, and a phase it never read is a scenario the test never ran.
//
// gateReconcileTimeout, when set, is written to the fixture root's global
// config, which is where axi answer reads the grace it gives a question park.
func newAnswerFollowFixture(t *testing.T, gateReconcileTimeout string, result ipc.AnswerReviewQuestionResult, phases []answerPhase) (*axiTimeoutFixture, *answerFollow) {
	t.Helper()
	follow := &answerFollow{}
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		answer: func() *ipc.AnswerReviewQuestionResult {
			follow.answeredAt.CompareAndSwap(0, time.Now().UnixNano())
			r := result
			return &r
		},
		subscribe: tickingSubscribe,
	})
	if gateReconcileTimeout != "" {
		p, err := paths.New()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.ConfigFile(), []byte("gate_reconcile_timeout: \""+gateReconcileTimeout+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var readsAfterAnswer atomic.Int32
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		follow.getRuns.Add(1)
		if follow.answeredAt.Load() == 0 {
			return phases[0].run(), nil
		}
		read := int(readsAfterAnswer.Add(1))
		served := 0
		for _, ph := range phases[:len(phases)-1] {
			served += ph.reads
			if read <= served {
				return ph.run(), nil
			}
		}
		return phases[len(phases)-1].run(), nil
	})
	return fx, follow
}

func tickingSubscribe(ctx context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
	return func(send func(interface{}) error) error {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if err := send(ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-timeout"}); err != nil {
					return nil
				}
			}
		}
	}, nil
}

func reviewParkedOn(t *testing.T, fx *axiTimeoutFixture, status types.StepStatus, items ...types.Finding) func() *ipc.RunInfo {
	return reviewParkedAtRound(t, fx, status, 0, items...)
}

// reviewParkedAtRound is reviewParkedOn with the step's persisted round count,
// which is what tells a re-park apart from the park an answer just released
// when both carry the same status.
func reviewParkedAtRound(t *testing.T, fx *axiTimeoutFixture, status types.StepStatus, round int, items ...types.Finding) func() *ipc.RunInfo {
	raw := findingsJSON(t, items, "review")
	return func() *ipc.RunInfo {
		run := fx.running()
		run.Steps = []ipc.StepResultInfo{{
			StepName:     types.StepReview,
			Status:       status,
			RoundCount:   round,
			FindingsJSON: &raw,
		}}
		return run
	}
}

// reviewRunning is the run with its review step back at work, which is what
// the gate a released answer leaves reads as.
func reviewRunning(fx *axiTimeoutFixture) *ipc.RunInfo {
	run := fx.running()
	run.Steps = []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusRunning}}
	return run
}

func reviewQuestion(id string) types.Finding {
	return types.Finding{
		ID: "question-" + id, Severity: types.FindingSeverityWarning, Action: types.ActionAskUser,
		Category: types.FindingCategoryReviewQuestion, Description: "Review question awaiting an answer: " + id + "?",
	}
}

func reviewCodeFinding() types.Finding {
	return types.Finding{ID: "review-1", Severity: "warning", File: "main.go", Action: types.ActionAskUser, Description: "calls os.Exit"}
}

var closedLastResumed = ipc.AnswerReviewQuestionResult{OK: true, Resumed: true, ClosedLast: true}

func TestAxiAnswer_LastAnswerFollowsTheRunToItsNextGate(t *testing.T) {
	var fx *axiTimeoutFixture
	var once sync.Once
	var stale, next func() *ipc.RunInfo
	build := func() {
		stale = reviewParkedOn(t, fx, types.StepStatusAwaitingApproval, reviewQuestion("q1"))
		next = reviewParkedOn(t, fx, types.StepStatusAwaitingApproval, reviewCodeFinding())
	}
	fx, _ = newAnswerFollowFixture(t, "", closedLastResumed, []answerPhase{
		{reads: 2, run: func() *ipc.RunInfo { once.Do(build); return stale() }},
		{reads: 2, run: func() *ipc.RunInfo { return reviewRunning(fx) }},
		{run: func() *ipc.RunInfo { once.Do(build); return next() }},
	})

	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "10s")
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}
	for _, want := range []string{"answered: true", "reviewer_resumed: true", "gate:", "review-1", "calls os.Exit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("answer output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "question-q1") {
		t.Fatalf("returned the gate the answer just released instead of the next one:\n%s", out)
	}
	if strings.Contains(out, "run: run-timeout\n") {
		t.Fatalf("scalar run key emitted beside the run object:\n%s", out)
	}
}

// An answer that lands before the park registers resumes nothing itself; the
// park then appears still carrying the question it closed until the gate's
// resumer releases it. That park is not the next decision point.
//
// The slow host spaces every run read further apart than the stale park's
// former 300ms window, as an overloaded test host did (issue #84): served on
// the clock, the follow never read that park, so the test ran no stale park
// at all, and the command's own setup counted against the bound.
func TestAxiAnswer_LastAnswerSkipsTheStalePreAnswerPark(t *testing.T) {
	for _, host := range []struct {
		name        string
		readLatency time.Duration
	}{
		{name: "idle host"},
		{name: "slow host", readLatency: 500 * time.Millisecond},
	} {
		t.Run(host.name, func(t *testing.T) {
			var fx *axiTimeoutFixture
			var stale func() *ipc.RunInfo
			var once sync.Once
			var staleServed atomic.Int32
			served := func(run func() *ipc.RunInfo) func() *ipc.RunInfo {
				return func() *ipc.RunInfo {
					time.Sleep(host.readLatency)
					return run()
				}
			}
			fx, follow := newAnswerFollowFixture(t, "8s", ipc.AnswerReviewQuestionResult{OK: true, ClosedLast: true}, []answerPhase{
				{reads: 1, run: served(func() *ipc.RunInfo { return reviewRunning(fx) })},
				{reads: 2, run: served(func() *ipc.RunInfo {
					once.Do(func() { stale = reviewParkedOn(t, fx, types.StepStatusAwaitingApproval, reviewQuestion("q1")) })
					staleServed.Add(1)
					return stale()
				})},
				{reads: 1, run: served(func() *ipc.RunInfo { return reviewRunning(fx) })},
				{run: served(func() *ipc.RunInfo { return fx.completed() })},
			})

			out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "10s")
			if err != nil {
				t.Fatalf("axi answer: %v\n%s", err, out)
			}
			waited := follow.sinceAnswer(t)
			if !strings.Contains(out, "outcome: passed") || !strings.Contains(out, "reviewer_resumed: false") {
				t.Fatalf("answer did not follow past the stale park to the outcome:\n%s", out)
			}
			if n := staleServed.Load(); n == 0 {
				t.Fatal("the follow never read the stale park, so this run did not exercise it")
			}
			if waited > 6*time.Second {
				t.Fatalf("stale park left after its release, yet the answer waited %s", waited)
			}
		})
	}
}

// A question park still there after one gate_reconcile_timeout is a genuine
// new question and is returned. The grace comes from the owning root's global
// config: under the 30s default this --wait would elapse first.
func TestAxiAnswer_GenuineReaskIsReturnedAfterTheGrace(t *testing.T) {
	var fx *axiTimeoutFixture
	var once sync.Once
	var stale, reask func() *ipc.RunInfo
	build := func() {
		stale = reviewParkedOn(t, fx, types.StepStatusAwaitingApproval, reviewQuestion("q1"))
		reask = reviewParkedOn(t, fx, types.StepStatusAwaitingApproval, reviewQuestion("q2"))
	}
	fx, _ = newAnswerFollowFixture(t, "1s", closedLastResumed, []answerPhase{
		{reads: 2, run: func() *ipc.RunInfo { once.Do(build); return stale() }},
		{reads: 2, run: func() *ipc.RunInfo { return reviewRunning(fx) }},
		{run: func() *ipc.RunInfo { once.Do(build); return reask() }},
	})

	started := time.Now()
	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "8s")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}
	if !strings.Contains(out, "question-q2") || !strings.Contains(out, "gate:") {
		t.Fatalf("genuine re-ask was not returned as the next gate:\n%s", out)
	}
	if elapsed < time.Second {
		t.Fatalf("re-ask returned after %s, before its grace", elapsed)
	}
}

func TestAxiAnswer_NonClosingAnswerReturnsAtOnce(t *testing.T) {
	var fx *axiTimeoutFixture
	fx, follow := newAnswerFollowFixture(t, "", ipc.AnswerReviewQuestionResult{OK: true, Open: 1, OpenIDs: []string{"q2"}}, []answerPhase{
		{run: func() *ipc.RunInfo { return reviewRunning(fx) }},
	})

	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it")
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}
	if !strings.Contains(out, "open_questions: 1") || !strings.Contains(out, "Answer the remaining questions") {
		t.Fatalf("non-closing answer lost its unchanged output:\n%s", out)
	}
	if n := follow.getRuns.Load(); n != 0 {
		t.Fatalf("non-closing answer read the run %d times; it must return at once", n)
	}
}

func TestAxiAnswer_LastAnswerWaitElapsedReattachesWithoutAnsweringAgain(t *testing.T) {
	var fx *axiTimeoutFixture
	fx, _ = newAnswerFollowFixture(t, "", closedLastResumed, []answerPhase{
		{run: func() *ipc.RunInfo { return reviewRunning(fx) }},
	})

	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "1s")
	assertWaitElapsed(t, err, out, "1s")
	if !strings.Contains(out, "Re-run `no-mistakes axi run`") {
		t.Fatalf("post-answer timeout did not provide a non-mutating reattach command:\n%s", out)
	}
	if strings.Contains(out, "Re-run `no-mistakes axi answer") {
		t.Fatalf("post-answer timeout instructed the caller to answer again:\n%s", out)
	}
}

// A question asked by the rereview inside a fix round parks as fix_review, so
// both the released park and the next one carry that status. Keyed on the
// review park status alone, the stale park read as already released and was
// handed back as this answer's next decision point.
func TestAxiAnswer_LastAnswerFollowsAFixReviewQuestionPark(t *testing.T) {
	var fx *axiTimeoutFixture
	var once sync.Once
	var stale, next func() *ipc.RunInfo
	build := func() {
		stale = reviewParkedOn(t, fx, types.StepStatusFixReview, reviewQuestion("q1"))
		next = reviewParkedOn(t, fx, types.StepStatusFixReview, reviewCodeFinding())
	}
	fx, _ = newAnswerFollowFixture(t, "", closedLastResumed, []answerPhase{
		{reads: 2, run: func() *ipc.RunInfo { once.Do(build); return stale() }},
		{reads: 2, run: func() *ipc.RunInfo { return reviewRunning(fx) }},
		{run: func() *ipc.RunInfo { once.Do(build); return next() }},
	})

	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "10s")
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}
	for _, want := range []string{"gate:", "review-1", "calls os.Exit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("answer output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "question-q1") {
		t.Fatalf("returned the fix_review gate the answer just released:\n%s", out)
	}
}

// The finalize turn can park again at the same status before any read observes
// the review step running. Keyed on the park status alone, that new gate read
// as the one this answer had just released, so the caller waited until --wait
// elapsed instead of being handed the decision it has to make.
func TestAxiAnswer_ReparkAtTheSameStatusIsReturnedAsTheNextGate(t *testing.T) {
	var fx *axiTimeoutFixture
	var once sync.Once
	var released, repark func() *ipc.RunInfo
	build := func() {
		released = reviewParkedAtRound(t, fx, types.StepStatusAwaitingApproval, 1, reviewQuestion("q1"))
		repark = reviewParkedAtRound(t, fx, types.StepStatusAwaitingApproval, 2, reviewCodeFinding())
	}
	fx, follow := newAnswerFollowFixture(t, "", closedLastResumed, []answerPhase{
		{run: func() *ipc.RunInfo { once.Do(build); return repark() }},
	})
	// The park the answer releases is read before the answer is sent, so its
	// round is the one the wait leaves.
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		once.Do(build)
		return released(), nil
	})

	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "6s")
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}
	for _, want := range []string{"gate:", "review-1", "calls os.Exit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("re-park at the same status was not returned as the next gate (missing %q):\n%s", want, out)
		}
	}
	if waited := follow.sinceAnswer(t); waited > 5*time.Second {
		t.Fatalf("answer waited %s for a gate that was already parked", waited)
	}
}

// An older daemon has no closed_last field, so it reads as false. It resumes
// the reviewer only when the last open question closed, so reviewer_resumed is
// the same signal there and must earn the same follow.
func TestAxiAnswer_ResumedWithoutClosedLastStillFollowsTheRun(t *testing.T) {
	var fx *axiTimeoutFixture
	var once sync.Once
	var next func() *ipc.RunInfo
	fx, _ = newAnswerFollowFixture(t, "", ipc.AnswerReviewQuestionResult{OK: true, Resumed: true}, []answerPhase{
		{reads: 2, run: func() *ipc.RunInfo { return reviewRunning(fx) }},
		{run: func() *ipc.RunInfo {
			once.Do(func() { next = reviewParkedOn(t, fx, types.StepStatusAwaitingApproval, reviewCodeFinding()) })
			return next()
		}},
	})

	out, err := executeCmd("axi", "answer", "--question", "q1", "--answer", "Keep it", "--wait", "10s")
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}
	for _, want := range []string{"gate:", "review-1", "calls os.Exit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a resumed reviewer without closed_last was not followed (missing %q):\n%s", want, out)
		}
	}
}
