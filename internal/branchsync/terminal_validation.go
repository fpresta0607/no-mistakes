package branchsync

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// TerminalValidationAnchors observes recovery evidence without recovering or
// reconstructing historical source. Every observed anchor must remain exact.
func TerminalValidationAnchors(ctx context.Context, database *db.DB, gateDir, callerDir, runID string) (map[string]string, error) {
	history, err := database.ValidationHistory(runID)
	if err != nil {
		return nil, err
	}
	bound := make(map[string]string)
	bind := func(key, head string) error {
		if previous, exists := bound[key]; exists && previous != head {
			return fmt.Errorf("terminal recovery anchor has contradictory bindings: %s", key)
		}
		if head == "" {
			return fmt.Errorf("terminal recovery anchor binding is missing: %s", key)
		}
		bound[key] = head
		return nil
	}
	for _, entry := range history {
		if adoption := entry.Adoption; adoption != nil {
			if adoption.SourceRef != "refs/no-mistakes/terminal-candidate/"+adoption.SuccessorID {
				return nil, fmt.Errorf("terminal source anchor identity changed")
			}
			for key, head := range map[string]string{"gate/" + adoption.SourceRef: adoption.CandidateSHA, "gate/refs/no-mistakes/terminal-lane/" + adoption.SuccessorID: adoption.GateHead} {
				if err := bind(key, head); err != nil {
					return nil, err
				}
			}
			if adoption.RecoveryHead != "" {
				if err := bind("gate/"+custody.RecoveryRef(adoption.Snapshot.Run.ID), adoption.RecoveryHead); err != nil {
					return nil, err
				}
			}
		}
		if admission := entry.Admission; admission != nil {
			if err := bind("gate/refs/no-mistakes/authored-candidate/"+admission.ID, admission.Request.CandidateSHA); err != nil {
				return nil, err
			}
			for key, head := range admission.Request.SourceAnchors {
				if err := bind(key, head); err != nil {
					return nil, err
				}
			}
		}
		ancestor := entry.Snapshot.Run
		records, err := database.GetRecoveryArchivesByRun(ancestor.ID)
		if err != nil {
			return nil, err
		}
		if len(records) > 1 {
			return nil, fmt.Errorf("terminal recovery archive bindings are ambiguous")
		}
		for _, record := range records {
			archiveRun := ancestor
			archiveRun.SubmittedHeadSHA, err = database.CustodySubmittedHead(&ancestor)
			if err != nil {
				return nil, err
			}
			_, isRequiredHeadBound := archiveRequiredHead(State{Local: LocalState{Head: record.RequiredHeadSHA}}, &archiveRun)
			if record.OwnerRunID != ancestor.ID || record.RunID != ancestor.ID || record.RepoID != ancestor.RepoID || record.Branch != ancestor.Branch || record.PreservedHeadSHA != ancestor.HeadSHA || !isRequiredHeadBound || !strings.HasPrefix(record.ArchiveRef, "refs/heads/archive/") {
				return nil, fmt.Errorf("terminal recovery archive identity contradicts the selected history")
			}
			if err := bind("caller/"+record.ArchiveRef, record.PreservedHeadSHA); err != nil {
				return nil, err
			}
			if err := bind("gate/"+custody.RecoveryRef(ancestor.ID), ancestor.HeadSHA); err != nil {
				return nil, err
			}
		}
	}
	anchors := make(map[string]string)
	for _, repository := range []struct{ name, directory string }{{"gate", gateDir}, {"caller", callerDir}} {
		output, err := git.Run(ctx, repository.directory, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/")
		if err != nil {
			return nil, fmt.Errorf("read terminal recovery anchors: %w", err)
		}
		refs := make(map[string]bool)
		for _, entry := range history {
			ancestor := entry.Snapshot.Run
			provenanceRefs := make(map[string]string)
			if adoption := entry.Adoption; adoption != nil {
				provenanceRefs[adoption.SourceRef] = adoption.CandidateSHA
				provenanceRefs["refs/no-mistakes/terminal-lane/"+adoption.SuccessorID] = adoption.GateHead
			}
			if admission := entry.Admission; admission != nil {
				provenanceRefs["refs/no-mistakes/authored-candidate/"+admission.ID] = admission.Request.CandidateSHA
			}
			for _, ref := range strings.Fields(output) {
				if head, exists := provenanceRefs[ref]; exists {
					refs[ref] = true
					if err := bind(repository.name+"/"+ref, head); err != nil {
						return nil, err
					}
				}
				if strings.HasPrefix(ref, "refs/no-mistakes/recover") && (strings.HasSuffix(ref, "/"+ancestor.ID) || strings.Contains(ref, "/"+ancestor.ID+"/")) {
					refs[ref] = true
					if ref == custody.RecoveryRef(ancestor.ID) {
						if err := bind(repository.name+"/"+ref, ancestor.HeadSHA); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		for key := range bound {
			name, ref, _ := strings.Cut(key, "/")
			if name != "gate" && name != "caller" || !strings.HasPrefix(ref, "refs/") {
				return nil, fmt.Errorf("terminal recovery anchor identity is invalid: %s", key)
			}
			if name == repository.name {
				refs[ref] = true
			}
		}
		for ref := range refs {
			head, exists, err := git.DirectRefTarget(ctx, repository.directory, ref)
			if err != nil || !exists {
				return nil, fmt.Errorf("terminal recovery anchors changed: %s is missing or unreadable: %v", ref, err)
			}
			objectType, err := git.Run(ctx, repository.directory, "cat-file", "-t", head)
			key := repository.name + "/" + ref
			if err != nil || objectType != "commit" || bound[key] != "" && bound[key] != head {
				return nil, fmt.Errorf("terminal recovery anchor contradicts the selected history: %s", ref)
			}
			anchors[key] = head
		}
	}
	return anchors, nil
}
