package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCachedBranchSyncFieldOmitsAbsentRunWithoutGit(t *testing.T) {
	newCLISyncFixture(t)
	tracePath := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", filepath.ToSlash(tracePath))
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	if field := cachedBranchSyncField(cmd, "absent-run"); field != nil {
		t.Fatalf("absent run produced branch-sync field: %#v", field)
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(trace) != 0 {
		t.Fatalf("absent run triggered Git inspection:\n%s", trace)
	}
}

func TestCachedBranchSyncFieldPreservesInspection(t *testing.T) {
	fixture := newCLISyncFixture(t)
	appPaths, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(appPaths.DB())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(fixture.runID)
	if err != nil || stored == nil {
		database.Close()
		t.Fatalf("load fixture run: %v, %v", stored, err)
	}
	other, err := database.InsertRun(stored.RepoID, "other-branch", fixture.old, fixture.base)
	closeErr := database.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("insert other run: %v; close: %v", err, closeErr)
	}

	for _, test := range []struct {
		name         string
		runID        string
		shouldRender bool
	}{
		{name: "present", runID: fixture.runID, shouldRender: true},
		{name: "empty", shouldRender: true},
		{name: "present_but_not_selected", runID: other.ID},
		{name: "read_error", runID: fixture.runID, shouldRender: true},
		{name: "open_error", runID: fixture.runID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "read_error" {
				connection, err := sql.Open("sqlite", appPaths.DB())
				if err != nil {
					t.Fatal(err)
				}
				_, err = connection.Exec("ALTER TABLE runs DROP COLUMN awaiting_agent_since")
				closeErr := connection.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("prepare legacy schema: %v; close: %v", err, closeErr)
				}
				readonly, err := db.OpenReadOnly(appPaths.DB())
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := readonly.GetRun(test.runID)
				closeErr = readonly.Close()
				if readErr == nil || closeErr != nil {
					t.Fatalf("expected legacy read error, got %v; close: %v", readErr, closeErr)
				}
			}
			if test.name == "open_error" {
				t.Setenv("NM_HOME", t.TempDir())
				missingPaths, err := paths.New()
				if err != nil {
					t.Fatal(err)
				}
				if readonly, err := db.OpenReadOnly(missingPaths.DB()); err == nil {
					readonly.Close()
					t.Fatal("expected missing database to fail read-only open")
				}
			}
			tracePath := filepath.Join(t.TempDir(), "git-trace.log")
			t.Setenv("GIT_TRACE", filepath.ToSlash(tracePath))
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())

			field := cachedBranchSyncField(cmd, test.runID)

			trace, err := os.ReadFile(tracePath)
			if err != nil || !strings.Contains(string(trace), "rev-parse") {
				t.Fatalf("expected existing Git inspection, got %q; error: %v", trace, err)
			}
			if !test.shouldRender {
				if field != nil {
					t.Fatalf("unmatched run produced branch-sync field: %#v", field)
				}
				return
			}
			if field == nil {
				t.Fatal("existing branch-sync field was omitted")
			}
			output, err := toon.MarshalString(toon.NewObject(*field))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"state: behind", "safety: refresh_required", "freshness: pipeline_push", fixture.runID, fixture.old, fixture.pushed} {
				if !strings.Contains(output, want) {
					t.Errorf("branch-sync field missing %q:\n%s", want, output)
				}
			}
		})
	}
}
