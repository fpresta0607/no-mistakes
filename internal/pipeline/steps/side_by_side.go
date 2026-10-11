package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This file owns global side_by_side. Without the setting none of it runs.
//
// The executor keeps exactly one step, and one approval gate, in flight. That
// does not change. What changes is when two later steps do their work: the
// Test step (baseline command, then the live check) and the documentation
// pass start when the review starts, each on a throwaway clone of the run's
// worktree at the commit the review reads. Such a clone is a side lane. A lane
// cannot change the run: it has its own repository, its own copy of the run
// record, and no approval.
//
// Each step then decides at its own turn, in its usual order, whether the
// lane's work is still the work it would do now:
//
//   - The run's head must still be the commit the lane ran on. A review fix
//     moves the head, and the step then runs as it always has, on the new head.
//     A lane is stopped as soon as the head moves, not only at its turn.
//   - The run's worktree must hold nothing uncommitted for the Test step, and
//     nothing uncommitted on a path the documentation commit changes.
//   - The lane must have finished without an error.
//
// Anything else discards the lane and runs the step in the run's worktree.
// A lane's result is therefore never more than the same step, on the same
// commit, started earlier. A repository whose worktree a plain clone cannot
// reproduce (submodules, Git LFS) starts nothing beside the review.
//
// What starting early cannot settle is what gets pushed. The baseline ran on
// the commit the review started on, and the documentation pass, the lint step
// and the formatter can all add commits after it. baselineBeforePush closes
// that: it names every commit after the tested one, and when those commits
// touch more than documentation it runs the baseline again on the head about
// to be pushed.

const (
	// sideLaneDirName is the folder of a run's evidence directory that holds
	// what side lanes wrote there. A discarded lane's folder is removed.
	sideLaneDirName = "side-lane"
	// sideLaneBranch names a lane's commit inside its clone so the run's
	// worktree can fetch it.
	sideLaneBranch = "no-mistakes-side-lane"
	// sideLaneLogLimit bounds how much of a lane's log is kept in memory.
	sideLaneLogLimit = 1 << 20
	// pushGuardCommitLimit bounds how many commits a note names.
	pushGuardCommitLimit = 20
)

// sideLaneAgents are the harnesses whose adapters keep no state between
// calls, so one agent object can serve a lane's call while it serves the
// review's. Any other harness runs the steps one after another.
var sideLaneAgents = map[types.AgentName]bool{types.AgentClaude: true, types.AgentCodex: true}

// errSideLaneStopped is the cancellation cause of a lane the run no longer
// needs.
var errSideLaneStopped = errors.New("side lane stopped")

// sideLaneLog keeps the tail of what a lane's step logged.
type sideLaneLog struct {
	mu   sync.Mutex
	text []byte
}

func (l *sideLaneLog) write(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text = append(l.text, text...)
	if over := len(l.text) - sideLaneLogLimit; over > 0 {
		l.text = append([]byte(nil), l.text[over:]...)
	}
}

func (l *sideLaneLog) line(text string) { l.write(strings.TrimRight(text, "\n") + "\n") }

func (l *sideLaneLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.text)
}

// sideLane is one step's work started beside the review.
type sideLane struct {
	step pipeline.Step
	// head is the commit the lane runs on: the run's head when the review started.
	head    string
	started time.Time
	cancel  context.CancelCauseFunc
	done    chan struct{}
	log     sideLaneLog

	// Written by run before done is closed, read only after it.
	scratch     string
	checkout    string
	evidenceDir string
	shared      *pipeline.RunShared
	elapsed     time.Duration
	outcome     *pipeline.StepOutcome
	err         error
	// finalHead is the clone's HEAD after the step, and leftover its
	// uncommitted changes as NUL-delimited porcelain status.
	finalHead string
	leftover  string
	// changed are the paths the lane's own commit changes, when it made one.
	changed []string
}

// sideLanes is a run's set of side lanes, kept on its RunShared.
type sideLanes struct {
	mu    sync.Mutex
	lanes map[types.StepName]*sideLane
}

