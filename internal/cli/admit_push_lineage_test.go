package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGatePushToAnAuthoredLineageBranchIsRefusedBeforeTheRefMoves pushes with
// real Git through the managed pre-receive hook, the real admit-push command
// and a daemon in its own process, so the hook's ancestry is a plain operator's
// (issue #150). Before the fix pre-receive admitted the push and moved the
// gate branch, and only the post-receive launch refused, which left the
// branch at a head the lineage never bound.
func TestGatePushToAnAuthoredLineageBranchIsRefusedBeforeTheRefMoves(t *testing.T) {
	// Arrange
	repoDir := setupTestRepo(t)
	p := paths.WithRoot(os.Getenv("NM_HOME"))
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cliGit(t, repoDir, "checkout", "-b", "feature")
	cliGit(t, repoDir, "commit", "--allow-empty", "-m", "authored base")
	boundHead := cliGit(t, repoDir, "rev-parse", "HEAD")
	repo, _, err := gate.Init(context.Background(), database, p, ".")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	// Isolate admission: a launch from post-receive would start a pipeline.
	if err := os.WriteFile(filepath.Join(gateDir, "hooks", "post-receive"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	userHookInput := filepath.Join(t.TempDir(), "user-hook-input.txt")
	userHook := "#!/bin/sh\ncat >> '" + filepath.ToSlash(userHookInput) + "'\n"
	if err := os.WriteFile(filepath.Join(gateDir, "hooks", "pre-receive.no-mistakes-user"), []byte(userHook), 0o755); err != nil {
		t.Fatal(err)
	}
	cliGit(t, gateDir, "fetch", repoDir, "refs/heads/feature:refs/heads/feature")
	child := seedFailedAuthoredChild(t, database, repo.ID, "feature", boundHead)
	if err := daemon.EnsureDaemon(p); err != nil {
		t.Fatal(err)
	}
	cliGit(t, repoDir, "commit", "--allow-empty", "-m", "ordinary work")
	ordinaryHead := cliGit(t, repoDir, "rev-parse", "HEAD")
	zero := strings.Repeat("0", len(ordinaryHead))
	gateRef := func(ref string) string {
		out, err := exec.Command("git", "--git-dir", gateDir, "rev-parse", "--verify", "--quiet", ref).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	push := func(refspecs ...string) (string, error) {
		command := exec.Command("git", append([]string{"push", gate.RemoteName}, refspecs...)...)
		command.Dir = repoDir
		out, err := command.CombinedOutput()
		return string(out), err
	}

	for _, refused := range []struct {
		name     string
		refspecs []string
	}{
		{name: "ordinary push", refspecs: []string{"feature"}},
		{name: "multi-ref push with one authored-lineage branch", refspecs: []string{"HEAD:refs/heads/side", "feature"}},
	} {
		// Act
		out, err := push(refused.refspecs...)

		// Assert
		if err == nil || !strings.Contains(out, "gate push refused before ref mutation") || !strings.Contains(out, "validation-only authored lineage") {
			t.Fatalf("%s: push was not refused by admission: %v\n%s", refused.name, err, out)
		}
		if got := gateRef("refs/heads/feature"); got != boundHead {
			t.Fatalf("%s: refused push moved the gate branch to %q, want %s", refused.name, got, boundHead)
		}
		if got := gateRef("refs/heads/side"); got != "" {
			t.Fatalf("%s: refused push created refs/heads/side at %s", refused.name, got)
		}
	}
	if _, err := os.Stat(userHookInput); !os.IsNotExist(err) {
		t.Fatalf("user hook ran for a refused push: %v", err)
	}

	// Positive control: another branch is admitted, and the user hook reads the exact input.
	if out, err := push("HEAD:refs/heads/side"); err != nil {
		t.Fatalf("push to a branch without lineage refused: %v\n%s", err, out)
	}
	if got := gateRef("refs/heads/side"); got != ordinaryHead {
		t.Fatalf("admitted push left refs/heads/side at %q, want %s", got, ordinaryHead)
	}
	recorded, err := os.ReadFile(userHookInput)
	if err != nil {
		t.Fatal(err)
	}
	if want := zero + " " + ordinaryHead + " refs/heads/side\n"; string(recorded) != want {
		t.Fatalf("user hook input = %q, want %q", recorded, want)
	}

	// Positive control: once the newest child's PR is merged the lineage has ended.
	if err := database.UpdateRunPRState(child, "merged"); err != nil {
		t.Fatal(err)
	}
	if out, err := push("feature"); err != nil {
		t.Fatalf("push after the lineage ended refused: %v\n%s", err, out)
	}
	if got := gateRef("refs/heads/feature"); got != ordinaryHead {
		t.Fatalf("admitted push left the gate branch at %q, want %s", got, ordinaryHead)
	}
}

// TestAxiRunLeavesARecordedOpenPRToDaemonAdmission covers the CLI half of
// issue #194. axi run's own pre-push check read only the recorded PR state, so
// it refused a branch whose newest authored child ended with its PR open even
// after that PR was merged on the forge. axi run now asks the daemon, which
// reads the live PR first, and it asks before any gate ref moves: the gate
// branch below holds a lineage-bound head the operator's branch has since
// rewritten, which stale-branch reconciliation would archive and delete. A
// daemon that predates the method refuses the same way.
func TestAxiRunLeavesARecordedOpenPRToDaemonAdmission(t *testing.T) {
	const lineageRefusal = "this branch has validation-only authored lineage"
	for _, test := range []struct {
		name          string
		refuseLineage func(context.Context, json.RawMessage) (interface{}, error)
		wantRefusal   string
	}{
		{name: "daemon admits", refuseLineage: func(context.Context, json.RawMessage) (interface{}, error) { return struct{}{}, nil }},
		{name: "daemon refuses", refuseLineage: func(context.Context, json.RawMessage) (interface{}, error) { return nil, errors.New(lineageRefusal) }, wantRefusal: lineageRefusal},
		{name: "daemon predates the method", wantRefusal: "method not found: " + ipc.MethodRefuseUnlinkedValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			requests := make(chan ipc.RefuseUnlinkedValidationParams, 1)
			opts := axiTimeoutOpts{}
			if test.refuseLineage != nil {
				opts.refuseLineage = func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
					var request ipc.RefuseUnlinkedValidationParams
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					requests <- request
					return test.refuseLineage(ctx, raw)
				}
			}
			fx := newAxiTimeoutFixture(t, opts)
			fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return nil, nil })
			p := paths.WithRoot(os.Getenv("NM_HOME"))
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repos, err := database.GetRepos()
			if err != nil || len(repos) != 1 {
				t.Fatalf("registered repos = %v, %v", repos, err)
			}
			local, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			cliGit(t, local, "commit", "--allow-empty", "-m", "lineage-bound head")
			boundHead := cliGit(t, local, "rev-parse", "HEAD")
			cliGit(t, local, "reset", "--hard", fx.head)
			gateDir := p.RepoDir(repos[0].ID)
			cliGit(t, local, "init", "--bare", gateDir)
			cliGit(t, gateDir, "fetch", local, boundHead+":refs/heads/feature/timeout")

			// Act
			out, err := executeCmd("axi", "run", "--intent", "x")

			// Assert
			if test.refuseLineage != nil {
				if request := <-requests; request.RepoID != repos[0].ID || request.Branch != "feature/timeout" {
					t.Fatalf("lineage request = %+v, want repo %s branch feature/timeout", request, repos[0].ID)
				}
			}
			untouchedGate := "refs/heads/feature/timeout " + boundHead
			gateRefs := cliGit(t, gateDir, "for-each-ref", "--format=%(refname) %(objectname)")
			if test.wantRefusal == "" {
				if strings.Contains(out, "check authored lineage") {
					t.Fatalf("admitted lineage refused: %v\n%s", err, out)
				}
				// Positive control: past the check, axi run reconciles the stale gate branch.
				if gateRefs == untouchedGate {
					t.Fatalf("admitted axi run never reached gate reconciliation:\n%s", out)
				}
				return
			}
			if err == nil || !strings.Contains(out, "check authored lineage: "+test.wantRefusal) {
				t.Fatalf("axi run was not refused with %q: %v\n%s", test.wantRefusal, err, out)
			}
			if gateRefs != untouchedGate {
				t.Fatalf("refused axi run changed gate refs to:\n%s\nwant only %s", gateRefs, untouchedGate)
			}
		})
	}
}

// seedFailedAuthoredChild records a validation-only authored child whose
// lineage has not ended: its predecessor ran at boundHead, and the child
// failed without a merged or closed PR.
func seedFailedAuthoredChild(t *testing.T, database *db.DB, repoID, branch, boundHead string) string {
	t.Helper()
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: strings.Repeat("a", 40), Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	proof, err := assertion.Verify(assertion.TrustedSHA, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := db.RunIntent{Summary: "Validate the exact authored source", Source: db.RunIntentSourceAgent, Score: 1}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
	parent, err := database.InsertRunWithLaunchAssertion(repoID, branch, boundHead, boundHead, &intent, "original", "generation", digest, "", true, nil, assertion, proof)
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
