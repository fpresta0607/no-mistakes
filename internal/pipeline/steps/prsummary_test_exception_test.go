package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBuildPipelineAttestation_CarriesTestExceptionSeparately(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, findings, commandOverride string
		isException                     bool
	}{
		{"inconclusive", `{"verdict":"inconclusive","findings":[]}`, "", true},
		{"no-go", `{"verdict":"no-go","findings":[]}`, "", true},
		{"agent-budget", `{"findings":[{"id":"` + types.FindingIDTestAgentTimeout + `","severity":"warning","description":"budget exhausted"}]}`, "", true},
		{"command", `{"findings":[]}`, "configured test command failed with exit code 7; Fixes #14", true},
		{"no-surface", `{"verdict":"no-surface","findings":[]}`, "", false},
		{"clean", `{"verdict":"go","findings":[]}`, "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reason := "Accept retained checks; seven repair contracts remain untested; Fixes #12"
			step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &testCase.findings, ApprovalReason: &reason}
			if testCase.commandOverride != "" {
				step.OverrideReason = &testCase.commandOverride
			}
			raw := buildPipelineAttestationWithPolicy([]*db.StepResult{step}, nil, testPipelineHeadSHA, pipelineAttestationPolicy{})
			if strings.Contains(raw, "Fixes #12") || strings.Contains(raw, "Fixes #14") {
				t.Fatal("serialized Test metadata contains an active closing reference")
			}
			var published struct {
				Steps []struct {
					TestException  string `json:"test_exception"`
					OverrideReason string `json:"override_reason"`
				} `json:"steps"`
				AllowTestCommandOverride string `json:"allow_test_command_override"`
			}
			payload := strings.TrimSuffix(strings.TrimPrefix(raw, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
			if err := json.Unmarshal([]byte(payload), &published); err != nil {
				t.Fatal(err)
			}
			if len(published.Steps) != 1 {
				t.Fatalf("published steps = %+v", published.Steps)
			}
			exception := published.Steps[0].TestException
			if testCase.isException && !strings.Contains(exception, reason) {
				t.Fatalf("stored approval omitted from test_exception: %s", raw)
			}
			if !testCase.isException && exception != "" {
				t.Fatalf("ordinary approval became an exception: %s", raw)
			}
			if published.Steps[0].OverrideReason != testCase.commandOverride || published.AllowTestCommandOverride != "" {
				t.Fatalf("broader exception changed command-only policy: %s", raw)
			}
		})
	}
}

