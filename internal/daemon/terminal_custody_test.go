package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalAgentsRequireActualInactivity(t *testing.T) {
	missing := 1 << 30
	active := os.Getpid()
	for _, test := range []struct {
		name       string
		pid        *int
		isAccepted bool
	}{{"no agent", nil, true}, {"exited agent", &missing, true}, {"live agent", &active, false}} {
		t.Run(test.name, func(t *testing.T) {
			err := validateTerminalAgentInactivity(&db.TerminalSnapshot{Steps: []db.StepResult{{AgentPID: test.pid}}})
			if (err == nil) != test.isAccepted {
				t.Fatalf("agent inactivity = %v, want %v: %v", err == nil, test.isAccepted, err)
			}
		})
	}
}

func TestTerminalRestart(t *testing.T) {
	root, err := os.MkdirTemp("", "terminal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, variable := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME"} {
		t.Setenv(variable, root)
	}
	binary := filepath.Join(root, "codex")
	script := "#!/bin/sh\necho fixture\n"
	if runtime.GOOS == "windows" {
		binary += ".bat"
		script = "@echo off\r\necho fixture\r\n"
	}
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	global := fmt.Sprintf("agent: codex\nagent_path_override:\n  codex: %q\nagent_config:\n  codex: {model: fixture, effort: xhigh}\nagent_args_override:\n  codex: ['-c', 'service_tier=\"default\"']\n", binary)
	if err := os.WriteFile(p.ConfigFile(), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, submitted := setupTestGitRepo(t, p, database, "terminal-restart")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	gate := p.RepoDir(repo.ID)
	gitCmd(t, gate, "update-ref", "refs/heads/feature", submitted)
	expected := launchExpectation(submitted)
	proof, err := expected.Verify(submitted, expected.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithLaunchAssertion(repo.ID, "feature", submitted, submitted, nil, "nonce", "frozen-policy", "intent-digest", "", false, nil, expected, proof)
	if err != nil {
		t.Fatal(err)
	}
	managed := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), gate, managed, submitted); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, managed, "config", "user.name", "Fixture")
	gitCmd(t, managed, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(managed, "fix.txt"), []byte("preserved fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, managed, "add", "fix.txt")
	gitCmd(t, managed, "commit", "-m", "preserved correction")
	candidate := gitOutput(t, managed, "rev-parse", "HEAD")
	for _, name := range types.AllSteps() {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name.Order() < types.StepTest.Order() {
			if err := database.StartStepWithAutoFixLimit(step.ID, 0); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[],"summary":"original result"}`
			if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, "original.log"); err != nil {
				t.Fatal(err)
			}
		} else if name == types.StepTest {
			if err := database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			stalePID := 1 << 30
			if err := database.SetStepAgentActivity(step.ID, "agent executing before crash", &stalePID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	start := func() func() {
		exited := make(chan error, 1)
		factory := func() []pipeline.Step {
			var sequence []pipeline.Step
			for _, name := range types.AllSteps() {
				if name == types.StepReview {
					sequence = append(sequence, &mockApprovalStep{name: name})
				} else {
					sequence = append(sequence, &mockPassStep{name: name})
				}
			}
			return sequence
		}
		go func() { exited <- RunWithOptions(p, database, factory) }()
		waitForDaemonReadyOrExit(t, p, exited)
		return func() {
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil); err != nil {
				t.Fatal(err)
			}
			client.Close()
			select {
			case err := <-exited:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("isolated daemon did not stop")
			}
		}
	}
	stop := start()
	terminal, err := database.GetRun(run.ID)
	if err != nil || terminal.Status != types.RunFailed || terminal.HeadSHA != candidate || terminal.TerminalHeadVerifiedAt == nil {
		t.Fatalf("crash recovery = %+v %v", terminal, err)
	}
	if gitOutput(t, gate, "rev-parse", custody.RecoveryRef(run.ID)) != candidate {
		t.Fatal("candidate was not anchored")
	}
	stop()
	if err := git.WorktreeRemove(context.Background(), gate, managed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "local-only.txt"), []byte("retained caller source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Trusted main moves while the immutable original receipt stays at submitted.
	gitCmd(t, gate, "update-ref", "refs/heads/main", candidate)
	stop = start()
	defer stop()
	before, err := database.ReadTerminalSnapshot(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.RecoverTerminalParams{RepoID: repo.ID, RunID: run.ID, Branch: "feature", CallerDir: repo.WorkingPath, SnapshotDigest: before.Digest(), Reason: "validate interrupted source"}
	var accepted ipc.RecoverTerminalResult
	if err := client.CallWithTimeout(ipc.MethodRecoverTerminal, request, &accepted, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if accepted.RunID != run.ID || accepted.CandidateSHA != candidate || !accepted.Started {
		t.Fatalf("acceptance = %+v", accepted)
	}
	for _, key := range []string{"user.name", "user.email"} {
		if actual, expected := gitOutput(t, managed, "config", "--worktree", "--get", key), gitOutput(t, repo.WorkingPath, "config", "--local", "--get", key); actual != expected {
			t.Fatalf("reconstructed %s = %q, want %q", key, actual, expected)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	var current *db.Run
	for time.Now().Before(deadline) {
		current, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.AwaitingAgentSince != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if current.AwaitingAgentSince == nil || current.ReviewApprovedHeadSHA != nil || current.TerminalHeadVerifiedAt != nil {
		t.Fatalf("candidate bypassed fresh Review: %+v", current)
	}
	event, err := database.GetTerminalAdoption(run.ID)
	if err != nil || event == nil || event.Assertion.TrustedSHA != candidate || !reflect.DeepEqual(event.Snapshot.Run, before.Run) || !reflect.DeepEqual(current.LaunchAssertionProof, before.Run.LaunchAssertionProof) {
		t.Fatalf("launch/history provenance = %+v %v", event, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo.WorkingPath, "local-only.txt")); err != nil || string(data) != "retained caller source\n" {
		t.Fatal("caller dirty source changed")
	}
	var repeated ipc.RecoverTerminalResult
	if err := client.Call(ipc.MethodRecoverTerminal, request, &repeated); err != nil || repeated.Started || repeated.AdoptionID != accepted.AdoptionID {
		t.Fatalf("independent replay = %+v %v", repeated, err)
	}
	if err := os.WriteFile(filepath.Join(managed, "later-fix.txt"), []byte("later correction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, managed, "add", "later-fix.txt")
	gitCmd(t, managed, "commit", "-m", "later correction")
	later := gitOutput(t, managed, "rev-parse", "HEAD")
	if err := client.Call(ipc.MethodCancelRun, ipc.CancelRunParams{RunID: run.ID}, nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		current, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, gate, managed, "terminal continuation regression")
	if preserved := gitOutput(t, managed, "rev-parse", "HEAD"); preserved != later {
		t.Fatal("later continuation correction was discarded")
	}
	if original := gitOutput(t, gate, "rev-parse", custody.RecoveryRef(run.ID)); original != candidate {
		t.Fatal("original recovery anchor was rewritten")
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: later, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.UpstreamURL), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, gate, managed, "published continuation regression")
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("clean successfully published source was not reclaimed: %v", err)
	}
}

func TestTerminalPublicationRequiresTheOriginalPRAndHead(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, variable := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "NM_HOME"} {
		t.Setenv(variable, p.Root())
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "published-terminal")
	public := p.RepoDir(repo.ID)
	gitCmd(t, public, "update-ref", "refs/heads/feature", head)
	repo.UpstreamURL = "https://github.com/example/fixture.git"
	gitCmd(t, repo.WorkingPath, "config", "url."+filepath.ToSlash(public)+".insteadOf", repo.UpstreamURL)
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(repo.UpstreamURL), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/example/fixture/pull/27"
	if err := database.UpdateRunPRURL(run.ID, url); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRState(run.ID, "open"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(t.TempDir(), "pr.json")
	binaryDir := t.TempDir()
	command := filepath.Join(binaryDir, "gh")
	script := "#!/bin/sh\ncat " + shellQuoteForTest(responsePath) + "\n"
	if runtime.GOOS == "windows" {
		command += ".bat"
		script = "@echo off\r\ntype \"" + responsePath + "\"\r\n"
	}
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binaryDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, test := range []struct {
		name, state, prHead, owner, base, kind string
		isAccepted                             bool
	}{
		{"bound", "open", head, "example/fixture", "main", "upstream", true},
		{"moved PR head", "open", run.BaseSHA + "0", "example/fixture", "main", "upstream", false},
		{"wrong repository", "open", head, "other/fixture", "main", "upstream", false},
		{"closed PR", "closed", head, "example/fixture", "main", "upstream", false},
		{"configured live base", "open", head, "example/fixture", "release", "upstream", true},
		{"live base differs from validation", "open", head, "example/fixture", "changed", "upstream", false},
		{"wrong target kind", "open", head, "example/fixture", "main", "fork", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"html_url":%q,"state":%q,"head":{"sha":%q,"ref":"feature","repo":{"full_name":%q}},"base":{"ref":%q,"repo":{"full_name":"example/fixture"}}}`, url, test.state, test.prHead, test.owner, test.base)
			if err := os.WriteFile(responsePath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			selected := *run
			selected.PushTargetKind = &test.kind
			cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
			if test.base == "release" {
				cfg.PR.BaseBranch = "release"
			}
			err := proveTerminalPublication(context.Background(), &selected, repo, repo.WorkingPath, nil, cfg)
			if (err == nil) != test.isAccepted {
				t.Fatalf("publication acceptance = %v, want %v: %v", err == nil, test.isAccepted, err)
			}
		})
	}
}
