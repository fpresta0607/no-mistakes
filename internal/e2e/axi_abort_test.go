//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestAxiAbortBuiltCLIConfirmsAbsentRun(t *testing.T) {
	isolation := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME", "CFO_ROOT", "CFO_HOME", "CODE_GOBLINS_ROOT", "GOBLINS_ROOT"} {
		directory := filepath.Join(isolation, name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, directory)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(name, "")
	}
	harness := NewHarness(t, SetupOpts{})
	if output, err := harness.Run("daemon", "start"); err != nil {
		t.Fatalf("start disposable daemon: %v\n%s", err, output)
	}
	client, err := ipc.Dial(paths.WithRoot(harness.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var state ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: "nonexistent-run-id"}, &state); err == nil || err.Error() != "run not found: nonexistent-run-id" {
		t.Fatalf("fixture did not prove exact absence: %+v, %v", state, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		output, err := harness.Run("axi", "abort", "--run", "nonexistent-run-id")
		t.Logf("public abort attempt %d: %v\n%s", attempt, err, output)
		if err != nil || !strings.Contains(output, "aborted: false") || !strings.Contains(output, "nonexistent-run-id") {
			t.Fatalf("confirmed absent run must be an idempotent no-op: %v\n%s", err, output)
		}
		if strings.Contains(output, "run_status") || strings.Contains(output, "terminal_confirmed: true") {
			t.Fatalf("absent run fabricated terminal state: %s", output)
		}
	}
	database, err := db.Open(paths.WithRoot(harness.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if run, err := database.GetRun("nonexistent-run-id"); err != nil || run != nil {
		t.Fatalf("abort created a run: %+v, %v", run, err)
	}
	if _, err := os.Stat(harness.AgentLog); !os.IsNotExist(err) {
		t.Fatalf("abort entered the agent fixture: %v", err)
	}
}
