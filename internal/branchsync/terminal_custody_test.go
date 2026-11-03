package branchsync

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAcceptedTerminalSourceReconstructsExactCandidateAfterCrash(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunFailed)
	f.service.Paths = paths.WithRoot(t.TempDir())
	managed := f.service.Paths.WorktreeDir(f.repo.ID, f.run.ID)
	mustRun(t, f.gate, "worktree", "add", "--detach", managed, f.preserved)
	mustRun(t, managed, "config", "user.name", "Fixture")
	mustRun(t, managed, "config", "user.email", "fixture@example.com")
	mustWrite(t, filepath.Join(managed, "candidate.txt"), "accepted correction\n")
	mustRun(t, managed, "add", "candidate.txt")
	mustRun(t, managed, "commit", "-m", "accepted correction")
	candidate := mustRun(t, managed, "rev-parse", "HEAD")
	source, err := f.service.TerminalSource(f.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/no-mistakes/terminal-candidate/successor"
	mustRun(t, f.gate, "update-ref", ref, candidate)
	event := &db.TerminalAdoption{Snapshot: db.ContinuationSnapshot{Run: *f.run}, CandidateSHA: candidate, RetainedSHA: candidate, CandidateTree: source.Tree, SourceRef: ref, GateHead: source.GateHead, RecoveryHead: source.RecoveryHead, ManagedDir: managed}
	current := *f.run
	current.ID = "successor"
	current.HeadSHA = candidate
	event.Successor = current
	state := "clean:" + candidate
	event.Snapshot.Run.TerminalSourceState = nil
	mustRun(t, f.gate, "worktree", "remove", managed)
	// Startup may append an ordinary preservation anchor when none existed.
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), candidate)
	replayed, err := f.service.AcceptedTerminalSource(f.ctx, event, &current)
	if err != nil || !replayed.Missing || replayed.Head != candidate || replayed.Tree != event.CandidateTree {
		t.Fatalf("replay = %+v %v", replayed, err)
	}
	state = "dirty:" + candidate
	event.Snapshot.Run.TerminalSourceState = &state
	if _, err := f.service.TerminalSource(f.ctx, &event.Snapshot.Run); err == nil {
		t.Fatal("missing dirty source was accepted before adoption")
	}
}

func TestTerminalSourceUsesOnlyBoundPreservedCommitWithoutManagedWorktree(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunFailed)
	f.service.Paths = paths.WithRoot(t.TempDir())
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.preserved)
	state := "clean:" + f.preserved
	f.run.TerminalSourceState = &state
	before := mustRun(t, f.gate, "show-ref")
	source, err := f.service.TerminalSource(f.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if source.Head != f.preserved || source.Tree != mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}") || source.ManagedDir != f.service.Paths.WorktreeDir(f.repo.ID, f.run.ID) {
		t.Fatalf("source = %+v", source)
	}
	if after := mustRun(t, f.gate, "show-ref"); before != after {
		t.Fatal("inspection mutated gate refs")
	}
	for _, observed := range []string{"", "unreadable", "dirty:" + f.preserved} {
		f.run.TerminalSourceState = &observed
		if _, err := f.service.TerminalSource(f.ctx, f.run); err == nil {
			t.Fatalf("missing source with observation %q was accepted", observed)
		}
	}
	f.run.TerminalSourceState = &state
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.submitted)
	if _, err := f.service.TerminalSource(f.ctx, f.run); err == nil {
		t.Fatal("contradictory recovery ref was accepted")
	}
}

func TestTerminalSourceRefusesDirtyOrUnboundManagedSource(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunCancelled)
	f.service.Paths = paths.WithRoot(t.TempDir())
	managed := f.service.Paths.WorktreeDir(f.repo.ID, f.run.ID)
	mustRun(t, f.gate, "worktree", "add", "--detach", managed, f.preserved)
	mustWrite(t, filepath.Join(managed, "untracked.txt"), "only copy\n")
	mustRun(t, managed, "config", "status.showUntrackedFiles", "no")
	if _, err := f.service.TerminalSource(f.ctx, f.run); err == nil {
		t.Fatal("dirty managed source was accepted as a committed candidate")
	}
	if content := mustRun(t, managed, "status", "--porcelain", "--untracked-files=all"); content == "" {
		t.Fatal("dirty source was removed")
	}
}

func TestTerminalSourceSelectsOnlyCleanCallerDescendantsOfRetainedReviewedOutput(t *testing.T) {
	for _, kind := range []string{"clean", "dirty", "unreviewed", "sibling", "missing dirty"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecoverFixture(t, types.RunCancelled)
			f.service.Paths = paths.WithRoot(t.TempDir())
			mustRun(t, f.local, "fetch", f.gate, f.preserved)
			base := f.preserved
			if kind == "sibling" {
				base = f.submitted
			}
			mustRun(t, f.local, "merge", "--ff-only", base)
			mustWrite(t, filepath.Join(f.local, "correction.txt"), "authored correction\n")
			mustRun(t, f.local, "add", "correction.txt")
			mustRun(t, f.local, "commit", "-m", "authored correction")
			candidate := mustRun(t, f.local, "rev-parse", "HEAD")
			f.run.ReviewApprovedHeadSHA = &f.preserved
			state := "clean:" + f.preserved
			if kind == "unreviewed" {
				f.run.ReviewApprovedHeadSHA = nil
			}
			if kind == "missing dirty" {
				state = "dirty:" + f.preserved
			}
			f.run.TerminalSourceState = &state
			mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.preserved)
			if kind == "dirty" {
				mustWrite(t, filepath.Join(f.local, "untracked.txt"), "only copy\n")
			}
			before := mustRun(t, f.gate, "show-ref")
			source, err := f.service.TerminalSource(f.ctx, f.run)
			if kind == "clean" {
				if err != nil || source.Head != candidate || source.RetainedHead != f.preserved || source.Tree != mustRun(t, f.local, "rev-parse", "HEAD^{tree}") {
					t.Fatalf("candidate=%+v err=%v", source, err)
				}
			} else if err == nil {
				t.Fatal("unbound or dirty candidate accepted")
			}
			if mustRun(t, f.gate, "show-ref") != before {
				t.Fatal("inspection mutated refs")
			}
		})
	}
}

