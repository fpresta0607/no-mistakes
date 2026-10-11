package pipeline

import "sync"

// HousekeepingLintResult is the lint assessment produced by the combined
// document+lint housekeeping pass: the document step performs both duties in
// one agent invocation and hands the lint half to the lint step so it does
// not pay a second cold agent pass.
type HousekeepingLintResult struct {
	// FindingsJSON holds the lint-category findings (possibly an empty set)
	// in the same JSON shape the lint step produces itself.
	FindingsJSON string
	// Summary is the housekeeping pass's one-line lint summary.
	Summary string
}

// RunShared carries in-memory run-scoped results one step hands to a later
// step in the same run. It lives on the executor for the run's lifetime and
// is never persisted: on any process boundary the consuming step simply
// falls back to doing its own work.
type RunShared struct {
	mu               sync.Mutex
	prepared         bool
	housekeepingLint *HousekeepingLintResult

	// The fields below serve global side_by_side and stay zero without it.

	// sideLanes is the steps package's record of work started beside the
	// review. It is opaque here: the steps own what it holds.
	sideLanes any
	// closers run once when the run's executor is done with this value, so
	// work started beside the review is stopped and its copies removed however
	// the run ended.
	closers []func()
	closed  bool
	// baseline is the last commit the Test step's baseline ran on in this
	// executor lifetime, and whether it passed there.
	baselineHead     string
	baselinePassed   bool
	baselineRecorded bool
	// pushGuardRestarts counts how often the Push step sent the run back to
	// Test because the baseline failed on the head to push.
	pushGuardRestarts int
}

// SideLanes returns what SetSideLanes stored, or nil.
func (s *RunShared) SideLanes() any {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sideLanes
}

// SetSideLanes stores the steps package's record of work started beside the
// review.
func (s *RunShared) SetSideLanes(lanes any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sideLanes = lanes
}

// OnClose registers fn to run when the executor is done with this value. A
// value that is already closed runs fn at once, so nothing registered late is
// left behind.
func (s *RunShared) OnClose(fn func()) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		fn()
		return
	}
	s.closers = append(s.closers, fn)
	s.mu.Unlock()
}

// Close runs every registered closer once, newest first. The executor calls
// it when a run's execution ends, whatever the outcome.
func (s *RunShared) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	closers := s.closers
	s.closers = nil
	s.closed = true
	s.mu.Unlock()
	for i := len(closers) - 1; i >= 0; i-- {
		closers[i]()
	}
}

// RecordBaseline records the commit the Test step's baseline last ran on and
// whether it passed there.
func (s *RunShared) RecordBaseline(headSHA string, passed bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baselineHead, s.baselinePassed, s.baselineRecorded = headSHA, passed, true
}

// Baseline returns what RecordBaseline last recorded. recorded is false when
// nothing was, as after a daemon restart, and a caller must then treat the
// head as untested.
func (s *RunShared) Baseline() (headSHA string, passed, recorded bool) {
	if s == nil {
		return "", false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baselineHead, s.baselinePassed, s.baselineRecorded
}

// CountPushGuardRestart counts one more return from Push to Test and returns
// the new total.
func (s *RunShared) CountPushGuardRestart() int {
	if s == nil {
		return 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushGuardRestarts++
	return s.pushGuardRestarts
}

// EnsurePrepared runs prepare once successfully for this executor lifetime.
// A failed attempt is not cached, so a caller may retry after the underlying
// problem is corrected.
func (s *RunShared) EnsurePrepared(prepare func() error) error {
	if s == nil {
		return prepare()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prepared {
		return nil
	}
	if err := prepare(); err != nil {
		return err
	}
	s.prepared = true
	return nil
}

// SetHousekeepingLint records the combined pass's lint assessment for the
// lint step. It replaces any previous assessment (a document fix round
// re-runs the combined pass and re-stashes a fresh result).
func (s *RunShared) SetHousekeepingLint(result HousekeepingLintResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = &result
}

// ClearHousekeepingLint discards a previous combined-pass lint assessment
// before a document pass starts, so a later lint step never consumes stale
// findings.
func (s *RunShared) ClearHousekeepingLint() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = nil
}

// TakeHousekeepingLint returns and consumes the combined pass's lint
// assessment. The second call returns false so a lint fix round re-assesses
// with its own agent pass instead of trusting a stale result.
func (s *RunShared) TakeHousekeepingLint() (HousekeepingLintResult, bool) {
	if s == nil {
		return HousekeepingLintResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.housekeepingLint == nil {
		return HousekeepingLintResult{}, false
	}
	result := *s.housekeepingLint
	s.housekeepingLint = nil
	return result, true
}
