package branchsync

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalValidationAnchorsReadEveryKnownAnchorWithoutChangingRefs(t *testing.T) {
	t.Parallel()
	f := newRecoverFixture(t, types.RunFailed)
	mustRun(t, f.local, "fetch", "--no-tags", "--no-write-fetch-head", f.gate, f.preserved)
	ref := custody.RecoveryRef(f.run.ID)
	for _, directory := range []string{f.gate, f.local} {
		mustRun(t, directory, "update-ref", ref, f.preserved)
		mustRun(t, directory, "update-ref", "refs/no-mistakes/recover-local/"+f.run.ID, f.submitted)
		mustRun(t, directory, "update-ref", "refs/no-mistakes/recover-rewritten/"+f.run.ID+"/1", f.preserved)
	}
	before := mustRun(t, f.gate, "show-ref")

	anchors, err := TerminalValidationAnchors(f.ctx, f.db, f.gate, f.local, f.run.ID)

	expected := map[string]string{}
	for _, name := range []string{"gate", "caller"} {
		expected[name+"/"+ref] = f.preserved
		expected[name+"/refs/no-mistakes/recover-local/"+f.run.ID] = f.submitted
		expected[name+"/refs/no-mistakes/recover-rewritten/"+f.run.ID+"/1"] = f.preserved
	}
	if err != nil || !reflect.DeepEqual(anchors, expected) {
		t.Fatalf("anchors = %+v %v", anchors, err)
	}
	if before != mustRun(t, f.gate, "show-ref") {
		t.Fatal("observation changed a recovery ref")
	}
}