func TestBuildPipelineSummary_RendersTestException(t *testing.T) {
	t.Parallel()
	reason := "Accept retained checks; seven repair contracts remain untested"
	findings := `{"verdict":"inconclusive","findings":[],"scenarios":[{"name":"repair scope","result":"untested","live":false,"reason":"fixture unavailable"}]}`
	step := &db.StepResult{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &reason}
	rounds := map[string][]*db.StepRound{"test": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}
	markdown, _ := BuildPipelineSummary([]*db.StepResult{step}, rounds, testPipelineHeadSHA)
	for _, expected := range []string{"### Test approval exception", reason, "live validation verdict: inconclusive", "repair scope", "untested"} {
		if !strings.Contains(markdown, expected) {
			t.Fatalf("human record omitted %q: %s", expected, markdown)
		}
	}
	expectedSection := "<!-- no-mistakes-test-exception:v1 -->\n### Test approval exception\n\n> live validation verdict: inconclusive\n> Test exception approved: Accept retained checks; seven repair contracts remain untested\n<!-- /no-mistakes-test-exception:v1 -->"
	attestation := buildPipelineAttestation([]*db.StepResult{step}, rounds, testPipelineHeadSHA)
	minimumBudget := len(attestation) + len("\n\n") + len(expectedSection)
	largePipeline := markdown + "\n\n### Historical rounds\n\n" + strings.Repeat("original failure record\n", 6000)
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		for _, limit := range []int{1600, minimumBudget, minimumBudget - 1} {
			for _, isByteBudget := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/bytes=%v", mode, limit, isByteBudget), func(t *testing.T) {
					sctx := &pipeline.StepContext{Config: &config.Config{PR: config.PR{Appendix: mode}}}
					narrative := "## What Changed\n\n" + strings.Repeat("change 😀\n", 1000)
					var body string
					if isByteBudget {
						body = buildPRBodyWithin(narrative, "low risk", "## Testing\n\n"+strings.Repeat("retained evidence\n", 1000), largePipeline, sctx, scm.ProviderGitHub, limit)
					} else {
						body = assembleDraftPRBody(sctx, narrative, "low risk", "", largePipeline, "", limit, scm.ProviderAzureDevOps)
					}
					if !strings.Contains(body, expectedSection) || strings.Count(body, testExceptionStart) != 1 || !strings.Contains(body, attestation) {
						t.Fatalf("required exception metadata was dropped or split:\n%s", body)
					}
					if limit < minimumBudget {
						if err := validateOwnedPRBudget(body, limit); err == nil {
							t.Fatal("publication accepted a budget smaller than the required metadata")
						}
					} else if (isByteBudget && len(body) > limit) || (!isByteBudget && scm.PRBodyLen(body) > limit) {
						t.Fatalf("required metadata fits but body exceeds %d: %d bytes, %d characters", limit, len(body), scm.PRBodyLen(body))
					}
				})
			}
		}
		for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
			t.Run(fmt.Sprintf("owned/%s/%s", mode, provider), func(t *testing.T) {
				pipelineMD, _ := BuildPipelineSummaryFor([]*db.StepResult{step}, rounds, testPipelineHeadSHA, provider)
				if provider == scm.ProviderBitbucket {
					pipelineMD += "\n\n```text\n" + attestation + "\n```"
				}
				appendix := appendixEvidence(mode, prBodyFlavorFor(provider), "low risk", "", pipelineMD, provider)
				content, err := composeOwnedPRContent(prOwnedBody{before: "Author narrative"}, "title", appendix, 0, "", "")
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(content.Body, expectedSection) || !strings.Contains(content.Body, attestation) {
					t.Fatalf("owned appendix omitted exception metadata: %s", content.Body)
				}
				if _, err := parsePROwnedBody(content.Body); err != nil {
					t.Fatal(err)
				}
				if _, err := composeOwnedPRContent(prOwnedBody{before: "Author narrative"}, "title", appendix, minimumBudget-1, "", ""); err == nil {
					t.Fatal("owned publication dropped required metadata to fit")
				}
			})
		}
		for _, approval := range []string{strings.Repeat("x", 33000), strings.Repeat("界", 13000)} {
			t.Run(fmt.Sprintf("internal-byte-cap/%s/%d", mode, len(approval)), func(t *testing.T) {
				largeStep := *step
				largeStep.ApprovalReason = &approval
				pipelineMD, _ := BuildPipelineSummary([]*db.StepResult{&largeStep}, nil, testPipelineHeadSHA)
				sctx := &pipeline.StepContext{Config: &config.Config{PR: config.PR{Appendix: mode}}}
				body := assembleDraftPRBody(sctx, "## What Changed\n\nRetain the exception", "low risk", "", pipelineMD, "", 100000, scm.ProviderGitHub)
				if !strings.Contains(body, extractPipelineAttestationMarker(pipelineMD)) || !strings.Contains(body, extractTestExceptionSection(pipelineMD)) {
					t.Fatal("assembly dropped required metadata at the internal byte cap")
				}
				if err := validateOwnedPRBudget(body, 100000); err == nil {
					t.Fatal("publication accepted required metadata larger than the internal byte cap")
				}
			})
		}
	}
	t.Run("closing-references", func(t *testing.T) {
		reason := "Fixes #12; Resolves group/sub#13; Implements #14; Fixes https://gitlab.com/group/repo/-/issues/15"
		step := *step
		step.ApprovalReason = &reason
		for _, provider := range []scm.Provider{scm.ProviderGitLab, scm.ProviderGitHub} {
			markdown, _ := BuildPipelineSummaryFor([]*db.StepResult{&step}, nil, testPipelineHeadSHA, provider)
			marker := extractPipelineAttestationMarker(markdown)
			section := extractTestExceptionSection(markdown)
			for _, phrase := range []string{"Fixes #12", "Resolves group/sub#13", "Implements #14", "Fixes https://gitlab.com/group/repo/-/issues/15"} {
				if strings.Contains(marker, phrase) || (provider == scm.ProviderGitLab && strings.Contains(section, phrase)) {
					t.Fatalf("generated metadata can close an issue on %s: %s", provider, phrase)
				}
			}
			if provider == scm.ProviderGitLab && !strings.Contains(html.UnescapeString(section), reason) {
				t.Fatal("human exception lost the recorded reason")
			}
			if provider == scm.ProviderGitHub && !strings.Contains(section, "Fixes `#12`") {
				t.Fatal("GitHub human exception did not protect the reference")
			}
			published := parsePipelineAttestationForTest(t, markdown)
			if published.Steps[0].TestException != "live validation verdict: inconclusive\nTest exception approved: "+reason || published.Steps[0].OverrideReason != "" || published.AllowTestCommandOverride != "" {
				t.Fatalf("closing protection changed the decoded record: %+v", published)
			}
		}
	})
}

