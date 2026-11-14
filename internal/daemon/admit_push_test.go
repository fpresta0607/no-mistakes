package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// seedAuthoredLineage records a validation-only authored child on branch whose
// lineage has not ended: the child failed and its PR was never merged or closed.
func seedAuthoredLineage(t *testing.T, database *db.DB, repoID, branch string) string {
	t.Helper()
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := db.RunIntent{Summary: "Validate the exact authored source", Source: db.RunIntentSourceAgent, Score: 1}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
	parent, err := database.InsertRunWithLaunchAssertion(repoID, branch, strings.Repeat("b", 40), strings.Repeat("c", 40), &intent, "original", "generation", digest, "", true, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(parent.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := database.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repoID, Branch: branch, CallerDir: t.TempDir(), CandidateSHA: strings.Repeat("d", 40), CandidateTree: strings.Repeat("e", 40), LaunchNonce: "original", ValidationGeneration: "generation", IntentDigest: digest,
		ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("f", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate this authored candidate"}
	event, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), Operator: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(event.ChildID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	return event.ChildID
}

func TestRefuseUnadmittableRefUpdates(t *testing.T) {
	zero, head := strings.Repeat("0", 40), strings.Repeat("1", 40)
	authored := func(old, new string) ipc.GateRefUpdate {
		return ipc.GateRefUpdate{Old: old, New: new, Ref: "refs/heads/authored"}
	}
	other := ipc.GateRefUpdate{Old: zero, New: head, Ref: "refs/heads/other"}
	for _, test := range []struct {
		name           string
		updates        []ipc.GateRefUpdate
		hasEndedPR     bool
		wantRefusalHas string
	}{
		{name: "no updates", wantRefusalHas: "cannot be inspected"},
		{name: "short object name", updates: []ipc.GateRefUpdate{{Old: zero, New: "1234567", Ref: "refs/heads/other"}}, wantRefusalHas: "malformed"},
		{name: "uppercase object name", updates: []ipc.GateRefUpdate{{Old: zero, New: strings.Repeat("A", 40), Ref: "refs/heads/other"}}, wantRefusalHas: "malformed"},
		{name: "ref outside refs", updates: []ipc.GateRefUpdate{{Old: zero, New: head, Ref: "other"}}, wantRefusalHas: "malformed"},
		{name: "ordinary update to an authored-lineage branch", updates: []ipc.GateRefUpdate{authored(strings.Repeat("b", 40), head)}, wantRefusalHas: "validation-only authored lineage"},
		{name: "deleting an authored-lineage branch", updates: []ipc.GateRefUpdate{authored(strings.Repeat("b", 40), zero)}, wantRefusalHas: "validation-only authored lineage"},
		{name: "one authored-lineage branch refuses the whole push", updates: []ipc.GateRefUpdate{other, authored(strings.Repeat("b", 40), head)}, wantRefusalHas: "refs/heads/authored"},
		{name: "another branch", updates: []ipc.GateRefUpdate{other}},
		{name: "a sha256 object name", updates: []ipc.GateRefUpdate{{Old: strings.Repeat("0", 64), New: strings.Repeat("2", 64), Ref: "refs/heads/other"}}},
		{name: "the lineage ended with its PR", updates: []ipc.GateRefUpdate{authored(strings.Repeat("b", 40), head)}, hasEndedPR: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, err := db.Open(filepath.Join(t.TempDir(), "no-mistakes.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repo, err := database.InsertRepoWithID("repo", t.TempDir(), "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			child := seedAuthoredLineage(t, database, repo.ID, "authored")
			if test.hasEndedPR {
				if err := database.UpdateRunPRState(child, "merged"); err != nil {
					t.Fatal(err)
				}
			}

			err = refuseUnadmittableRefUpdates(database, repo.ID, test.updates)

			if test.wantRefusalHas == "" && err != nil {
				t.Fatalf("admissible push refused: %v", err)
			}
			if test.wantRefusalHas != "" && (err == nil || !strings.Contains(err.Error(), test.wantRefusalHas)) {
				t.Fatalf("refusal = %v, want one naming %q", err, test.wantRefusalHas)
			}
		})
	}
}
