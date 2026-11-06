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
func TerminalValidationAnchors(ctx context.Context, gateDir, callerDir string, run *db.Run) (map[string]string, error) {
	anchors := make(map[string]string)
	for _, repository := range []struct{ name, directory string }{{"gate", gateDir}, {"caller", callerDir}} {
		output, err := git.Run(ctx, repository.directory, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/")
		if err != nil {
			return nil, fmt.Errorf("read terminal recovery anchors: %w", err)
		}
		for _, ref := range strings.Fields(output) {
			if !strings.HasPrefix(ref, "refs/no-mistakes/recover") || !strings.HasSuffix(ref, "/"+run.ID) && !strings.Contains(ref, "/"+run.ID+"/") {
				continue
			}
			if symbolic, err := git.Run(ctx, repository.directory, "symbolic-ref", "-q", ref); err == nil && symbolic != "" {
				return nil, fmt.Errorf("terminal recovery anchor is symbolic: %s", ref)
			}
			head, exists, err := git.ExactRefTarget(ctx, repository.directory, ref)
			if err != nil || !exists {
				return nil, fmt.Errorf("terminal recovery anchor cannot be read: %s", ref)
			}
			commit, err := git.Run(ctx, repository.directory, "rev-parse", ref+"^{commit}")
			if err != nil || head != commit || ref == custody.RecoveryRef(run.ID) && head != run.HeadSHA {
				return nil, fmt.Errorf("terminal recovery anchor contradicts the selected history: %s", ref)
			}
			anchors[repository.name+"/"+ref] = head
		}
	}
	return anchors, nil
}
