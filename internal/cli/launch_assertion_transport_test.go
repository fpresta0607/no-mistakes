package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLaunchAssertionCommandKeepsCapturedProofThroughReceipt(t *testing.T) {
	for _, path := range []string{"claim_receipt", "fresh_fallback"} {
		t.Run(path, func(t *testing.T) {
			expected := cliLaunchExpectation()
			expected.Profiles["primary"] = append(expected.Profiles["primary"], agentcfg.Selection{Harness: "codex", Model: "fixture/fallback", Effort: agentcfg.EffortHigh, ServiceTier: "fast"})
			expected.Profiles["reviewer_after_round"] = []agentcfg.Selection{{Harness: "codex", Model: "fixture-reviewer", Effort: agentcfg.EffortMedium, ServiceTier: "default"}}
			expected.Profiles["fixer_after_round"] = []agentcfg.Selection{{Harness: "codex", Model: "fixture-fixer", Effort: agentcfg.EffortLow, ServiceTier: "fast"}}
			proof, err := expected.Verify(expected.TrustedSHA, expected.Profiles)
			if err != nil {
				t.Fatal(err)
			}
			filePath := filepath.Join(t.TempDir(), "expected.json")
			originalBytes, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			replacement := cliLaunchExpectation()
			replacement.TrustedSHA = strings.Repeat("b", 40)
			replacement.Profiles["primary"][0].Model = "file-was-replaced"
			replacementBytes, err := json.Marshal(replacement)
			if err != nil {
				t.Fatal(err)
			}
			claims := make(chan ipc.ClaimLaunchReceiptParams, 128)
			freshRequests := make(chan ipc.StartFreshRunParams, 1)
			var variant atomic.Int32
			var probes atomic.Int32
			launched := olderDaemonFixture(t, nil, func(server *ipc.Server) {
				server.HandleStream(ipc.MethodSubscribe, hangSubscribe)
				server.Handle(ipc.MethodProbeLaunchAssertion, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var request ipc.ProbeLaunchAssertionParams
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					if !request.LaunchAssertion.Matches(expected) {
						return nil, fmt.Errorf("fixture capability received a changed capture")
					}
					if err := os.WriteFile(filePath, replacementBytes, 0o600); err != nil {
						return nil, err
					}
					probes.Add(1)
					return &ipc.ProbeLaunchAssertionResult{AssertionDigest: expected.Digest()}, nil
				})
				server.Handle(ipc.MethodClaimLaunchReceipt, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var request ipc.ClaimLaunchReceiptParams
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					claims <- request
					if path == "fresh_fallback" {
						return &ipc.ClaimLaunchReceiptResult{}, nil
					}
					receiptProof := proof
					if variant.Load() == 1 {
						receiptProof = nil
					} else if variant.Load() == 2 {
						conflicting := *proof
						conflicting.AssertionDigest = replacement.Digest()
						receiptProof = &conflicting
					}
					return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{
						RunID: "captured-run", Disposition: "reused", LaunchNonce: request.LaunchNonce, ValidationGeneration: request.ValidationGeneration,
						Branch: request.Branch, HeadSHA: request.SubmittedHeadSHA, SubmittedHeadSHA: request.SubmittedHeadSHA,
						IntentDigest: request.IntentDigest, LaunchAssertionProof: receiptProof,
					}}, nil
				})
				server.Handle(ipc.MethodStartFreshRun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var request ipc.StartFreshRunParams
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					freshRequests <- request
					return &ipc.StartFreshRunResult{Receipt: ipc.LaunchReceipt{
						RunID: "captured-run", Disposition: "created", LaunchNonce: request.LaunchNonce, ValidationGeneration: request.ValidationGeneration,
						Branch: request.Branch, HeadSHA: request.HeadSHA, SubmittedHeadSHA: request.HeadSHA,
						IntentDigest: digestLaunchIntent(request.Intent), LaunchAssertionProof: proof,
					}}, nil
				})
				server.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "captured-run", Status: types.RunCompleted}}, nil
				})
			})
			local, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			root, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(root.DB())
			if err != nil {
				t.Fatal(err)
			}
			repository, err := findRepo(database)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			optionsPath := filepath.Join(t.TempDir(), "push-options.txt")
			if path == "fresh_fallback" {
				gateDirectory := root.RepoDir(repository.ID)
				cliGit(t, local, "clone", "--bare", local, gateDirectory)
				cliGit(t, gateDirectory, "config", "receive.advertisePushOptions", "true")
				cliGit(t, local, "remote", "add", gate.RemoteName, gateDirectory)
				quotedOptionsPath := "'" + strings.ReplaceAll(filepath.ToSlash(optionsPath), "'", "'\\''") + "'"
				hook := "#!/bin/sh\n: > " + quotedOptionsPath + "\ni=0\nwhile [ \"$i\" -lt \"$GIT_PUSH_OPTION_COUNT\" ]; do\n  eval \"option=\\${GIT_PUSH_OPTION_$i}\"\n  printf '%s\\n' \"$option\" >> " + quotedOptionsPath + "\n  i=$((i+1))\ndone\n"
				if err := os.WriteFile(filepath.Join(gateDirectory, "hooks", "post-receive"), []byte(hook), 0o755); err != nil {
					t.Fatal(err)
				}
				cliGit(t, local, "commit", "--allow-empty", "-m", "captured fresh head")
			}
			head := cliGit(t, local, "rev-parse", "HEAD")
			branch := cliGit(t, local, "branch", "--show-current")
			beforeRefs := cliGit(t, local, "for-each-ref", "--format=%(refname) %(objectname)")
			const intent = "  preserve captured intent\n\n"
			variants := 1
			if path == "claim_receipt" {
				variants = 3
			}
			for index := range variants {
				variant.Store(int32(index))
				if err := os.WriteFile(filePath, originalBytes, 0o600); err != nil {
					t.Fatal(err)
				}
				command := newAxiRunCmd()
				command.SetContext(t.Context())
				command.SetArgs([]string{"--intent", intent, "--launch-assertion", filePath, "--launch-nonce", "captured-nonce", "--validation-generation", "captured-generation"})
				var output bytes.Buffer
				command.SetOut(&output)
				command.SetErr(&output)
				commandError := command.Execute()
				if index == 0 {
					if commandError != nil {
						t.Fatalf("matching captured receipt command failed: %v\n%s", commandError, output.String())
					}
					for _, fact := range []string{
						"launch_receipt:\n", "launch_nonce: captured-nonce", "validation_generation: captured-generation",
						"branch: " + branch, "head_sha: " + head, "submitted_head_sha: " + head,
						strings.TrimSpace(axiDoc(toon.Field{Key: "intent_digest", Value: digestLaunchIntent(intent)})),
						"assertion_digest: " + expected.Digest(), "trusted_sha: " + expected.TrustedSHA,
						"primary[2]{harness,model,effort,service_tier}:\n        codex,fixture,xhigh,default\n        codex,fixture/fallback,high,fast",
						"reviewer[1]{harness,model,effort,service_tier}:\n        codex,fixture,xhigh,default",
						"fixer[1]{harness,model,effort,service_tier}:\n        codex,fixture,xhigh,default",
						"reviewer_after_round[1]{harness,model,effort,service_tier}:\n        codex,fixture-reviewer,medium,default",
						"fixer_after_round[1]{harness,model,effort,service_tier}:\n        codex,fixture-fixer,low,fast",
					} {
						if !strings.Contains(output.String(), fact) {
							t.Errorf("receipt omitted structured captured fact %q:\n%s", fact, output.String())
						}
					}
				} else if commandError == nil || !strings.Contains(output.String(), "missing or conflicting native launch proof") || strings.Contains(output.String(), "launch_receipt:") {
					t.Errorf("missing/conflicting receipt proof was accepted: variant=%d error=%v\n%s", index, commandError, output.String())
				}
				data, err := os.ReadFile(filePath)
				if err != nil || !bytes.Equal(data, replacementBytes) || probes.Load() != int32(index+1) {
					t.Fatalf("capture replacement premise was not reached: probes=%d, error=%v", probes.Load(), err)
				}
				claimCount := len(claims)
				if claimCount == 0 {
					t.Fatal("command did not transport a claim")
				}
				for range claimCount {
					request := <-claims
					if !reflect.DeepEqual(request.LaunchAssertion, expected) || request.RepoID != repository.ID || request.Branch != branch || request.SubmittedHeadSHA != head ||
						request.LaunchNonce != "captured-nonce" || request.ValidationGeneration != "captured-generation" || request.IntentDigest != digestLaunchIntent(intent) {
						t.Errorf("claim changed immutable capture/identity: %+v", request)
					}
				}
			}
			if len(*launched) != 0 {
				t.Errorf("command entered an unrelated launch method: %v", *launched)
			}
			if path == "claim_receipt" {
				if len(freshRequests) != 0 || cliGit(t, local, "for-each-ref", "--format=%(refname) %(objectname)") != beforeRefs {
					t.Error("receipt claim/rejection started a new launch or changed custody")
				}
			} else {
				if len(freshRequests) != 1 {
					t.Fatalf("fresh fallback count=%d, want exactly one", len(freshRequests))
				}
				request := <-freshRequests
				if !reflect.DeepEqual(request.LaunchAssertion, expected) || request.RepoID != repository.ID || request.Branch != branch || request.HeadSHA != head || request.Intent != intent ||
					request.LaunchNonce != "captured-nonce" || request.ValidationGeneration != "captured-generation" {
					t.Errorf("fresh fallback changed the captured source/profile/identity: %+v", request)
				}
				data, err := os.ReadFile(optionsPath)
				if err != nil {
					t.Fatalf("owned Git hook did not observe the push options: %v", err)
				}
				options := strings.Split(strings.TrimSpace(string(data)), "\n")
				pushed, err := parseLaunchAssertionPushOptions(options)
				if err != nil || !reflect.DeepEqual(pushed, expected) {
					t.Errorf("real local Git push changed the captured assertion: %+v, error=%v", pushed, err)
				}
				for _, required := range []string{formatIntentPushOption(intent), formatLaunchNoncePushOption("captured-nonce"), formatValidationGenerationPushOption("captured-generation")} {
					count := 0
					for _, option := range options {
						if option == required {
							count++
						}
					}
					if count != 1 {
						t.Errorf("push option binding %q count=%d, want one", required, count)
					}
				}
			}
		})
	}
}