// claim hands out a lane once: the step that owns it either takes its result
// or discards it, and a later round of that step starts from nothing.
func (l *sideLanes) claim(name types.StepName) *sideLane {
	l.mu.Lock()
	defer l.mu.Unlock()
	lane := l.lanes[name]
	delete(l.lanes, name)
	return lane
}

// claimStale hands out every lane started on a commit other than head.
func (l *sideLanes) claimStale(head string) []*sideLane {
	l.mu.Lock()
	defer l.mu.Unlock()
	var stale []*sideLane
	for name, lane := range l.lanes {
		if lane.head != head {
			stale = append(stale, lane)
			delete(l.lanes, name)
		}
	}
	return stale
}

// close stops and removes every lane no step claimed. It runs when the run's
// execution ends, so a lane never outlives its run.
func (l *sideLanes) close() {
	l.mu.Lock()
	lanes := l.lanes
	l.lanes = map[types.StepName]*sideLane{}
	l.mu.Unlock()
	for _, lane := range lanes {
		lane.discard()
	}
}

// startSideLanes starts the Test step's work and the documentation pass
// beside the review that is about to run. It is called once the review has
// resolved its base, and does nothing unless the executor offered side lanes.
func startSideLanes(sctx *pipeline.StepContext, baseSHA string) {
	if sctx.SideLaneContext == nil || sctx.SideLane || sctx.Shared == nil || sctx.Config == nil || !sctx.Config.SideBySide {
		return
	}
	// Only the review's first pass has later steps still to come on the same
	// commit. A fix round, an answer round or a replay does not.
	if sctx.Fixing || sctx.FinalizingAnswers || sctx.SkipFixExecution || sctx.EvalReplay {
		return
	}
	// Once per run: a review that is entered again later starts nothing new.
	if sctx.Shared.SideLanes() != nil {
		return
	}
	lanes := &sideLanes{lanes: map[types.StepName]*sideLane{}}
	sctx.Shared.SetSideLanes(lanes)
	sctx.Shared.OnClose(lanes.close)

	if reason := sideLaneRefusal(sctx); reason != "" {
		sctx.Log("side by side: nothing starts beside the review: " + reason)
		return
	}
	for _, step := range []pipeline.Step{&TestStep{}, &DocumentStep{}} {
		name := step.Name()
		// With no commands.lint the documentation pass also fixes lint, so it
		// may change code. Code changed beside the baseline would only be
		// tested again, so that pass keeps its place after the Test step.
		if name == types.StepDocument && strings.TrimSpace(sctx.Config.Commands.Lint) == "" {
			sctx.Log("side by side: the documentation pass keeps its place after the Test step, because with no commands.lint it also fixes lint and may change code")
			continue
		}
		laneCtx, ok := sctx.SideLaneContext(name)
		if !ok {
			continue
		}
		ctx, cancel := context.WithCancelCause(laneCtx.Ctx)
		laneCtx.Ctx = ctx
		laneCtx.Env = sctx.Env
		lane := &sideLane{step: step, head: sctx.Run.HeadSHA, started: time.Now(), cancel: cancel, done: make(chan struct{})}
		lanes.mu.Lock()
		lanes.lanes[name] = lane
		lanes.mu.Unlock()
		sctx.Log(fmt.Sprintf("side by side: the %s step's work starts beside the review, on %s", name, shortObjectID(lane.head)))
		go lane.run(laneCtx, sctx.WorkDir, baseSHA)
	}
}

// sideLaneRefusal says why this run starts nothing beside the review, or "".
func sideLaneRefusal(sctx *pipeline.StepContext) string {
	agents := sctx.Config.Agents
	if len(agents) == 0 {
		agents = []types.AgentName{sctx.Config.Agent}
	}
	for _, name := range agents {
		if !sideLaneAgents[name] {
			return fmt.Sprintf("the %q harness is not known to serve two calls at once", name)
		}
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Sprintf("the worktree head could not be read: %v", err)
	}
	if head != sctx.Run.HeadSHA {
		return fmt.Sprintf("the worktree is at %s and the run's recorded head is %s", shortObjectID(head), shortObjectID(sctx.Run.HeadSHA))
	}
	return sideLaneCopyGap(sctx)
}