func TestRebindPipelineAttestation_PreservesTestException(t *testing.T) {
	t.Parallel()
	oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	humanSection := "<!-- no-mistakes-test-exception:v1 -->\n### Test approval exception\n\n> live validation verdict: inconclusive; approved retained checks with seven untested contracts\n<!-- /no-mistakes-test-exception:v1 -->"
	original := pipelineAttestationCommentPrefix + `{"head_sha":"` + oldHead + `","steps":[{"step":"test","status":"completed","test_exception":"live validation verdict: inconclusive; approved retained checks with seven untested contracts"}],"live_validation":{"verdict":"inconclusive","live":1,"total":8}}` + pipelineAttestationCommentClosingToken + "\n\n" + humanSection
	for _, testCase := range []struct {
		name  string
		steps []*db.StepResult
	}{
		{"retained", nil},
		{"fresh-clean", []*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rebound, isRebound := rebindPipelineAttestationWithSteps(original, newHead, testCase.steps, pipelineAttestationPolicy{})
			if !isRebound || !strings.Contains(rebound, newHead) {
				t.Fatalf("restamp failed: %s", rebound)
			}
			if strings.Contains(rebound, "test_exception") != (testCase.steps == nil) {
				t.Fatalf("restamp lost the retained exception or copied it over fresh steps: %s", rebound)
			}
			if testCase.steps == nil && !strings.Contains(rebound, humanSection) {
				t.Fatalf("nil-step restamp lost the human exception: %s", rebound)
			}
			if strings.Contains(rebound, "live_validation") || strings.Contains(rebound, "override_reason") || strings.Contains(rebound, "allow_test_command_override") {
				t.Fatalf("restamp invented validation or a command waiver: %s", rebound)
			}
		})
	}
	for _, isOwned := range []bool{false, true} {
		for _, isRetained := range []bool{false, true} {
			t.Run(fmt.Sprintf("publication/owned=%v/retained=%v", isOwned, isRetained), func(t *testing.T) {
				root := "D:/private/R&D task/run"
				reason := "Fixes #12 at " + root + "/proof and /home/private-person/work"
				findings := `{"verdict":"inconclusive","findings":[]}`
				steps := []*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &reason}}
				record := pipelineAttestation{HeadSHA: oldHead, Steps: []pipelineAttestationStep{{Step: types.StepTest, Status: types.StepStatusCompleted}}}
				if isRetained {
					record.Steps[0].TestException = "live validation verdict: inconclusive\nTest exception approved: " + reason
					steps = nil
				}
				payload, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				marker := pipelineAttestationCommentPrefix + string(payload) + pipelineAttestationCommentClosingToken
				if isOwned {
					marker = wrapPRAppendix(marker)
				}
				prefix, suffix := "Author keeps /home/author-private/work and Fixes #90\n\n", "\n\nAuthor suffix"
				host := &attestationTestHost{body: prefix + marker + suffix, title: "Author title", provider: scm.ProviderGitLab}
				policy := attestationPolicyFrom(&pipeline.StepContext{WorkDir: root})
				if err := restampPRAttestationWithSteps(context.Background(), host, &scm.PR{Number: "42"}, newHead, steps, nil, policy); err != nil {
					t.Fatal(err)
				}
				if host.updates != 1 || host.title != "Author title" || !strings.HasPrefix(host.body, prefix) || !strings.HasSuffix(host.body, suffix) {
					t.Fatal("restamp changed author bytes or failed to publish")
				}
				if isOwned {
					if _, err := parsePROwnedBody(host.body); err != nil {
						t.Fatal(err)
					}
				}
				published := parsePipelineAttestationForTest(t, host.body)
				if published.Steps[0].TestException != "live validation verdict: inconclusive\nTest exception approved: Fixes #12 at {run-worktree}/proof and ~/work" || published.LiveValidation != nil || published.Steps[0].OverrideReason != "" || published.AllowTestCommandOverride != "" {
					t.Fatalf("restamp changed the decoded exception or waiver semantics: %+v", published)
				}
				publicMarker := extractPipelineAttestationMarker(host.body)
				for _, private := range []string{"D:/private", "private-person", "Fixes #12"} {
					if strings.Contains(publicMarker, private) {
						t.Fatalf("restamp published unsafe metadata: %s", publicMarker)
					}
				}
			})
		}
		for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderAzureDevOps} {
			t.Run(fmt.Sprintf("budget/owned=%v/%s", isOwned, provider), func(t *testing.T) {
				marker := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted}}, nil, oldHead)
				if isOwned {
					marker = wrapPRAppendix(marker)
				}
				prefix, approval := "", strings.Repeat("x", 4000)
				if provider == scm.ProviderGitHub {
					prefix, approval = strings.Repeat("author-byte-contract\n", 2100), strings.Repeat("x", 33000)
				}
				findings := `{"verdict":"inconclusive","findings":[]}`
				steps := []*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &approval}}
				host := &attestationTestHost{body: prefix + marker, provider: provider}
				if err := restampPRAttestationWithSteps(context.Background(), host, &scm.PR{Number: "42"}, newHead, steps, nil, pipelineAttestationPolicy{}); err == nil || host.updates != 0 {
					t.Fatalf("restamp wrote past the publication budget: err=%v, writes=%d", err, host.updates)
				}
			})
		}
	}
}

