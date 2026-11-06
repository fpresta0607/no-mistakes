package steps

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
}

func TestRebindPipelineAttestation_PreservesTestException(t *testing.T) {
	t.Parallel()
	oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("b", 40)
	original := pipelineAttestationCommentPrefix + `{"head_sha":"` + oldHead + `","steps":[{"step":"test","status":"completed","test_exception":"live validation verdict: inconclusive; approved retained checks with seven untested contracts"}],"live_validation":{"verdict":"inconclusive","live":1,"total":8}}` + pipelineAttestationCommentClosingToken
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
	reason := `Accept retained checks from C:\Users\private-person\work and /home/private-person/work`
	findings := `{"verdict":"inconclusive","findings":[]}`
	step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &reason}
	markdown, _ := BuildPipelineSummary([]*db.StepResult{step}, nil, testPipelineHeadSHA)
	published := redactPRContent(prContent{Body: markdown}, "", "").Body
	if strings.Contains(published, "private-person") {
		t.Fatalf("Test exception leaked a home path: %s", published)
	}
	attestation := parsePipelineAttestationForTest(t, published)
	if len(attestation.Steps) != 1 || !strings.Contains(attestation.Steps[0].TestException, "Accept retained checks") {
		t.Fatalf("publication lost the exception or damaged its JSON: %+v", attestation)
	}
}