// sideLaneCopyGap says what a lane's copy of the worktree would lack, or "".
// A clone checks out tracked files only. It initializes no submodule, and its
// Git LFS files would come from a remote it does not have, so a test run there
// could pass or fail for a reason the run's own worktree does not share.
func sideLaneCopyGap(sctx *pipeline.StepContext) string {
	if _, err := os.Lstat(filepath.Join(sctx.WorkDir, ".gitmodules")); err == nil {
		return "the repository has submodules, which a copy of the worktree would not hold"
	}
	listed, err := stepGitRunRaw(sctx, "ls-files", "-z", "--", ":(glob)**/.gitattributes")
	if err != nil {
		return fmt.Sprintf("the repository's .gitattributes files could not be listed: %v", err)
	}
	for _, path := range changedPathList(listed) {
		data, err := os.ReadFile(filepath.Join(sctx.WorkDir, filepath.FromSlash(path)))
		if err != nil {
			return fmt.Sprintf("%s could not be read: %v", path, err)
		}
		if strings.Contains(string(data), "filter=lfs") {
			return "the repository keeps files in Git LFS, which a copy of the worktree would not hold"
		}
	}
	return ""
}

// stopStaleSideLanes stops and removes every lane started on a commit other
// than head, as soon as the run's head moves to it. The lane's step would
// discard it at its own turn, and until then it would only hold the machine.
func stopStaleSideLanes(sctx *pipeline.StepContext, head string) {
	if sctx.SideLane || sctx.Shared == nil {
		return
	}
	lanes, _ := sctx.Shared.SideLanes().(*sideLanes)
	if lanes == nil {
		return
	}
	for _, lane := range lanes.claimStale(head) {
		lane.discard()
		sctx.Log(fmt.Sprintf("side by side: the %s work started beside the review on %s is stopped and removed, because the run's head moved to %s", lane.step.Name(), shortObjectID(lane.head), shortObjectID(head)))
	}
}

// run does the lane's step in a throwaway clone of the run's worktree. A
// separate clone rather than a linked worktree keeps the gate repository and
// the run's worktree untouched, as the base attribution run already does.
func (lane *sideLane) run(sctx *pipeline.StepContext, runWorkDir, baseSHA string) {
	defer close(lane.done)
	defer func() { lane.elapsed = time.Since(lane.started) }()
	// The lane is speculative. A panic in it must cost the lane, never the daemon.
	defer func() {
		if recovered := recover(); recovered != nil {
			lane.outcome, lane.err = nil, fmt.Errorf("side lane panicked: %v", recovered)
		}
	}()
	lane.shared = sctx.Shared
	lane.outcome, lane.err = lane.execute(sctx, runWorkDir, baseSHA)
}

func (lane *sideLane) execute(sctx *pipeline.StepContext, runWorkDir, baseSHA string) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	scratch, err := os.MkdirTemp("", "no-mistakes-side-"+string(lane.step.Name())+"-")
	if err != nil {
		return nil, fmt.Errorf("create side lane directory: %w", err)
	}
	lane.scratch = scratch
	lane.checkout = filepath.Join(scratch, "work")
	source, err := filepath.Abs(runWorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve run worktree: %w", err)
	}
	sctx.WorkDir = runWorkDir
	if _, err := stepGitRun(sctx, "clone", "--quiet", "--shared", "--no-checkout", source, lane.checkout); err != nil {
		return nil, fmt.Errorf("clone the run worktree: %w", err)
	}
	// A pipeline commit in the clone must carry the identity it would carry
	// in the run's worktree, which may be set only there.
	for _, key := range []string{"user.name", "user.email"} {
		if value, err := stepGitRun(sctx, "config", "--get", key); err == nil && value != "" {
			if _, err := stepGitRun(sctx, "-C", lane.checkout, "config", key, value); err != nil {
				return nil, fmt.Errorf("carry %s into the side lane: %w", key, err)
			}
		}
	}
	sctx.WorkDir = lane.checkout
	if _, err := stepGitRun(sctx, "checkout", "--quiet", "--detach", lane.head); err != nil {
		return nil, fmt.Errorf("check out %s in the side lane: %w", shortObjectID(lane.head), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}

	sctx.SideLaneBaseSHA = baseSHA
	if sctx.EvidenceDir != "" {
		sctx.EvidenceDir = filepath.Join(sctx.EvidenceDir, sideLaneDirName, string(lane.step.Name()))
		lane.evidenceDir = sctx.EvidenceDir
	}
	sctx.Log = lane.log.line
	sctx.LogChunk = lane.log.write
	sctx.LogFile = lane.log.line

	outcome, err := lane.step.Execute(sctx)
	if err != nil {
		return nil, err
	}
	if outcome == nil {
		return nil, errors.New("the step returned no outcome")
	}
	if lane.finalHead, err = stepGitHeadSHA(sctx); err != nil {
		return nil, fmt.Errorf("read the side lane head: %w", err)
	}
	if lane.leftover, err = stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames"); err != nil {
		return nil, fmt.Errorf("read the side lane status: %w", err)
	}
	if lane.finalHead != lane.head {
		changed, err := stepGitRunRaw(sctx, "diff", "--name-only", "-z", "--no-renames", lane.head, lane.finalHead)
		if err != nil {
			return nil, fmt.Errorf("list the side lane commit's paths: %w", err)
		}
		lane.changed = changedPathList(changed)
		if _, err := stepGitRun(sctx, "branch", "-f", sideLaneBranch, lane.finalHead); err != nil {
			return nil, fmt.Errorf("name the side lane commit: %w", err)
		}
	}
	return outcome, nil
}

