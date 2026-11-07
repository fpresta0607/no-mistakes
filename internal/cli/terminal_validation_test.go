package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalValidationHasOneGuardedMutationAndOneReadOnlyPreflight(t *testing.T) {
	root := newRootCmd()
	for _, test := range []struct {
		name         string
		shouldMutate bool
	}{{"validate-terminal", true}, {"terminal-preflight", false}} {
		command, _, err := root.Find([]string{"axi", test.name})
		if err != nil || command.CommandPath() != "no-mistakes axi "+test.name || mutatesPipelineControl(command) != test.shouldMutate {
			t.Fatalf("incorrect command guard for %s: %v", test.name, err)
		}
		for _, forbidden := range []string{"yes", "skip", "model", "effort", "run"} {
			if command.Flags().Lookup(forbidden) != nil {
				t.Fatalf("terminal admission exposes %s", forbidden)
			}
		}
	}
}

func TestTerminalPreflightRequiresEveryAdmissionCandidateAnchor(t *testing.T) {
	for _, lane := range []string{"authored", "terminal"} {
		t.Run(lane, func(t *testing.T) {
			directory, root, database, repo := setupAxiQueryRepo(t)
			run(t, directory, "git", "checkout", "-b", "feature")
			chdir(t, directory)
			head, err := git.HeadSHA(context.Background(), directory)
			if err != nil {
				t.Fatal(err)
			}
			selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
			assertion := &launchassert.Expectation{TrustedSHA: head, Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
			proof, err := assertion.Verify(head, assertion.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			intent := db.RunIntent{Summary: "validate retained candidate", Source: db.RunIntentSourceAgent, Score: 1}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
			parent, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", head, head, &intent, "source-launch", "source-generation", digest, "", true, nil, assertion, proof)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "candidate.txt"), []byte("retained correction\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run(t, directory, "git", "add", "candidate.txt")
			run(t, directory, "git", "commit", "-m", "test(cli): retain candidate")
			candidate, err := git.HeadSHA(context.Background(), directory)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := git.Run(context.Background(), directory, "rev-parse", "HEAD^{tree}")
			if err != nil {
				t.Fatal(err)
			}
			if lane == "terminal" {
				if err := database.UpdateRunHeadSHA(parent.ID, candidate); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.UpdateRunStatus(parent.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			snapshot, err := database.ReadTerminalSnapshot(parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repo.ID, Branch: "feature", CallerDir: directory, CandidateSHA: candidate, CandidateTree: tree, LaunchNonce: "source-launch", ValidationGeneration: "source-generation", IntentDigest: digest,
				ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("a", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "validate the retained candidate without renewed repairs"}
			if lane == "terminal" {
				request.TerminalGeneration, request.AcceptUnknownSource = "successor-generation", true
				fresh := *assertion
				fresh.Apply = true
				request.LaunchAssertion = &fresh
				proof, err = fresh.Verify(head, fresh.Profiles)
				if err != nil {
					t.Fatal(err)
				}
			}
			event, _, err := database.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: filepath.Dir(root.WorktreeDir(repo.ID, "pending")), Gates: "[]", Operator: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunErrorStatus(event.ChildID, "provider quota exhausted", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			gate := root.RepoDir(repo.ID)
			run(t, directory, "git", "clone", "--bare", directory, gate)
			ref := "refs/no-mistakes/authored-candidate/" + event.ID
			for _, change := range []string{"unchanged", "moved", "deleted"} {
				t.Run(change, func(t *testing.T) {
					run(t, gate, "git", "update-ref", ref, candidate)
					if change == "moved" {
						run(t, gate, "git", "update-ref", ref, head)
					} else if change == "deleted" {
						run(t, gate, "git", "update-ref", "-d", ref)
					}
					before, err := database.GetRunsByRepo(repo.ID)
					if err != nil {
						t.Fatal(err)
					}
					refs, err := git.Run(context.Background(), gate, "show-ref")
					if err != nil {
						t.Fatal(err)
					}
					command := newAxiTerminalPreflightCmd()
					var output bytes.Buffer
					command.SetOut(&output)
					command.SetArgs([]string{"--predecessor", event.ChildID})
					command.SetContext(context.Background())

					err = command.Execute()

					if change == "unchanged" {
						var observed struct {
							SourceAnchors map[string]string `toon:"source_anchors"`
						}
						if err != nil || toon.UnmarshalString(output.String(), &observed) != nil || observed.SourceAnchors["gate/"+ref] != candidate {
							t.Fatalf("preflight lost the expected admission anchor: %s %v", output.String(), err)
						}
					} else if err == nil || !strings.Contains(output.String(), ref) || !strings.Contains(output.String(), "terminal recovery anchor") {
						t.Fatalf("preflight accepted a %s expected candidate anchor: %s %v", change, output.String(), err)
					}
					after, err := database.GetRunsByRepo(repo.ID)
					if err != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("preflight changed immutable run history: %v", err)
					}
					afterRefs, err := git.Run(context.Background(), gate, "show-ref")
					if err != nil || refs != afterRefs {
						t.Fatalf("preflight changed gate refs: %v", err)
					}
				})
			}
		})
	}
}

func TestTerminalPreflightObservesBindingsWithoutMutatingHistoryRefsOrStartingADaemon(t *testing.T) {
	directory, root, database, repo := setupAxiQueryRepo(t)
	run(t, directory, "git", "checkout", "-b", "feature")
	chdir(t, directory)
	head, err := git.HeadSHA(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	run(t, directory, "git", "clone", "--bare", directory, root.RepoDir(repo.ID))
	parent, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(parent.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunsCustodyReturned([]string{parent.ID}); err != nil {
		t.Fatal(err)
	}
	run(t, directory, "git", "update-ref", custody.RecoveryRef(parent.ID), head)
	run(t, root.RepoDir(repo.ID), "git", "update-ref", custody.RecoveryRef(parent.ID), head)
	archiveRef := "refs/heads/archive/terminal-preflight"
	run(t, directory, "git", "update-ref", archiveRef, head)
	if _, err := database.RecordRecoveryArchive(db.RecoveryArchive{RepoID: repo.ID, RunID: parent.ID, Branch: "feature", RequiredHeadSHA: head, PreservedHeadSHA: head, ArchiveRef: archiveRef}); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := git.Run(context.Background(), directory, "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	command := newAxiTerminalPreflightCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--predecessor", parent.ID})
	command.SetContext(context.Background())

	if err := command.Execute(); err != nil {
		t.Fatalf("preflight: %v\n%s", err, output.String())
	}

	for _, required := range []string{"candidate_is_terminal_head: true", "caller_is_clean: true", "historical_source_completeness: unknown", head, "gate/" + custody.RecoveryRef(parent.ID), "caller/" + custody.RecoveryRef(parent.ID), "caller/" + archiveRef} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("preflight lost %s: %s", required, output.String())
		}
	}
	after, err := database.GetRunsByRepo(repo.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("preflight wrote history: %v", err)
	}
	afterRefs, err := git.Run(context.Background(), directory, "show-ref")
	if err != nil || refs != afterRefs {
		t.Fatalf("preflight wrote refs: %v", err)
	}
	if _, err := os.Stat(root.PIDFile()); !os.IsNotExist(err) {
		t.Fatalf("preflight started a daemon: %v", err)
	}
	for _, change := range []string{"symbolic", "deleted"} {
		if change == "symbolic" {
			run(t, directory, "git", "symbolic-ref", archiveRef, "refs/heads/feature")
		} else {
			run(t, directory, "git", "update-ref", "--no-deref", "-d", archiveRef)
		}
		command := newAxiTerminalPreflightCmd()
		var refused bytes.Buffer
		command.SetOut(&refused)
		command.SetArgs([]string{"--predecessor", parent.ID})
		command.SetContext(context.Background())
		if err := command.Execute(); err == nil || !strings.Contains(refused.String(), "terminal recovery anchors changed") {
			t.Fatalf("preflight accepted a %s archive: %s %v", change, refused.String(), err)
		}
	}
}