func TestTerminalManagedCandidateMustContainRetainedPublicOutput(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	f.service.Paths = paths.WithRoot(t.TempDir())
	managed := f.service.Paths.WorktreeDir(f.repo.ID, f.run.ID)
	mustRun(t, f.gate, "worktree", "add", "--detach", managed, f.preserved)
	mustWrite(t, filepath.Join(f.local, "public-only.txt"), "published sibling\n")
	mustRun(t, f.local, "add", "public-only.txt")
	mustRun(t, f.local, "commit", "-m", "published sibling")
	public := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.gate, "fetch", f.local, public)
	f.run.LastPushedSHA = &public
	f.run.ReviewApprovedHeadSHA = &public
	mustRun(t, f.local, "checkout", "--detach", f.submitted)
	mustRun(t, f.local, "checkout", "-B", f.run.Branch, f.submitted)
	before := mustRun(t, f.gate, "show-ref")
	if _, err := f.service.TerminalSource(f.ctx, f.run); err == nil {
		t.Fatal("managed candidate omitted retained published output")
	}
	if mustRun(t, f.gate, "show-ref") != before {
		t.Fatal("refusal mutated gate refs")
	}
}

func TestRefreshHandsAnAdoptedPredecessorsBranchToItsSuccessor(t *testing.T) {
	cases := []struct {
		name              string
		callerAtCandidate bool
		keepLocal         bool
		wantState         string
		wantCandidateHead bool
	}{
		{name: "recover fast-forwards the operator to the owed candidate", wantState: StatePipelineOwned, wantCandidateHead: true},
		{name: "keep-local returns custody at the operator's own head", keepLocal: true, wantState: StatePipelineOwned},
		{name: "a caller that stood at the candidate already owns it", callerAtCandidate: true, wantState: StateUserOwned, wantCandidateHead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRecoverFixture(t, types.RunFailed)
			if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
				t.Fatal(err)
			}
			callerHead := f.submitted
			if tc.callerAtCandidate {
				mustRun(t, f.local, "fetch", f.gate, f.preserved)
				mustRun(t, f.local, "merge", "--ff-only", f.preserved)
				callerHead = f.preserved
			}
			profile := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
			assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {profile}, "reviewer": {profile}, "fixer": {profile}}}
			proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			predecessor, err := f.db.InsertRunWithLaunchAssertion(f.repo.ID, f.run.Branch, f.submitted, f.base, nil, "nonce", "generation", "intent", "", false, nil, assertion, proof)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunHeadSHA(predecessor.ID, f.preserved); err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunErrorStatusWithVerifiedHead(predecessor.ID, "interrupted", types.RunFailed, f.preserved); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.db.ReadContinuationSnapshot(predecessor.ID)
			if err != nil {
				t.Fatal(err)
			}
			adoption, _, err := f.db.AdoptTerminalCandidate(f.ctx, db.TerminalAdoption{Snapshot: *snapshot, CandidateSHA: f.preserved, CandidateTree: mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}"), SourceRef: "refs/no-mistakes/terminal-candidate/successor", CallerDir: f.local, CallerHead: callerHead, Reason: "validate preserved source", Operator: "fixture", Assertion: assertion, Proof: proof})
			if err != nil {
				t.Fatal(err)
			}
			// The successor ends unmoved at its candidate, the predecessor's pipeline-created head.
			if err := f.db.UpdateRunErrorStatusWithVerifiedHead(adoption.Successor.ID, types.RunCancelReasonAbortedByUser, types.RunCancelled, f.preserved); err != nil {
				t.Fatal(err)
			}

			refreshed := f.service.Refresh(f.ctx)
			recovered := f.service.Recover(f.ctx, tc.keepLocal)

			if refreshed.Pipeline.RunID != adoption.Successor.ID || refreshed.State != tc.wantState {
				t.Fatalf("refresh selected run %s as %s (%s), want the successor %s as %s", refreshed.Pipeline.RunID, refreshed.State, refreshed.Safety, adoption.Successor.ID, tc.wantState)
			}
			if recovered.Pipeline.RunID != adoption.Successor.ID || !recovered.Recovered {
				t.Fatalf("recover selected run %s (%s: %s), want custody of the successor %s returned", recovered.Pipeline.RunID, recovered.Safety, recovered.Error, adoption.Successor.ID)
			}
			wantHead := callerHead
			if tc.wantCandidateHead {
				wantHead = f.preserved
			}
			if head := mustRun(t, f.local, "rev-parse", "HEAD"); head != wantHead {
				t.Fatalf("operator branch head = %s, want %s", head, wantHead)
			}
		})
	}
}