// discard stops a lane and removes what it wrote. It is safe on a lane that
// is still running, and it waits for the lane to stop before it removes
// anything the lane's processes may still hold.
func (lane *sideLane) discard() {
	lane.cancel(errSideLaneStopped)
	<-lane.done
	lane.removeClone()
	if lane.evidenceDir != "" {
		_ = os.RemoveAll(lane.evidenceDir)
	}
}

// removeClone removes the lane's clone. A file a just-ended process still
// holds can refuse deletion for a moment on Windows, so it retries briefly
// and otherwise leaves the directory to the system's temp cleanup.
func (lane *sideLane) removeClone() {
	if lane.scratch == "" {
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.RemoveAll(lane.scratch); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// takeSideLane returns the result of name's work started beside the review
// when that work is still what the step would do now. Otherwise it discards
// the lane, says why, and returns false, and the caller runs the step as it
// always has.
func takeSideLane(sctx *pipeline.StepContext, name types.StepName) (*pipeline.StepOutcome, bool) {
	if sctx.SideLane || sctx.Shared == nil {
		return nil, false
	}
	lanes, _ := sctx.Shared.SideLanes().(*sideLanes)
	if lanes == nil {
		return nil, false
	}
	lane := lanes.claim(name)
	if lane == nil {
		return nil, false
	}
	on := shortObjectID(lane.head)
	refuse := func(reason string) (*pipeline.StepOutcome, bool) {
		lane.discard()
		if text := strings.TrimSpace(lane.log.String()); text != "" {
			sctx.LogFile(fmt.Sprintf("side by side: log of the discarded %s work on %s:\n%s", name, on, text))
		}
		sctx.Log(fmt.Sprintf("side by side: the %s work started beside the review on %s is not used: %s. The step runs now, as it does without side by side.", name, on, reason))
		return nil, false
	}
	// A fix round answers findings of this run's worktree. Work done on the
	// commit before those findings existed says nothing about it.
	if sctx.Fixing {
		return refuse("this is a fix round")
	}
	select {
	case <-lane.done:
	default:
		sctx.Log(fmt.Sprintf("side by side: waiting for the %s work started beside the review on %s", name, on))
		select {
		case <-lane.done:
		case <-sctx.Ctx.Done():
			return refuse("the run was stopped")
		}
	}
	if reason := lane.adopt(sctx); reason != "" {
		return refuse(reason)
	}
	lane.removeClone()
	if text := strings.TrimSpace(lane.log.String()); text != "" {
		sctx.LogFile(fmt.Sprintf("side by side: log of the %s work on %s:\n%s", name, on, text))
	}
	sctx.Log(fmt.Sprintf("side by side: took the %s result of the work started beside the review on %s, which is still the run's head. It ran for %s there.", name, on, lane.elapsed.Round(time.Second)))
	return lane.outcome, true
}

// adopt brings a finished lane's result into the run, or returns why it
// cannot be used. It changes the run's worktree only when it returns "".
func (lane *sideLane) adopt(sctx *pipeline.StepContext) string {
	name := lane.step.Name()
	if lane.err != nil {
		return fmt.Sprintf("it failed (%v)", lane.err)
	}
	if lane.outcome == nil {
		return "it produced no result"
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Sprintf("the worktree head could not be read (%v)", err)
	}
	if head != sctx.Run.HeadSHA {
		return fmt.Sprintf("the worktree is at %s and the run's recorded head is %s", shortObjectID(head), shortObjectID(sctx.Run.HeadSHA))
	}
	if head != lane.head {
		return fmt.Sprintf("the run's head has moved to %s since", shortObjectID(head))
	}
	status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return fmt.Sprintf("the worktree status could not be read (%v)", err)
	}
	dirty := porcelainZPaths(status)

	switch name {
	case types.StepTest:
		// What the Test step leaves uncommitted is brought over into a clean
		// worktree only, so nothing of the run's is ever written over.
		if len(dirty) > 0 {
			return "the run's worktree holds uncommitted changes: " + strings.Join(dirty, ", ")
		}
		if lane.finalHead != lane.head {
			return fmt.Sprintf("it left a commit (%s) that no step validated", shortObjectID(lane.finalHead))
		}
		if err := lane.bringLeftovers(sctx); err != nil {
			return fmt.Sprintf("what it left in its copy could not be brought over (%v)", err)
		}
		if _, passed, recorded := lane.shared.Baseline(); recorded {
			sctx.Shared.RecordBaseline(lane.head, passed)
		}
	case types.StepDocument:
		if strings.TrimSpace(lane.leftover) != "" {
			return "it left uncommitted changes in its copy"
		}
		if lane.finalHead != lane.head {
			// The files a Test step leaves uncommitted, its new tests among
			// them, may stay where they are: a later step commits them, and
			// the push answers for them. A commit that changes one of them is
			// refused, so nothing uncommitted is ever written over.
			if overlap := sharedPaths(lane.changed, dirty); len(overlap) > 0 {
				return "its commit changes paths the run's worktree holds uncommitted changes to: " + strings.Join(overlap, ", ")
			}
			if err := lane.bringCommit(sctx); err != nil {
				return fmt.Sprintf("its commit could not be brought over (%v)", err)
			}
		}
	default:
		return "no step takes this lane"
	}
	return ""
}

// porcelainZPaths returns the paths of NUL-delimited porcelain v1 status
// entries written with --no-renames, so each entry names one path.
func porcelainZPaths(status string) []string {
	var paths []string
	for _, item := range strings.Split(status, "\x00") {
		if len(item) > 3 {
			paths = append(paths, item[3:])
		}
	}
	return paths
}

// sharedPaths returns the paths of a that b also names.
func sharedPaths(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, path := range b {
		inB[path] = true
	}
	var shared []string
	for _, path := range a {
		if inB[path] {
			shared = append(shared, path)
		}
	}
	return shared
}

// bringLeftovers copies the uncommitted files a Test lane left in its clone
// (the test files its evidence turn wrote) into the run's worktree, where the
// same step would have left them. Only new files and edits to tracked files
// are brought. Anything else refuses the lane, and a failure part way puts
// the worktree back.
func (lane *sideLane) bringLeftovers(sctx *pipeline.StepContext) (err error) {
	type change struct {
		rel string
		new bool
	}
	var changes []change
	for _, item := range strings.Split(strings.TrimSuffix(lane.leftover, "\x00"), "\x00") {
		if item == "" {
			continue
		}
		if len(item) < 4 || item[2] != ' ' {
			return fmt.Errorf("invalid git status entry %q", item)
		}
		code, path := item[:2], item[3:]
		rel := filepath.FromSlash(path)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("path %q is outside the worktree", path)
		}
		switch code {
		case "??":
			changes = append(changes, change{rel: rel, new: true})
		case " M":
			changes = append(changes, change{rel: rel})
		default:
			return fmt.Errorf("change %q to %s is neither a new file nor an edit", code, path)
		}
	}
	var brought []change
	defer func() {
		if err == nil {
			return
		}
		for _, c := range brought {
			if c.new {
				_ = os.Remove(filepath.Join(sctx.WorkDir, c.rel))
			} else {
				_, _ = stepGitRun(sctx, "checkout", "HEAD", "--", filepath.ToSlash(c.rel))
			}
		}
	}()
	for _, c := range changes {
		source := filepath.Join(lane.checkout, c.rel)
		target := filepath.Join(sctx.WorkDir, c.rel)
		info, statErr := os.Stat(source)
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", c.rel)
		}
		if _, statErr := os.Lstat(target); c.new && statErr == nil {
			return fmt.Errorf("%s already exists in the run's worktree", c.rel)
		}
		data, readErr := os.ReadFile(source)
		if readErr != nil {
			return readErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return mkErr
		}
		brought = append(brought, c)
		if writeErr := os.WriteFile(target, data, info.Mode().Perm()); writeErr != nil {
			return writeErr
		}
	}
	return nil
}

