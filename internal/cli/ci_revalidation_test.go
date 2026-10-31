package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCIRevalidationWaitBoundsTheMutationRequestWithoutRetry(t *testing.T) {
	locations := paths.WithRoot(makeSocketSafeTempDir(t))
	if err := locations.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(locations.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	caller := t.TempDir()
	cliGit(t, caller, "init", "-b", "feature")
	cliGit(t, caller, "config", "user.name", "Test")
	cliGit(t, caller, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(caller, "file.txt"), []byte("retained\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliGit(t, caller, "add", "file.txt")
	cliGit(t, caller, "commit", "-m", "retain the candidate")
	head := cliGit(t, caller, "rev-parse", "HEAD")
	repo, err := database.InsertRepo(caller, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	before, err := database.ReadCIRepairSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer close(release)
	requests := make(chan ipc.RespondParams, 2)
	server := ipc.NewServer()
	server.Handle(ipc.MethodRespond, func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
		var request ipc.RespondParams
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		requests <- request
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &ipc.RespondResult{OK: true}, nil
	})
	startIPCServer(t, server, locations.Socket())
	client := dialReady(t, locations.Socket())
	defer client.Close()
	chdir(t, caller)
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetContext(t.Context())
	arguments := respondArgs{candidate: head, reason: "validate the retained candidate", ghConfigDir: t.TempDir(), expectedLogin: "operator", reselectPublication: true, wait: 2 * time.Second}
	ctx, cancel, err := boundAxiWait(command.Context(), arguments.wait)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runAxiCIRevalidation(command, &axiEnv{p: locations, d: database, client: client}, ctx, run.ID, runView{}, arguments)
	}()
	select {
	case request := <-requests:
		if request.RunID != run.ID || request.CIRevalidation == nil || !request.CIRevalidation.ReselectPublication || request.CIRevalidation.CandidateSHA != head {
			t.Fatalf("mutation lost its exact operator binding: %+v", request)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("reselection request was not sent")
	}
	select {
	case err := <-done:
		assertWaitElapsed(t, err, output.String(), "2s")
	case <-time.After(4 * time.Second):
		t.Fatal("reselection ignored its caller's wait deadline")
	}
	if len(requests) != 0 {
		t.Fatal("timed-out adoption mutation was retried")
	}
	after, err := database.ReadCIRepairSnapshot(run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("timed-out follow altered the retained snapshot: %v", err)
	}
}

func TestCIRevalidationRefusesInvalidOperatorInputsBeforeReadingRunState(t *testing.T) {
	for _, name := range []string{"automatic decisions", "finding action", "candidate", "reason", "relative profile", "login"} {
		t.Run(name, func(t *testing.T) {
			arguments := respondArgs{candidate: strings.Repeat("a", 40), reason: "validate this retained work", ghConfigDir: t.TempDir(), expectedLogin: "operator"}
			switch name {
			case "automatic decisions":
				arguments.autoYes = true
			case "finding action":
				arguments.findings = "f1"
			case "candidate":
				arguments.candidate = "HEAD"
			case "reason":
				arguments.reason = " "
			case "relative profile":
				arguments.ghConfigDir = "relative"
			case "login":
				arguments.expectedLogin = ""
			}
			var output bytes.Buffer
			command := &cobra.Command{}
			command.SetOut(&output)
			command.SetErr(&output)
			if err := runAxiCIRevalidation(command, nil, t.Context(), "unread-run", runView{}, arguments); err == nil {
				t.Fatal("invalid operator request reached the run-state boundary")
			}
		})
	}
}
