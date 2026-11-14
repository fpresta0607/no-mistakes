package daemon

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// refuseUnadmittableRefUpdates is the branch half of pre-receive admission. It
// runs before Git moves any ref, so a refusal leaves every gate ref of the push
// where it was. A branch whose validation-only authored lineage has not ended
// accepts no hook-mediated update at all: authored admission imports its
// candidate without moving the gate branch, and an ordinary push that moved it
// would leave the branch at a head the immutable lineage never bound, which
// the next authored admission refuses. Updates that cannot be read, or a
// lineage that cannot be inspected, refuse the whole push.
func refuseUnadmittableRefUpdates(database *db.DB, repoID string, updates []ipc.GateRefUpdate) error {
	if len(updates) == 0 {
		return fmt.Errorf("admission needs the proposed ref updates; a push whose updates cannot be inspected is refused")
	}
	for _, update := range updates {
		if !isObjectID(update.Old) || !isObjectID(update.New) || !strings.HasPrefix(update.Ref, "refs/") {
			return fmt.Errorf("malformed proposed ref update %q %q %q", update.Old, update.New, update.Ref)
		}
	}
	for _, update := range updates {
		if err := database.RefuseUnlinkedValidation(repoID, branchFromRef(update.Ref)); err != nil {
			return fmt.Errorf("refuse %s before it moves: %w", update.Ref, err)
		}
	}
	return nil
}

// isObjectID reports whether value is a full SHA-1 or SHA-256 object name.
func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