// bringCommit fast-forwards the run's worktree to the commit a documentation
// lane made on the run's head, and records it exactly as the documentation
// step records its own commit.
func (lane *sideLane) bringCommit(sctx *pipeline.StepContext) error {
	if _, err := stepGitRun(sctx, "fetch", "--quiet", "--no-tags", lane.checkout, sideLaneBranch); err != nil {
		return fmt.Errorf("fetch the commit: %w", err)
	}
	fetched, err := stepGitRun(sctx, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve the fetched commit: %w", err)
	}
	if fetched != lane.finalHead {
		return fmt.Errorf("fetched %s, want %s", shortObjectID(fetched), shortObjectID(lane.finalHead))
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", lane.head, lane.finalHead); err != nil {
		return fmt.Errorf("%s does not descend from %s", shortObjectID(lane.finalHead), shortObjectID(lane.head))
	}
	if _, err := stepGitRun(sctx, "merge", "--ff-only", "--quiet", lane.finalHead); err != nil {
		return fmt.Errorf("fast-forward to %s: %w", shortObjectID(lane.finalHead), err)
	}
	return recordAgentFixHead(sctx, types.StepDocument, lane.finalHead)
}

// isDocumentationPath reports whether path is documentation for this run's
// repository: its trusted test.documentation_paths, else the built-in list.
func isDocumentationPath(sctx *pipeline.StepContext, path string) bool {
	for _, pattern := range sctx.Config.Test.DocumentationPatterns() {
		if matchIgnorePattern(path, pattern) {
			return true
		}
	}
	return false
}

