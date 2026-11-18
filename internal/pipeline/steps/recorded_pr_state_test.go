package steps

import (
	"context"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
)

// TestObserveTerminalPRStateProvesTheMergeAtTheLastPublishedHead runs on a
// forge whose merged proof names the PR's head. A validation-only authored
// child inherits its predecessor's published head as last_pushed_sha, and its
// own HeadSHA is a candidate that may never have been pushed, so the PR head
// the merge proves is the last published one. Proving it against HeadSHA, as
// the CI step does for its own push, would keep the lineage locked for good.
// A merge proven at any other head is reported, and nothing is recorded.
func TestObserveTerminalPRStateProvesTheMergeAtTheLastPublishedHead(t *testing.T) {
	t.Parallel()
	const (
		prURL         = "https://git.example.com/team/repo/pulls/1"
		publishedHead = "1111111111111111111111111111111111111111"
		unrelatedHead = "2222222222222222222222222222222222222222"
		candidateHead = "candidate"
	)
	for _, test := range []struct {
		name          string
		lastPushedSHA string
		mergedHead    string
		wantRecorded  string
		wantElsewhere bool
	}{
		{name: "merged at the head the predecessor published", lastPushedSHA: publishedHead, mergedHead: publishedHead, wantRecorded: "merged"},
		{name: "merged at the candidate head the child never published", lastPushedSHA: publishedHead, mergedHead: candidateHead, wantRecorded: "open", wantElsewhere: true},
		{name: "merged at a head that matches neither", lastPushedSHA: publishedHead, mergedHead: unrelatedHead, wantRecorded: "open", wantElsewhere: true},
		{name: "merged at its own head when nothing was published before it", mergedHead: candidateHead, wantRecorded: "merged"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			mergedHead := test.mergedHead
			if mergedHead == candidateHead {
				mergedHead = headSHA
			}
			fakeProviderPlugin(t, sctx, fakeplugin.State{PRs: []fakeplugin.PR{{Number: "1", URL: prURL, HeadBranch: "feature", BaseBranch: "main", HeadSHA: mergedHead, State: "merged"}}})
			if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "open"); err != nil {
				t.Fatal(err)
			}
			url := prURL
			sctx.Run.PRURL = &url
			if test.lastPushedSHA != "" {
				lastPushed := test.lastPushedSHA
				sctx.Run.LastPushedSHA = &lastPushed
			}

			// Act
			isMergedAtAnotherHead, err := ObserveTerminalPRState(sctx)

			// Assert
			if err != nil {
				t.Fatalf("ObserveTerminalPRState() error = %v", err)
			}
			if isMergedAtAnotherHead != test.wantElsewhere {
				t.Fatalf("ObserveTerminalPRState() merged at another head = %v, want %v", isMergedAtAnotherHead, test.wantElsewhere)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.PRState == nil || *run.PRState != test.wantRecorded {
				t.Fatalf("recorded PR state = %v, want %q", run.PRState, test.wantRecorded)
			}
		})
	}
}

// TestProveMergeEndsOnlyOnAValidatedProofAtTheReportedHead covers a merged PR
// whose proof does not name the head the run published. The lineage may end
// only when the forge proves the merge at the head it reports instead (merged,
// the same PR); a proof that says the PR is not merged, one for another PR,
// or a head change that names no head proves nothing and fails closed.
func TestProveMergeEndsOnlyOnAValidatedProofAtTheReportedHead(t *testing.T) {
	t.Parallel()
	const (
		publishedHead = "1111111111111111111111111111111111111111"
		movedHead     = "2222222222222222222222222222222222222222"
	)
	pr := &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}
	merged := scm.MergedProof{Merged: true, Number: "42", URL: pr.URL, MergeCommitSHA: "3333333333333333333333333333333333333333"}
	for _, test := range []struct {
		name          string
		forgeProof    scm.MergedProof
		forgeErr      error
		wantElsewhere bool
		wantErr       bool
	}{
		{name: "merged at the published head", forgeProof: withHead(merged, publishedHead)},
		{name: "merged at the head the forge reports", forgeProof: withHead(merged, movedHead), wantElsewhere: true},
		{name: "proof at the reported head says not merged", forgeProof: scm.MergedProof{Number: "42", URL: pr.URL, HeadSHA: movedHead}, wantErr: true},
		{name: "proof at the reported head names another PR", forgeProof: scm.MergedProof{Merged: true, Number: "43", URL: pr.URL, HeadSHA: movedHead}, wantErr: true},
		{name: "head change that names no head", forgeErr: fmt.Errorf("provider plugin: %w", scm.ErrHeadChanged), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			host := &reportedHeadProofHost{proof: test.forgeProof, err: test.forgeErr}

			// Act
			isMergedAtAnotherHead, err := proveMerge(context.Background(), host, pr, publishedHead)

			// Assert
			if (err != nil) != test.wantErr {
				t.Fatalf("proveMerge() error = %v, want error %v", err, test.wantErr)
			}
			if isMergedAtAnotherHead != test.wantElsewhere {
				t.Fatalf("proveMerge() merged at another head = %v, want %v", isMergedAtAnotherHead, test.wantElsewhere)
			}
		})
	}
}

func withHead(proof scm.MergedProof, head string) scm.MergedProof {
	proof.HeadSHA = head
	return proof
}

// reportedHeadProofHost answers a merged-proof request the way the
// proof-capable providers do: a proof for another head is refused with the
// head it names.
type reportedHeadProofHost struct {
	scm.Host
	proof scm.MergedProof
	err   error
}

func (h *reportedHeadProofHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergedProof: true}
}

func (h *reportedHeadProofHost) GetMergedProof(_ context.Context, _ *scm.PR, expectedHead string) (scm.MergedProof, error) {
	if h.err != nil {
		return scm.MergedProof{}, h.err
	}
	if h.proof.HeadSHA != expectedHead {
		return scm.MergedProof{}, &scm.HeadChangedError{Expected: expectedHead, Reported: h.proof.HeadSHA}
	}
	return h.proof, nil
}
