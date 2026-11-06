package cli

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
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
