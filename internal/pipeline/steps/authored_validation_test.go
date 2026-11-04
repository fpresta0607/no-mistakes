package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/authoredvalidation"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/launchassert"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func authoredStageContext(t *testing.T, ag agent.Agent, commands config.Commands) *pipeline.StepContext {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContext(t, ag, dir, base, head, commands)
	repository, err := sctx.DB.InsertRepo(dir, dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	selection := agentcfg.Selection{Harness: types.AgentCodex, Model: "fixture", Effort: agentcfg.EffortXHigh, ServiceTier: "default"}
	assertion := &launchassert.Expectation{TrustedSHA: base, Profiles: map[string][]agentcfg.Selection{"primary": {selection}, "reviewer": {selection}, "fixer": {selection}}}
	proof, err := assertion.Verify(base, assertion.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	intent := db.RunIntent{Summary: "Validate the exact authored source without repairs", Source: db.RunIntentSourceAgent, Score: 1}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(intent.Summary)))
	parent, err := sctx.DB.InsertRunWithLaunchAssertion(repository.ID, "feature", base, base, &intent, "original", "generation", digest, "", true, nil, assertion, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunStatus(parent.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sctx.DB.ReadTerminalSnapshot(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredvalidation.Request{PredecessorID: parent.ID, SnapshotDigest: snapshot.Digest(), RepoID: repository.ID, Branch: "feature", CallerDir: dir, CandidateSHA: head, CandidateTree: gitCmd(t, dir, "rev-parse", "HEAD^{tree}"), LaunchNonce: "original", ValidationGeneration: "generation", IntentDigest: digest,
		ControllerPolicy: authoredvalidation.ControllerPolicy{SHA256: strings.Repeat("f", 64), Class: "ordinary", ReviewCycles: 2}, LaunchAssertion: assertion, HistoricalSourceCompleteness: "unknown", Reason: "Validate this authored candidate"}
	event, _, err := sctx.DB.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: proof, WorktreeRoot: t.TempDir(), Operator: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run, err = sctx.DB.GetRun(event.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Repo, sctx.Shared = repository, &pipeline.RunShared{}
	return sctx
}

func TestAuthoredStagesRefuseAgentEditsBeforeCommitting(t *testing.T) {
	for _, test := range []struct {
		name     string
		step     pipeline.Step
		commands config.Commands
	}{
		{"document", &DocumentStep{}, config.Commands{Lint: "echo checked"}},
		{"combined document lint", &DocumentStep{}, config.Commands{}},
		{"fallback lint", &LintStep{}, config.Commands{}},
	} {
		for _, isAgentCommit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/agent_commit_%t", test.name, isAgentCommit), func(t *testing.T) {
				ag := &mockAgent{name: "editing-agent", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("native repair\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					if isAgentCommit {
						gitCmd(t, opts.CWD, "add", "feature.txt")
						gitCmd(t, opts.CWD, "commit", "-m", "native repair")
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repaired source"}`)}, nil
				}}
				sctx := authoredStageContext(t, ag, test.commands)
				admitted := sctx.Run.HeadSHA

				_, err := test.step.Execute(sctx)

				if err == nil || len(ag.calls) != 1 {
					t.Fatalf("editing agent was accepted: calls=%d error=%v", len(ag.calls), err)
				}
				if !isAgentCommit && gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != admitted {
					t.Fatal("stage committed the agent's forbidden repair")
				}
				stored, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil || stored.HeadSHA != admitted || stored.LastPushedSHA != nil {
					t.Fatalf("forbidden repair acquired native authority: %+v %v", stored, err)
				}
			})
		}
	}
}

func TestAuthoredHousekeepingReportsBothDutiesWithoutChangingSource(t *testing.T) {
	ag := &mockAgent{name: "reporting-agent", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"d1","severity":"warning","description":"stale documentation","action":"auto-fix","category":"documentation"},{"id":"l1","severity":"warning","description":"format needed","action":"auto-fix","category":"lint"}],"summary":"report source issues"}`)}, nil
	}}
	sctx := authoredStageContext(t, ag, config.Commands{})
	admitted := sctx.Run.HeadSHA

	document, err := (&DocumentStep{}).Execute(sctx)
	if err != nil || !document.NeedsApproval || document.AutoFixable {
		t.Fatalf("document failed to report: %+v %v", document, err)
	}
	lint, err := (&LintStep{}).Execute(sctx)
	if err != nil || !lint.NeedsApproval || lint.AutoFixable || len(ag.calls) != 1 {
		t.Fatalf("combined lint duty lost: %+v %v calls=%d", lint, err, len(ag.calls))
	}
	if gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != admitted || gitStatusPorcelain(t, sctx.WorkDir) != "" {
		t.Fatal("report-only housekeeping changed source")
	}
}

func TestAuthoredCommandsRejectSourceEdits(t *testing.T) {
	for _, name := range []string{"test", "lint", "prepare", "custom", "wrapped test"} {
		t.Run(name, func(t *testing.T) {
			sctx := authoredStageContext(t, nil, config.Commands{})
			command := "echo native-repair>feature.txt"
			var err error

			if name == "wrapped test" {
				binDir := fakeCLIBinDir(t)
				linkTestBinary(t, binDir, "turnline")
				program := filepath.Join(binDir, "turnline")
				if runtime.GOOS == "windows" {
					program += ".exe"
				}
				sctx.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "wrapper", "FAKE_CLI_WRAPPER_LOG": filepath.Join(t.TempDir(), "wrapper.ndjson")})
				sctx.Config.TestCommandWrapper.Command = []string{program, "--"}
				_, _, err = runConfiguredChecks(sctx, "test", command)
			} else if name == "custom" {
				_, err = (&CustomGateStep{Gate: config.Gate{Name: "authored", Command: command, After: "review"}}).Execute(sctx)
			} else {
				_, _, err = executeRepositoryCommand(sctx, name, command)
			}

			if err == nil {
				t.Fatal("mutating validation command was accepted")
			}
			content, readErr := os.ReadFile(filepath.Join(sctx.WorkDir, "feature.txt"))
			if readErr != nil || !strings.Contains(string(content), "native-repair") {
				t.Fatalf("command did not reproduce the edit: %q %v", content, readErr)
			}
		})
	}
}

func TestAuthoredPushNeverFormatsOrCommitsLeftovers(t *testing.T) {
	for _, hasLeftovers := range []bool{false, true} {
		t.Run(fmt.Sprintf("leftovers_%t", hasLeftovers), func(t *testing.T) {
			sctx := authoredStageContext(t, nil, config.Commands{Format: "echo formatter-repair>feature.txt"})
			admitted := sctx.Run.HeadSHA
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			gitCmd(t, sctx.WorkDir, "remote", "set-url", "origin", upstream)
			sctx.Repo.UpstreamURL = upstream
			setupGateMirror(t, sctx)
			recordReviewApproval(t, sctx, admitted)
			if hasLeftovers {
				if err := os.WriteFile(filepath.Join(sctx.WorkDir, "feature.txt"), []byte("leftover repair\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			_, err := (&PushStep{}).Execute(sctx)

			if hasLeftovers {
				if err == nil || gitCmd(t, upstream, "for-each-ref", "refs/heads/") != "" {
					t.Fatalf("leftover source reached publication: %v", err)
				}
			} else if err != nil || gitCmd(t, upstream, "rev-parse", "refs/heads/feature") != admitted {
				t.Fatalf("clean authored publication failed: %v", err)
			}
			if gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != admitted {
				t.Fatal("Push committed a native repair")
			}
		})
	}
}

func TestAuthoredRebaseReportsIntegrationWithoutChangingTheCandidate(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		for _, isBehind := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/behind_%t", strategy, isBehind), func(t *testing.T) {
				sctx := authoredStageContext(t, nil, config.Commands{})
				admitted := sctx.Run.HeadSHA
				sctx.Config.Rebase.Strategy = strategy
				if isBehind {
					gitCmd(t, sctx.WorkDir, "update-ref", "refs/heads/main", admitted)
					gitCmd(t, sctx.WorkDir, "checkout", "--detach")
					gitCmd(t, sctx.WorkDir, "checkout", "main")
				} else {
					gitCmd(t, sctx.WorkDir, "checkout", "main")
				}
				if err := os.WriteFile(filepath.Join(sctx.WorkDir, "upstream.txt"), []byte("new upstream\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, sctx.WorkDir, "add", "upstream.txt")
				gitCmd(t, sctx.WorkDir, "commit", "-m", "advance upstream")
				gitCmd(t, sctx.WorkDir, "checkout", "--detach", admitted)

				outcome, err := (&RebaseStep{}).Execute(sctx)

				if err != nil || !outcome.NeedsApproval || outcome.AutoFixable {
					t.Fatalf("integration did not report for authored source: %+v %v", outcome, err)
				}
				if gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != admitted || gitStatusPorcelain(t, sctx.WorkDir) != "" {
					t.Fatal("integration changed the authored candidate")
				}
			})
		}
	}
}

func TestAuthoredRepairBoundariesRefuseBeforeInvokingOrPublishing(t *testing.T) {
	for _, boundary := range []string{"repair agent", "formatter command", "agent commit", "CI commit", "publication", "push command"} {
		t.Run(boundary, func(t *testing.T) {
			ag := &mockAgent{name: "must-not-run", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("forbidden repair agent was invoked")
				return nil, nil
			}}
			sctx := authoredStageContext(t, ag, config.Commands{})
			admitted := sctx.Run.HeadSHA
			var err error

			switch boundary {
			case "repair agent":
				_, err = sctx.RunAgent(agent.RunOpts{CWD: sctx.WorkDir, Purpose: "review-fix"})
			case "formatter command":
				_, _, err = executeRepositoryCommand(sctx, "format", "echo formatter-repair>feature.txt")
			case "agent commit", "CI commit":
				if err := os.WriteFile(filepath.Join(sctx.WorkDir, "feature.txt"), []byte("leftover repair\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if boundary == "agent commit" {
					err = commitAgentFixes(sctx, types.StepDocument, "repair", "repair")
				} else {
					_, err = (&CIStep{}).commitRepair(sctx, "repair", nil, nil)
				}
			case "publication":
				err = publishRunHead(sctx, sctx.Run.BaseSHA, "", nil)
			case "push command":
				err = stepGitPushCommit(sctx, "unreachable", sctx.Run.BaseSHA, "refs/heads/feature", "", false)
			}

			if err == nil || !strings.Contains(err.Error(), "validation-only") {
				t.Fatalf("repair boundary did not refuse authored authority: %v", err)
			}
			if gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != admitted {
				t.Fatal("repair boundary committed source")
			}
			if boundary == "formatter command" && gitStatusPorcelain(t, sctx.WorkDir) != "" {
				t.Fatal("formatter command ran before its refusal")
			}
		})
	}
}

func TestAuthoredDescendantReconstructsReportOnlyAuthority(t *testing.T) {
	ag := &mockAgent{name: "editing-agent", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("forbidden descendant repair\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repaired source"}`)}, nil
	}}
	sctx := authoredStageContext(t, ag, config.Commands{})
	if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sctx.DB.ReadTerminalSnapshot(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := sctx.DB.GetValidationPredecessor(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "feature.txt"), []byte("new authored candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, sctx.WorkDir, "commit", "-am", "author descendant")
	request := previous.Request
	request.PredecessorID, request.SnapshotDigest, request.LaunchNonce = sctx.Run.ID, snapshot.Digest(), *snapshot.Run.LaunchNonce
	request.CandidateSHA = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	request.CandidateTree = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD^{tree}")
	event, _, err := sctx.DB.AcceptValidationAdmission(context.Background(), db.ValidationAdmission{Request: request, Snapshot: *snapshot, AssertionProof: previous.AssertionProof, WorktreeRoot: t.TempDir(), Operator: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := sctx.DB.GetRun(event.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := &pipeline.StepContext{Ctx: context.Background(), Agent: ag, WorkDir: sctx.WorkDir, DB: sctx.DB, Run: child, Repo: sctx.Repo, Shared: &pipeline.RunShared{}, Config: sctx.Config, Log: sctx.Log, LogChunk: sctx.LogChunk, LogFile: sctx.LogFile}

	_, err = (&DocumentStep{}).Execute(recovered)

	if err == nil || len(ag.calls) != 1 || gitCmd(t, recovered.WorkDir, "rev-parse", "HEAD") != request.CandidateSHA {
		t.Fatalf("reconstructed descendant granted normal-turn repairs: %v calls=%d", err, len(ag.calls))
	}
}