// baselineBeforePush makes the Push step answer for every commit that came
// after the one the Test step's baseline ran on. Under side_by_side that
// commit is the one the review started on, and the documentation pass, the
// lint step and the formatter may all have added commits since.
//
// It always says which commits those are. When they touch more than
// documentation, and a baseline is configured and passed on the tested
// commit, it runs the baseline again on head. A baseline that does not pass
// there stops the push: the run goes back to the Test step once, which owns
// failing tests and their fix rounds, and a second failure fails the run with
// nothing pushed.
//
// It returns the note to record on the step, and whether to go back to Test.
func baselineBeforePush(sctx *pipeline.StepContext, head string) (note string, restart bool, err error) {
	if sctx.Config == nil || !sctx.Config.SideBySide {
		return "", false, nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return "", false, fmt.Errorf("load step results before push: %w", err)
	}
	testRan := false
	for _, step := range steps {
		if step.StepName == types.StepTest && step.Status != types.StepStatusSkipped && step.Status != types.StepStatusPending {
			testRan = true
		}
	}
	if !testRan {
		sctx.Log("side by side: the Test step did not run in this run, so there is no tested commit to compare the pushed head with")
		return "", false, nil
	}

	testCmd := strings.TrimSpace(sctx.Config.Commands.Test)
	hasBaseline := testCmd != "" || len(sctx.Config.CommandOverrides["test"].Additional) > 0
	tested, passed, recorded := sctx.Shared.Baseline()
	if recorded && tested == head {
		sctx.Log(fmt.Sprintf("side by side: pushing %s, the commit the Test step ran on", shortObjectID(head)))
		return "", false, nil
	}

	documentationOnly := false
	if !recorded {
		note = fmt.Sprintf("The commit the Test step ran on is not known to this daemon, so %s is treated as untested.", shortObjectID(head))
	} else {
		commits, files, known := commitsAfter(sctx, tested, head)
		if !known {
			note = fmt.Sprintf("The head to push, %s, does not descend from %s, the commit the Test step ran on.", shortObjectID(head), shortObjectID(tested))
		} else {
			documentationOnly = true
			var other []string
			for _, file := range files {
				if !isDocumentationPath(sctx, file) {
					documentationOnly = false
					other = append(other, file)
				}
			}
			note = fmt.Sprintf("%d commit(s) came after %s, the commit the Test step ran on: %s.", len(commits), shortObjectID(tested), strings.Join(commits, "; "))
			if documentationOnly {
				note += " They touch documentation only, so the baseline test command was not run again."
			} else {
				note += " They touch more than documentation: " + strings.Join(other[:min(len(other), pushGuardCommitLimit)], ", ") + "."
			}
		}
	}
	sctx.Log("side by side: " + note)
	if documentationOnly {
		return note, false, nil
	}
	if !hasBaseline {
		note += " No baseline test command is configured, so nothing was run again."
		sctx.Log("side by side: no baseline test command is configured, so nothing is run again")
		return note, false, nil
	}
	if recorded && !passed {
		note += " The baseline did not pass on the tested commit and the Test step was completed over it, so it was not run again."
		sctx.Log("side by side: the baseline did not pass on the tested commit and the Test step was completed over it, so it is not run again here")
		return note, false, nil
	}

	if err := ensurePrepared(sctx, types.StepPush); err != nil {
		return "", false, fmt.Errorf("prepare test dependencies before push: %w", err)
	}
	declareCommandOverrides(sctx, "test")
	sctx.Log(fmt.Sprintf("side by side: running the baseline test command again on %s, the head to push", shortObjectID(head)))
	output, results, err := runConfiguredChecks(sctx, "test", testCmd)
	if err != nil {
		logConfiguredCommandOutput(sctx, output, types.StepPush)
		return "", false, fmt.Errorf("run test command before push: %w", err)
	}
	logConfiguredCommandOutput(sctx, output, types.StepPush)
	if len(noTurnChecks(results)) == 0 && len(failedChecks(results)) == 0 {
		sctx.Shared.RecordBaseline(head, true)
		sctx.Log(fmt.Sprintf("side by side: the baseline passed on %s", shortObjectID(head)))
		return note + " The baseline test command was run again on the pushed head and passed.", false, nil
	}
	sctx.Shared.RecordBaseline(head, false)
	if sctx.Shared.CountPushGuardRestart() > 1 {
		return "", false, fmt.Errorf("the baseline test command did not pass on %s, the head to push, for the second time in this run; nothing was pushed", shortObjectID(head))
	}
	sctx.Log(fmt.Sprintf("side by side: the baseline did not pass on %s, so nothing is pushed and the run goes back to the Test step", shortObjectID(head)))
	return note, true, nil
}

// commitsAfter lists the commits of tested..head, oldest first and bounded,
// and every path they change together. known is false when tested is not an
// ancestor of head, where "the commits after" has no meaning.
func commitsAfter(sctx *pipeline.StepContext, tested, head string) (commits, files []string, known bool) {
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", tested, head); err != nil {
		return nil, nil, false
	}
	log, err := stepGitRun(sctx, "log", "--reverse", "--format=%h %s", tested+".."+head)
	if err != nil {
		return nil, nil, false
	}
	for _, line := range strings.Split(log, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			commits = append(commits, line)
		}
	}
	if len(commits) > pushGuardCommitLimit {
		more := len(commits) - pushGuardCommitLimit
		commits = append(commits[:pushGuardCommitLimit], fmt.Sprintf("and %d more", more))
	}
	changed, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "-z", "--no-renames", tested, head)
	if err != nil {
		return nil, nil, false
	}
	return commits, changedPathList(changed), true
}
