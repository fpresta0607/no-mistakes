package branchsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

type TerminalSource struct {
	Head         string
	Tree         string
	GateHead     string
	RecoveryHead string
	ManagedDir   string
	Missing      bool
}

// TerminalSource selects committed source from this run's own managed checkout
// or exact recovery anchor. It supplies no review or terminal verification.
func (s *Service) TerminalSource(ctx context.Context, run *db.Run) (*TerminalSource, error) {
	return s.terminalSource(ctx, run, nil, nil)
}

func (s *Service) AcceptedTerminalSource(ctx context.Context, event *db.TerminalAdoption, current *db.Run) (*TerminalSource, error) {
	if event == nil || current == nil || current.ID != event.Snapshot.Run.ID || current.HeadSHA != event.CandidateSHA || event.SourceRef != "refs/no-mistakes/terminal-candidate/"+current.ID {
		return nil, fmt.Errorf("accepted terminal source identity changed")
	}
	if symbolic, err := git.Run(ctx, s.GateDir, "symbolic-ref", "-q", event.SourceRef); err == nil && symbolic != "" {
		return nil, fmt.Errorf("accepted terminal source anchor is symbolic")
	}
	head, exists, err := git.ExactRefTarget(ctx, s.GateDir, event.SourceRef)
	if err != nil || !exists || head != event.CandidateSHA || !objectExists(ctx, s.GateDir, head) {
		return nil, fmt.Errorf("accepted terminal source anchor changed")
	}
	return s.terminalSource(ctx, &event.Snapshot.Run, event, current)
}

func (s *Service) terminalSource(ctx context.Context, run *db.Run, event *db.TerminalAdoption, current *db.Run) (*TerminalSource, error) {
	if s.Paths == nil || s.Repo == nil || run == nil || run.RepoID != s.Repo.ID || !run.Status.Terminal() || run.Status == types.RunCompleted || run.CustodyReturnedAt != nil || run.PushActive {
		return nil, fmt.Errorf("source adoption requires the selected unrecovered terminal run")
	}
	branch, err := git.CurrentBranch(ctx, s.workDir())
	if err != nil || branch != run.Branch {
		return nil, fmt.Errorf("terminal source belongs to another branch")
	}
	callerHead, err := git.HeadSHA(ctx, s.workDir())
	if err != nil || callerHead != run.HeadSHA && callerHead != ptr(run.SubmittedHeadSHA) && callerHead != ptr(run.ReviewApprovedHeadSHA) && callerHead != ptr(run.LastPushedSHA) {
		return nil, fmt.Errorf("caller head is not bound to the original run")
	}
	gateRef := "refs/heads/" + run.Branch
	if symbolic, err := git.Run(ctx, s.GateDir, "symbolic-ref", "-q", gateRef); err == nil && symbolic != "" {
		return nil, fmt.Errorf("gate branch is symbolic")
	}
	gateHead, exists, err := git.ExactRefTarget(ctx, s.GateDir, gateRef)
	if err != nil || !exists || !objectExists(ctx, s.GateDir, gateHead) {
		return nil, fmt.Errorf("gate branch is missing or unreadable")
	}
	compatible, err := recoveryAnchorCompatible(ctx, s.GateDir, run.ID, run.HeadSHA)
	if event != nil && event.RecoveryHead == "" && err == nil && !compatible {
		compatible, err = recoveryAnchorCompatible(ctx, s.GateDir, run.ID, event.CandidateSHA)
	}
	if err != nil || !compatible {
		return nil, fmt.Errorf("run recovery anchor contradicts the recorded head")
	}
	recoveryHead, _, err := git.ExactRefTarget(ctx, s.GateDir, custody.RecoveryRef(run.ID))
	if err != nil {
		return nil, err
	}
	if event != nil && event.RecoveryHead != "" && recoveryHead != event.RecoveryHead {
		return nil, fmt.Errorf("original recovery anchor changed")
	}
	managed := worktrees.RecordedDir(s.Paths, run.WorktreePath(), run.RepoID, run.ID)
	source := &TerminalSource{GateHead: gateHead, RecoveryHead: recoveryHead, ManagedDir: managed}
	if _, err := os.Stat(managed); os.IsNotExist(err) {
		observed, expectedHead := run.TerminalSourceState, run.HeadSHA
		if event != nil {
			observed, expectedHead = current.TerminalSourceState, event.CandidateSHA
		}
		if observed == nil || *observed != "clean:"+expectedHead {
			return nil, fmt.Errorf("missing managed source has no bound clean-source observation; missing dirty content cannot be reconstructed")
		}
		source.Missing = true
		if event == nil {
			anchor := custody.RecoveryRef(run.ID)
			head, exists, err := git.ExactRefTarget(ctx, s.GateDir, anchor)
			if err != nil || !exists || head != run.HeadSHA || run.TerminalHeadVerifiedAt == nil || !objectExists(ctx, s.GateDir, head) {
				return nil, fmt.Errorf("missing managed source has no exact verified recovery anchor; missing dirty content cannot be reconstructed")
			}
		}
		source.Head = expectedHead
	} else {
		if err != nil {
			return nil, fmt.Errorf("managed source is unreadable: %w", err)
		}
		common, err := git.Run(ctx, managed, "rev-parse", "--git-common-dir")
		if !filepath.IsAbs(common) {
			common = filepath.Join(managed, common)
		}
		if err != nil || worktrees.Canonical(common) != worktrees.Canonical(s.GateDir) {
			return nil, fmt.Errorf("managed source does not belong to this run's gate")
		}
		clean, reason := worktreeClean(ctx, managed)
		if !clean {
			return nil, fmt.Errorf("managed source is %s; preserve its tracked, staged and untracked content before adoption", reason)
		}
		head, err := git.HeadSHA(ctx, managed)
		if err != nil || head != run.HeadSHA && !isAncestor(ctx, managed, run.HeadSHA, head) {
			return nil, fmt.Errorf("managed source is not bound to the recorded history")
		}
		source.Head = head
	}
	tree, err := git.Run(ctx, s.GateDir, "rev-parse", source.Head+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("candidate content cannot be read")
	}
	source.Tree = tree
	if event != nil && (source.Head != event.CandidateSHA || source.Tree != event.CandidateTree || source.GateHead != event.GateHead || source.ManagedDir != event.ManagedDir) {
		return nil, fmt.Errorf("accepted terminal source content or lane changed")
	}
	return source, nil
}
