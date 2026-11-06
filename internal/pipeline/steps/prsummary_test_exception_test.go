package steps

import (
	"encoding/json"
	"fmt"
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
		{"command", `{"findings":[]}`, "configured test command failed with exit code 7", true},
		{"no-surface", `{"verdict":"no-surface","findings":[]}`, "", false},
		{"clean", `{"verdict":"go","findings":[]}`, "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reason := "Accept retained checks; seven repair contracts remain untested"
			step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &testCase.findings, ApprovalReason: &reason}
			if testCase.commandOverride != "" {
				step.OverrideReason = &testCase.commandOverride
			}
			raw := buildPipelineAttestationWithPolicy([]*db.StepResult{step}, nil, testPipelineHeadSHA, pipelineAttestationPolicy{})
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
	}
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
	for _, root := range []string{"D:/private/R&D/run", `D:\private\R&D\run`, "D:/private/O'Brien/run", "D:/private/<task>/run"} {
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
