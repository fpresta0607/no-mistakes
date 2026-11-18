package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAuthoredLineageEndsWhenTheChildsOpenPRWasLaterMergedOrClosed is the
// issue #194 regression. A newest authored child that ended with its PR open
// (ci_end_at_green, the CI idle timeout, a failure) kept that recorded state
// after the PR was merged or closed on the forge, so push admission refused
// the branch for good. Admission now reads the live PR state for that child
// alone, records it, and fails closed when the read cannot be made.
func TestAuthoredLineageEndsWhenTheChildsOpenPRWasLaterMergedOrClosed(t *testing.T) {
	const prURL = "https://github.com/test/repo/pull/42"
	forgeLog := installFakeForge(t)
	for _, test := range []struct {
		name           string
		branch         string
		childStatus    types.RunStatus
		hasOpenPR      bool
		liveState      string
		liveStateErr   string
		wantRefusalHas string
		wantRecorded   string
		wantForgeRead  bool
	}{
		{name: "merged on the forge after the child ended", branch: "authored", childStatus: types.RunCompleted, hasOpenPR: true, liveState: "MERGED", wantRecorded: "merged", wantForgeRead: true},
		{name: "closed on the forge after the child failed", branch: "authored", childStatus: types.RunFailed, hasOpenPR: true, liveState: "CLOSED", wantRecorded: "closed", wantForgeRead: true},
		{name: "still open on the forge", branch: "authored", childStatus: types.RunCompleted, hasOpenPR: true, liveState: "OPEN", wantRefusalHas: "validation-only authored lineage", wantRecorded: "open", wantForgeRead: true},
		{name: "live state cannot be read", branch: "authored", childStatus: types.RunCompleted, hasOpenPR: true, liveStateErr: "HTTP 502", wantRefusalHas: "live PR state could not be read", wantRecorded: "open", wantForgeRead: true},
		{name: "active child with an open PR", branch: "authored", childStatus: types.RunRunning, hasOpenPR: true, liveState: "MERGED", wantRefusalHas: "validation-only authored lineage", wantRecorded: "open"},
		{name: "terminal child without a PR", branch: "authored", childStatus: types.RunFailed, liveState: "MERGED", wantRefusalHas: "validation-only authored lineage", wantRecorded: "none"},
		{name: "branch without authored lineage", branch: "other", childStatus: types.RunCompleted, hasOpenPR: true, liveState: "MERGED", wantRecorded: "open"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repo, err := database.InsertRepoWithID("repo", t.TempDir(), "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			child := seedAuthoredLineage(t, database, repo.ID, "authored")
			if test.hasOpenPR {
				if err := database.UpdateRunPRURL(child, prURL); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunPRState(child, "open"); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.UpdateRunStatus(child, test.childStatus); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_CLI_STATE", test.liveState)
			t.Setenv("FAKE_CLI_STATE_ERR", test.liveStateErr)
			if err := os.WriteFile(forgeLog, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			manager := NewRunManager(database, p, nil)
			update := ipc.GateRefUpdate{Old: strings.Repeat("b", 40), New: strings.Repeat("1", 40), Ref: "refs/heads/" + test.branch}

			// Act
			err = manager.refuseUnadmittableRefUpdates(context.Background(), repo.ID, []ipc.GateRefUpdate{update})

			// Assert
			if test.wantRefusalHas == "" && err != nil {
				t.Fatalf("admissible push refused: %v", err)
			}
			if test.wantRefusalHas != "" && (err == nil || !strings.Contains(err.Error(), test.wantRefusalHas)) {
				t.Fatalf("refusal = %v, want one naming %q", err, test.wantRefusalHas)
			}
			run, err := database.GetRun(child)
			if err != nil {
				t.Fatal(err)
			}
			recorded := ""
			if run.PRState != nil {
				recorded = *run.PRState
			}
			if recorded != test.wantRecorded {
				t.Fatalf("recorded PR state = %q, want %q", recorded, test.wantRecorded)
			}
			if run.Status != test.childStatus {
				t.Fatalf("child status = %s, want it unchanged at %s", run.Status, test.childStatus)
			}
			calls, err := os.ReadFile(forgeLog)
			if err != nil {
				t.Fatal(err)
			}
			if hasRead := strings.Contains(string(calls), "pr view"); hasRead != test.wantForgeRead {
				t.Fatalf("forge PR read = %v, want %v; calls:\n%s", hasRead, test.wantForgeRead, calls)
			}
		})
	}
}

// TestAxiRunLineageCheckReadsTheLivePR covers the daemon method axi run calls
// before any gate ref moves. It applies the same decision as push admission,
// so a newest child that ended with its PR open is admitted once that PR was
// merged on the forge and refused while it is still open, and a request that
// names no branch is refused rather than read as a branch without lineage.
func TestAxiRunLineageCheckReadsTheLivePR(t *testing.T) {
	installFakeForge(t)
	for _, test := range []struct {
		name           string
		branch         string
		liveState      string
		wantRefusalHas string
		wantRecorded   string
	}{
		{name: "merged on the forge", branch: "authored", liveState: "MERGED", wantRecorded: "merged"},
		{name: "still open on the forge", branch: "authored", liveState: "OPEN", wantRefusalHas: "validation-only authored lineage", wantRecorded: "open"},
		{name: "no branch named", liveState: "MERGED", wantRefusalHas: "repo_id and branch are required", wantRecorded: "open"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repo, err := database.InsertRepoWithID("repo", t.TempDir(), "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			child := seedAuthoredLineage(t, database, repo.ID, "authored")
			if err := database.UpdateRunPRURL(child, "https://github.com/test/repo/pull/42"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPRState(child, "open"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(child, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_CLI_STATE", test.liveState)
			socketRoot, err := os.MkdirTemp("", "dtest")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
			server := ipc.NewServer()
			registerHandlers(server, NewRunManager(database, p, nil), database, func() {})
			socket := paths.WithRoot(socketRoot).Socket()
			if err := server.Listen(socket); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.ServeReady() }()
			t.Cleanup(func() { server.Close(); <-done })
			client, err := ipc.Dial(socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })

			// Act
			err = client.Call(ipc.MethodRefuseUnlinkedValidation, &ipc.RefuseUnlinkedValidationParams{RepoID: repo.ID, Branch: test.branch}, nil)

			// Assert
			if test.wantRefusalHas == "" && err != nil {
				t.Fatalf("admissible launch refused: %v", err)
			}
			if test.wantRefusalHas != "" && (err == nil || !strings.Contains(err.Error(), test.wantRefusalHas)) {
				t.Fatalf("refusal = %v, want one naming %q", err, test.wantRefusalHas)
			}
			run, err := database.GetRun(child)
			if err != nil {
				t.Fatal(err)
			}
			if run.PRState == nil || *run.PRState != test.wantRecorded {
				t.Fatalf("recorded PR state = %v, want %q", run.PRState, test.wantRecorded)
			}
		})
	}
}

// installFakeForge puts the fake gh first on PATH and returns the file it logs
// each invocation to.
func installFakeForge(t *testing.T) string {
	t.Helper()
	installFakeCLI(t, "gh")
	t.Setenv("FAKE_CLI_MODE", "ci-gh")
	forgeLog := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("FAKE_CLI_LOG", forgeLog)
	return forgeLog
}

// installFakeCLI builds internal/pipeline/fakecli under name, which selects
// what it fakes, and puts it first on PATH.
func installFakeCLI(t *testing.T, name string) {
	t.Helper()
	binDir := t.TempDir()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	command := exec.Command("go", "build", "-o", filepath.Join(binDir, name), "../pipeline/fakecli")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake forge: %v\n%s", err, output)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestAuthoredLineageEndsWhenTheChildsPRWasMergedAtAnotherHead runs on a forge
// whose merged proof names the merged head. A PR whose head moved outside
// no-mistakes before it was merged is no longer open, so nothing can continue
// the lineage, yet the proof does not name the child's published head. That
// merge ends the lineage without being recorded as the child's own; refusing
// it kept the branch locked for good, the issue #194 symptom.
func TestAuthoredLineageEndsWhenTheChildsPRWasMergedAtAnotherHead(t *testing.T) {
	const prURL = "https://git.example.com/team/repo/pulls/1"
	childHead, movedHead := strings.Repeat("d", 40), strings.Repeat("9", 40)
	installFakeCLI(t, fakeplugin.ExecutableName)
	for _, test := range []struct {
		name           string
		liveState      string
		liveHead       string
		wantRefusalHas string
		wantRecorded   string
	}{
		{name: "merged at the child's head", liveState: "merged", liveHead: childHead, wantRecorded: "merged"},
		{name: "merged at a head moved outside no-mistakes", liveState: "merged", liveHead: movedHead, wantRecorded: "open"},
		{name: "still open at a head moved outside no-mistakes", liveState: "open", liveHead: movedHead, wantRefusalHas: "validation-only authored lineage", wantRecorded: "open"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			globalConfig := "provider_plugins:\n  fake:\n    command: " + fakeplugin.ExecutableName + "\n    hosts: [git.example.com]\n"
			if err := os.WriteFile(p.ConfigFile(), []byte(globalConfig), 0o644); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(t.TempDir(), "plugin-state.json")
			forge := fakeplugin.State{PRs: []fakeplugin.PR{{Number: "1", URL: prURL, HeadBranch: "authored", BaseBranch: "main", HeadSHA: test.liveHead, State: test.liveState}}}
			if err := fakeplugin.Save(statePath, forge); err != nil {
				t.Fatal(err)
			}
			t.Setenv(fakeplugin.EnvState, statePath)
			t.Setenv(fakeplugin.EnvLog, filepath.Join(t.TempDir(), "plugin-requests.ndjson"))
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repo, err := database.InsertRepoWithID("repo", t.TempDir(), "https://git.example.com/team/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			child := seedAuthoredLineage(t, database, repo.ID, "authored")
			if err := database.UpdateRunPRURL(child, prURL); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPRState(child, "open"); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(child, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			manager := NewRunManager(database, p, nil)
			update := ipc.GateRefUpdate{Old: strings.Repeat("b", 40), New: strings.Repeat("1", 40), Ref: "refs/heads/authored"}

			// Act
			err = manager.refuseUnadmittableRefUpdates(context.Background(), repo.ID, []ipc.GateRefUpdate{update})

			// Assert
			if test.wantRefusalHas == "" && err != nil {
				t.Fatalf("admissible push refused: %v", err)
			}
			if test.wantRefusalHas != "" && (err == nil || !strings.Contains(err.Error(), test.wantRefusalHas)) {
				t.Fatalf("refusal = %v, want one naming %q", err, test.wantRefusalHas)
			}
			run, err := database.GetRun(child)
			if err != nil {
				t.Fatal(err)
			}
			if run.HeadSHA != childHead {
				t.Fatalf("child head = %s, want the seeded %s", run.HeadSHA, childHead)
			}
			if run.PRState == nil || *run.PRState != test.wantRecorded {
				t.Fatalf("recorded PR state = %v, want %q", run.PRState, test.wantRecorded)
			}
		})
	}
}