func TestBuildPipelineSummary_QuotedTestExceptionCannotOwnTheRecord(t *testing.T) {
	t.Parallel()
	reason := "Accept retained checks; live repair coverage is incomplete"
	testFindings := `{"verdict":"inconclusive","findings":[]}`
	foreign, err := json.Marshal(types.Findings{Items: []types.Finding{{Severity: types.FindingSeverityWarning, Description: testExceptionStart + "\nforeign approval\n" + testExceptionEnd}}})
	if err != nil {
		t.Fatal(err)
	}
	foreignFindings := string(foreign)
	steps := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &foreignFindings},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &testFindings, ApprovalReason: &reason},
	}
	rounds := map[string][]*db.StepRound{"review": {{Round: 1, Trigger: "initial", FindingsJSON: &foreignFindings}}}
	markdown, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if strings.Count(markdown, testExceptionStart) != 1 || strings.Count(markdown, testExceptionEnd) != 1 || !strings.Contains(markdown, "foreign approval") {
		t.Fatalf("quoted record acquired exception ownership or disappeared: %s", markdown)
	}
}

func TestBuildPipelineSummary_TestExceptionIsRedactedAtPublication(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"D:/private/R&D/run", `D:\private\R&D\run`, "D:/private/O'Brien/run", "D:/private/<task>/run", "D:/private/R&D task/run"} {
		t.Run(root, func(t *testing.T) {
			reason := `Accept retained checks from C:\Users\private-person\work and /home/private-person/work under ` + root + "/evidence"
			findings := `{"verdict":"inconclusive","findings":[]}`
			step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &reason}
			markdown, _ := BuildPipelineSummary([]*db.StepResult{step}, nil, testPipelineHeadSHA)
			for _, isOwned := range []bool{false, true} {
				published := redactPRContent(prContent{Body: markdown}, root, "").Body
				if isOwned {
					content, err := composeOwnedPRContent(prOwnedBody{before: "Author narrative"}, "title", markdown, 0, root, "")
					if err != nil {
						t.Fatal(err)
					}
					published = content.Body
					if _, err := parsePROwnedBody(published); err != nil {
						t.Fatal(err)
					}
				}
				for _, private := range []string{"private-person", "D:/private", `D:\private`, `D:\\private`} {
					if strings.Contains(published, private) {
						t.Fatalf("Test exception leaked a private path: %s", published)
					}
				}
				attestation := parsePipelineAttestationForTest(t, published)
				if len(attestation.Steps) != 1 || attestation.Steps[0].TestException != "live validation verdict: inconclusive\nTest exception approved: Accept retained checks from ~\\work and ~/work under {run-worktree}/evidence" {
					t.Fatalf("publication lost the exception or damaged its JSON: %+v", attestation)
				}
			}
		})
	}
}

