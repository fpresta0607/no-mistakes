package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestTerminalContinuationRequiresExplicitReason(t *testing.T) {
	cmd := newAxiSyncCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--recover", "--continue"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(output.String(), "--reason") {
		t.Fatalf("continuation must require an operator reason: %v %s", err, output.String())
	}
}

// The daemon fetches an unpublished caller commit with `git fetch <CallerDir>`,
// and upload-pack never searches parent directories, so a continuation started
// from a subdirectory must still name the repository's top level.
func TestTerminalContinuationFromASubdirectoryNamesTheRepositoryTopLevel(t *testing.T) {
	var received ipc.RecoverTerminalParams
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodRecoverTerminal, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &received); err != nil {
				return nil, err
			}
			return &ipc.RecoverTerminalResult{RunID: "successor"}, nil
		})
	})
	topLevel, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.GetRepoByPath(topLevel)
	if err != nil || repo == nil {
		t.Fatalf("fixture repository is not registered: %v", err)
	}
	head := cliGit(t, topLevel, "rev-parse", "HEAD")
	if _, err := database.InsertRun(repo.ID, "feature/omit", head, head); err != nil {
		t.Fatal(err)
	}
	subdirectory := filepath.Join(topLevel, "nested", "deeper")
	if err := os.MkdirAll(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, subdirectory)
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&output)

	err = runTerminalContinuation(cmd, "continue preserved source")

	if err != nil {
		t.Fatalf("continuation failed: %v\n%s", err, output.String())
	}
	if received.CallerDir != topLevel {
		t.Fatalf("CallerDir = %q, want the repository top level %q", received.CallerDir, topLevel)
	}
}
