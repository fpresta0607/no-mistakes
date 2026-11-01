package branchsync

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
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
	ref := "refs/no-mistakes/terminal-candidate/" + f.run.ID
	mustRun(t, f.gate, "update-ref", ref, candidate)
	event := &db.TerminalAdoption{Snapshot: db.TerminalSnapshot{Run: *f.run}, CandidateSHA: candidate, CandidateTree: source.Tree, SourceRef: ref, GateHead: source.GateHead, RecoveryHead: source.RecoveryHead, ManagedDir: managed}
	current := *f.run
	current.HeadSHA = candidate
	state := "clean:" + candidate
	current.TerminalSourceState = &state
	mustRun(t, f.gate, "worktree", "remove", managed)
	// Startup may append an ordinary preservation anchor when none existed.
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), candidate)
	replayed, err := f.service.AcceptedTerminalSource(f.ctx, event, &current)
	if err != nil || !replayed.Missing || replayed.Head != candidate || replayed.Tree != event.CandidateTree {
		t.Fatalf("replay = %+v %v", replayed, err)
	}
	state = "dirty:" + candidate
	if _, err := f.service.AcceptedTerminalSource(f.ctx, event, &current); err == nil {
		t.Fatal("missing dirty source was accepted")
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