func TestRestampOfALegacyCommandOverrideLeavesTheExceptionToRefresh(t *testing.T) {
	t.Parallel()
	oldHead := strings.Repeat("a", 40)
	commandOverride := "configured test command failed with exit code 7"
	for _, isOwned := range []bool{false, true} {
		for _, isApproved := range []bool{false, true} {
			t.Run(fmt.Sprintf("owned=%v/approved=%v", isOwned, isApproved), func(t *testing.T) {
				// Arrange: a body published before test_exception existed.
				marker := pipelineAttestationCommentPrefix + `{"head_sha":"` + oldHead + `","steps":[{"step":"test","status":"completed","override_reason":"` + commandOverride + `"}]}` + pipelineAttestationCommentClosingToken
				if isOwned {
					marker = wrapPRAppendix(marker)
				}
				prefix, suffix := "Author narrative\n\n", "\n\nAuthor suffix"
				restampHost := &attestationTestHost{body: prefix + marker + suffix, title: "Author title"}

				// Act: a CI repair restamp that did not re-run Test.
				if err := restampPRAttestationWithSteps(context.Background(), restampHost, &scm.PR{Number: "17"}, testPipelineHeadSHA, nil, nil, pipelineAttestationPolicy{}); err != nil {
					t.Fatal(err)
				}

				// Assert: the restamp keeps the legacy record as it was.
				restamped := parsePipelineAttestationForTest(t, restampHost.body)
				if restamped.HeadSHA != testPipelineHeadSHA || restamped.Steps[0].OverrideReason != commandOverride {
					t.Fatalf("restamp lost the head or the command override: %+v", restamped)
				}
				if restamped.Steps[0].TestException != "" || strings.Contains(restampHost.body, testExceptionStart) {
					t.Fatalf("nil-step restamp invented an exception record: %s", restampHost.body)
				}

				// Act: the first exact-head refresh of that restamped body.
				override := commandOverride
				step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &override}
				if isApproved {
					reason := "Accept the retained failure for this release"
					step.ApprovalReason = &reason
				}
				url := "https://github.com/owner/repo/pull/17"
				refreshHost := &refreshPRHost{pr: scm.PR{Number: "17", URL: url}, body: restampHost.body, state: scm.PRStateOpen, observedHead: testPipelineHeadSHA}
				sctx := &pipeline.StepContext{Ctx: context.Background(), Run: &db.Run{Branch: "feature", HeadSHA: testPipelineHeadSHA, PRURL: &url}}
				isChanged, err := refreshPublishedPRWithHost(sctx, refreshHost, []*db.StepResult{step})

				// Assert: refresh adds both representations and stays idempotent.
				if err != nil || !isChanged || refreshHost.writes != 1 {
					t.Fatalf("refresh refused the restamped legacy body: changed=%v error=%v writes=%d", isChanged, err, refreshHost.writes)
				}
				refreshed := parsePipelineAttestationForTest(t, refreshHost.body)
				if refreshed.Steps[0].TestException != step.TestOverrideReason() || refreshed.Steps[0].OverrideReason != commandOverride {
					t.Fatalf("refresh published the wrong exception record: %+v", refreshed)
				}
				if section := extractTestExceptionSection(refreshHost.body); section == "" || !strings.Contains(section, commandOverride) {
					t.Fatalf("refresh did not add the human exception: %s", refreshHost.body)
				}
				if !strings.HasPrefix(refreshHost.body, prefix) || (isOwned && !strings.Contains(refreshHost.body, suffix)) {
					t.Fatalf("refresh changed author bytes: %s", refreshHost.body)
				}
				isChanged, err = refreshPublishedPRWithHost(sctx, refreshHost, []*db.StepResult{step})
				if err != nil || isChanged || refreshHost.writes != 1 {
					t.Fatalf("second refresh was not a no-op: changed=%v error=%v writes=%d", isChanged, err, refreshHost.writes)
				}
			})
		}
	}
}
