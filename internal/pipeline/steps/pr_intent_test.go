package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func publishValidationTextForTest(sctx *pipeline.StepContext) {
	shouldPublish := true
	sctx.Config.PR.PublishIntent = &shouldPublish
}

func TestPRPrivateValidationIntentIsNotPublishedByDefault(t *testing.T) {
	t.Parallel()
	const privateIntent = "Fix the request parser.\nExecution: use the internal deployment runbook.\nAuthentication scope: private-service only."
	for _, policy := range []string{"pr: {}", "pr: {publish_intent: false}"} {
		for _, path := range []string{"ordinary", "fallback", "template", "title"} {
			t.Run(policy+"/"+path, func(t *testing.T) {
				sctx, ag, _ := templateTestContext(t)
				if path != "template" {
					sctx.Config.PR.Template = ""
				}
				trusted, err := config.LoadRepoFromBytes([]byte(policy))
				if err != nil {
					t.Fatal(err)
				}
				pushed, err := config.LoadRepoFromBytes([]byte("pr: {publish_intent: true}"))
				if err != nil {
					t.Fatal(err)
				}
				sctx.Config.PR.PublishIntent = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(pushed, trusted, true)).PR.PublishIntent
				sctx.UserIntent = privateIntent
				sctx.IntentSource = db.RunIntentSourceAgent
				sctx.ClosingIssueRefs = []string{"50"}
				if err := sctx.DB.UpdateRunIntent(sctx.Run.ID, db.RunIntent{Summary: privateIntent, Source: db.RunIntentSourceAgent}); err != nil {
					t.Fatal(err)
				}
				insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{TestingSummary: "Request parser regression passed."}), "")
				if path == "fallback" {
					ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
						return nil, errors.New("draft unavailable")
					}
				}

				var content prContent
				if path == "title" {
					content.Title, err = (&PRStep{}).draftConfiguredPRTitle(sctx, "feature", "main", sctx.Run.BaseSHA)
				} else {
					content, err = (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
				}

				if err != nil {
					t.Fatal(err)
				}
				if len(ag.calls) == 0 {
					t.Fatal("no public drafting turn ran")
				}
				for _, call := range ag.calls {
					if strings.Contains(call.Prompt, privateIntent) || strings.Contains(call.Prompt, "USER INTENT") {
						t.Error("public drafting turn received private validation intent")
					}
				}
				if strings.Contains(content.Body, "## Intent") || strings.Contains(content.Body, "Authentication scope:") {
					t.Error("PR body published operational instructions")
				}
				if path != "title" {
					if strings.Contains(content.Body, "Request parser regression passed.") || !strings.Contains(content.Body, "Closes #50") {
						t.Error("public body exposed validation text or lost the requested issue reference")
					}
					if got := parsePipelineAttestationForTest(t, content.Body).HeadSHA; got != sctx.Run.HeadSHA {
						t.Errorf("attestation head = %q, want %q", got, sctx.Run.HeadSHA)
					}
				}
				stored, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Intent == nil || *stored.Intent != privateIntent || stored.IntentSource == nil || *stored.IntentSource != db.RunIntentSourceAgent {
					t.Fatal("publication changed the stored validation intent or source")
				}
				if !strings.Contains(userIntentPromptSection(sctx), privateIntent) || !intentSourceIsAuthoritative(sctx) {
					t.Fatal("private validation lost its complete authoritative requirements")
				}
			})
		}
	}
}

func TestPRIntentWithoutConfigurationStaysPrivate(t *testing.T) {
	t.Parallel()
	for _, sctx := range []*pipeline.StepContext{nil, {UserIntent: "private brief"}} {
		if publicPRIntent(sctx) != "" || prDraftIntentPromptSection(sctx) != "" {
			t.Fatal("missing configuration permitted intent publication")
		}
	}
}

func TestPRPrivateIntentCannotLeakThroughRecordedValidation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		for _, path := range []string{"ordinary", "fallback", "template", "supplied", "keep", "carried-keep"} {
			t.Run(mode+"/"+path, func(t *testing.T) {
				sctx, ag, _ := templateTestContext(t)
				if path != "template" {
					sctx.Config.PR.Template = ""
				}
				sctx.Config.PR.PublishIntent = nil
				sctx.Config.PR.Appendix = mode
				sctx.UserIntent = "Require the private Project Cedar launch criterion."
				sctx.IntentSource = db.RunIntentSourceAgent
				leakedText := "Project Cedar must wait for the confidential launch window"
				review := findingsJSON(t, types.Findings{
					RiskLevel: "high", RiskRationale: leakedText,
					Items: []types.Finding{{ID: "r1", Severity: "warning", Action: "ask-user", Description: leakedText}},
				})
				initialReview := findingsJSON(t, types.Findings{Items: []types.Finding{
					{ID: "r1", Severity: "warning", Description: leakedText},
					{ID: "r2", Severity: "error", Description: leakedText},
				}})
				steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil || len(steps) != 1 || steps[0].StepName != types.StepReview {
					t.Fatalf("expected the template fixture's Review step: %+v, %v", steps, err)
				}
				reviewStep := steps[0]
				if _, err := sctx.DB.InsertStepRound(reviewStep.ID, 1, "initial", &initialReview, nil, 10); err != nil {
					t.Fatal(err)
				}
				if _, err := sctx.DB.InsertStepRound(reviewStep.ID, 2, "auto_fix", &review, &leakedText, 10); err != nil {
					t.Fatal(err)
				}
				if err := sctx.DB.SetStepFindings(reviewStep.ID, review); err != nil {
					t.Fatal(err)
				}
				if path == "carried-keep" {
					source, err := sctx.DB.GetStepResult(reviewStep.ID)
					if err != nil {
						t.Fatal(err)
					}
					retry, err := sctx.DB.InsertRun(sctx.Repo.ID, sctx.Run.Branch, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := sctx.DB.InsertCarriedStepResult(retry.ID, source); err != nil {
						t.Fatal(err)
					}
					sctx.Run = retry
				}
				testing := findingsJSON(t, types.Findings{
					Verdict: "go", TestedHeadSHA: sctx.Run.HeadSHA,
					TestingSummary: leakedText, Tested: []string{leakedText},
					Scenarios: []types.TestScenario{{Name: leakedText, Result: "pass", Live: true, Evidence: leakedText}},
					Artifacts: []types.TestArtifact{{Kind: "log", Label: leakedText, Content: leakedText}},
				})
				step := insertCompletedStep(t, sctx, types.StepTest, testing, "")
				if _, err := sctx.DB.InsertStepRound(step.ID, 2, "auto_fix", &testing, &leakedText, 10); err != nil {
					t.Fatal(err)
				}
				insertCompletedStep(t, sctx, types.StepDocument, "", leakedText)
				if path == "fallback" {
					ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
						return nil, errors.New("draft unavailable")
					}
				}
				if path == "keep" || path == "carried-keep" {
					sctx.Config.PR.UpdateText = "keep"
				}
				var content prContent
				if path == "supplied" {
					title, body := "fix: parser", "Author supplied description."
					sctx.Run.PRTitle, sctx.Run.PRBody = &title, &body
					content, err = (&PRStep{}).buildSuppliedPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, "", scm.ProviderGitHub, 0)
				} else {
					content, err = (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
				}
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(content.Body, leakedText) || strings.Contains(content.Body, sctx.UserIntent) {
					t.Errorf("private criterion escaped through recorded validation:\n%s", content.Body)
				}
				finalCount := "1"
				if path == "carried-keep" {
					finalCount = "unavailable"
				}
				if !strings.Contains(content.Body, "Recorded finding counts by snapshot: Review: final "+finalCount+", rounds [1: 2, 2: 1]") {
					t.Errorf("private publication lost recorded finding counts:\n%s", content.Body)
				}
				attestation := parsePipelineAttestationForTest(t, content.Body)
				if attestation.HeadSHA != sctx.Run.HeadSHA || attestation.LiveValidation == nil || attestation.LiveValidation.Live != 1 || attestation.LiveValidation.Total != 1 {
					t.Errorf("private publication lost factual attestation: %+v", attestation)
				}
				stored, err := sctx.DB.GetRoundsByStep(step.ID)
				if err != nil || len(stored) != 2 || stored[0].FindingsJSON == nil || *stored[0].FindingsJSON != testing {
					t.Fatalf("publication changed local validation evidence: %+v, %v", stored, err)
				}
				if !strings.Contains(userIntentPromptSection(sctx), sctx.UserIntent) {
					t.Fatal("validation lost private intent")
				}
			})
		}
	}
}

func TestPRPrivateValidationCountsSnapshots(t *testing.T) {
	t.Parallel()
	const privateText = "Synthetic private finding text"
	one := findingsJSON(t, types.Findings{Items: []types.Finding{{ID: "r1", Description: privateText}}})
	two := findingsJSON(t, types.Findings{Items: []types.Finding{{ID: "r1", Description: privateText}, {ID: "r2", Description: privateText}}})
	clean := findingsJSON(t, types.Findings{})
	malformed := "invalid " + privateText
	for _, tc := range []struct {
		name   string
		final  *string
		rounds []*db.StepRound
		want   string
	}{
		{name: "final only", final: &one, want: "Review: final 1"},
		{name: "repaired", final: &clean, rounds: []*db.StepRound{{Round: 1, FindingsJSON: &two}, {Round: 2, FindingsJSON: &one}, {Round: 3, FindingsJSON: &clean}}, want: "Review: final 0, rounds [1: 2, 2: 1, 3: 0]"},
		{name: "repeated snapshots", final: &one, rounds: []*db.StepRound{{Round: 1, FindingsJSON: &one}, {Round: 2, FindingsJSON: &one}}, want: "Review: final 1, rounds [1: 1, 2: 1]"},
		{name: "rounds only", rounds: []*db.StepRound{{Round: 1, FindingsJSON: &two}, {Round: 2, FindingsJSON: &one}}, want: "Review: final unavailable, rounds [1: 2, 2: 1]"},
		{name: "missing", rounds: []*db.StepRound{{Round: 1}}, want: "Review: final unavailable, rounds [1: unavailable]"},
		{name: "unreadable", final: &malformed, rounds: []*db.StepRound{{Round: 1, FindingsJSON: &malformed}}, want: "Review: final unavailable, rounds [1: unavailable]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := &db.StepResult{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: tc.final}
			steps := []*db.StepResult{step}
			rounds := map[string][]*db.StepRound{step.ID: tc.rounds}
			publicSummary, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub)

			privateSummary, notice := buildPipelineSummaryWithVisibility(steps, rounds, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{}, false)

			if !strings.Contains(notice, "Recorded finding counts by snapshot: "+tc.want+".") {
				t.Fatalf("recorded snapshot counts = %q, want %q", notice, tc.want)
			}
			if strings.Contains(privateSummary+notice, privateText) {
				t.Fatal("snapshot counts published private finding text")
			}
			if extractPipelineAttestationMarker(privateSummary) != extractPipelineAttestationMarker(publicSummary) {
				t.Fatal("snapshot counts changed the machine-readable attestation")
			}
			if !strings.Contains(privateSummary, "**Review**: completed") {
				t.Fatal("snapshot counts lost the lifecycle status")
			}
		})
	}
}

func TestPRValidationTextPublicationUsesTrustedPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, trusted             string
		shouldOmit, shouldPublish bool
	}{
		{name: "default", trusted: "pr: {}"},
		{name: "explicit refusal", trusted: "pr: {publish_intent: false}"},
		{name: "trusted opt-in", trusted: "pr: {publish_intent: true}", shouldPublish: true},
		{name: "caller omission", trusted: "pr: {publish_intent: true}", shouldOmit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			trusted, err := config.LoadRepoFromBytes([]byte(tc.trusted))
			if err != nil {
				t.Fatal(err)
			}
			pushed, err := config.LoadRepoFromBytes([]byte("pr: {publish_intent: true}"))
			if err != nil {
				t.Fatal(err)
			}
			sctx.Config = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(pushed, trusted, true))
			sctx.Run.OmitIntent = tc.shouldOmit
			const recordedText = "Synthetic criterion paraphrase: preserve C:/synthetic/operator/private-run."
			insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, types.Findings{
				RiskLevel: "high", RiskRationale: recordedText,
				Items: []types.Finding{{ID: "r1", Description: recordedText}},
			}), "")
			insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{TestingSummary: recordedText}), "")

			pipelineMD, risk, testing, evidence := (&PRStep{}).buildPipelineSection(sctx, scm.ProviderGitHub)

			if got := strings.Contains(pipelineMD+risk+testing+evidence, recordedText); got != tc.shouldPublish {
				t.Fatalf("recorded validation text published = %v, want %v", got, tc.shouldPublish)
			}
			if !tc.shouldPublish && !strings.Contains(risk, "Review: final 1, rounds [1: 1]") {
				t.Fatalf("private publication lost recorded counts: %s", risk)
			}
			if attestation := parsePipelineAttestationForTest(t, pipelineMD); attestation.HeadSHA != head || len(attestation.Steps) != 2 {
				t.Fatalf("publication changed attestation: %+v", attestation)
			}
		})
	}
}

func TestPRPrivateValidationDoesNotUploadArtifactsOrPublishConversation(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	sctx.Config.PR.Template = ""
	sctx.Config.PR.PublishIntent = nil
	withReviewConversation(sctx)
	const privateText = "Synthetic operator decision about the private launch window."
	if err := sctx.DB.RecordReviewAnswer(db.ReviewAnswer{
		RepoID: sctx.Repo.ID, Branch: sctx.Run.Branch, RunID: sctx.Run.ID, QuestionID: "q1", AskOrdinal: 1,
		Question: privateText, Answer: privateText,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sctx.EvidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(sctx.EvidenceDir, "synthetic.png")
	if err := os.WriteFile(artifact, []byte(privateText), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Config.Test.Evidence.AttachMedia = true
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
		Artifacts: []types.TestArtifact{{Kind: "screenshot", Label: privateText, Path: artifact}},
	}), "")
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"synthetic.png": testAttachmentURL}}

	content, err := (&PRStep{mediaUploader: uploader}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)

	if err != nil {
		t.Fatal(err)
	}
	if len(uploader.calls) != 0 || strings.Contains(content.Body, privateText) || strings.Contains(content.Body, "Review conversation") {
		t.Fatalf("private validation published an artifact or conversation: uploads=%d\n%s", len(uploader.calls), content.Body)
	}
}

func TestPROmitIntentSuppressionIsTightenOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		runOmitIntent  bool
		repoPolicy     string
		wantIntSection bool
	}{
		// Without the stamp the repository's trusted policy alone decides.
		{name: "default keeps private", runOmitIntent: false, repoPolicy: "pr: {}", wantIntSection: false},
		{name: "trusted opt-in publishes", runOmitIntent: false, repoPolicy: "pr: {publish_intent: true}", wantIntSection: true},
		// The run stamp removes the section even where the repo publishes.
		{name: "run stamp removes", runOmitIntent: true, repoPolicy: "pr: {}", wantIntSection: false},
		{name: "run stamp removes with repo publish", runOmitIntent: true, repoPolicy: "pr: {publish_intent: true}", wantIntSection: false},
		// The repository's trusted ceiling is untouched by anything else.
		{name: "trusted ceiling wins", runOmitIntent: false, repoPolicy: "pr: {publish_intent: false}", wantIntSection: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Run.OmitIntent = tc.runOmitIntent
			trusted, err := config.LoadRepoFromBytes([]byte(tc.repoPolicy))
			if err != nil {
				t.Fatal(err)
			}
			sctx.Config.PR = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(nil, trusted, false)).PR
			sctx.UserIntent = "Entire original intent remains reviewer input."
			got, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got.Body, "## Intent") != tc.wantIntSection {
				t.Fatalf("runOmit=%v policy=%s: body has Intent section = %v, want %v:\n%s", tc.runOmitIntent, tc.repoPolicy, !tc.wantIntSection, tc.wantIntSection, got.Body)
			}
			if !strings.Contains(got.Body, "## What Changed") {
				t.Fatalf("body lost its main section:\n%s", got.Body)
			}
			if got := strings.Contains(ag.calls[len(ag.calls)-1].Prompt, sctx.UserIntent); got != tc.wantIntSection {
				t.Fatalf("drafting prompt contains intent = %v under runOmit=%v", got, tc.runOmitIntent)
			}
		})
	}
}

// Under the caller-side omission the PR-drafting turns receive no intent text
// at all, on both the ordinary narrative path and the repository-template
// path: withhold, never scan. Every other step prompt keeps the full intent.
func TestPROmitIntentWithholdsIntentFromDraftingTurns(t *testing.T) {
	t.Parallel()
	const secret = "Private goal: replace the vendor before the contract renews."
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*pipeline.StepContext, *mockAgent)
	}{
		{name: "ordinary narrative", setup: func(t *testing.T) (*pipeline.StepContext, *mockAgent) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
			}}
			return newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{}), ag
		}},
		{name: "repository template narrative", setup: func(t *testing.T) (*pipeline.StepContext, *mockAgent) {
			sctx, ag, _ := templateTestContext(t)
			return sctx, ag
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, ag := tc.setup(t)
			sctx.UserIntent = secret
			yes := true
			sctx.Config.PR.PublishIntent = &yes
			sctx.Run.OmitIntent = true
			got, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) == 0 {
				t.Fatal("no drafting turn ran")
			}
			for i, call := range ag.calls {
				if strings.Contains(call.Prompt, secret) || strings.Contains(call.Prompt, "USER INTENT") {
					t.Fatalf("drafting turn %d received intent text:\n%s", i, call.Prompt)
				}
			}
			if strings.Contains(got.Body, secret) || strings.Contains(got.Body, "## Intent") {
				t.Fatalf("PR body published withheld intent:\n%s", got.Body)
			}
			// Other step prompts are unchanged: the full intent still reaches them.
			if !strings.Contains(userIntentPromptSection(sctx), secret) {
				t.Fatal("omission leaked into the shared step prompt section")
			}
		})
	}
}

func TestPRPublishIntentSuppressionCoversDefaultAgentAndFallback(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	for _, tc := range []struct {
		name    string
		publish *bool
		want    bool
	}{
		{"default", nil, false},
		{"enabled", &yes, true},
		{"disabled", &no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, fallback := range []bool{false, true} {
				dir, base, head := setupGitRepo(t)
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if fallback {
						return nil, errors.New("draft unavailable")
					}
					return &agent.Result{Output: json.RawMessage(`{"title":"feat: helper","body":"## What Changed\n\n- A helper."}`)}, nil
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				policy := "pr: {}"
				if tc.publish != nil {
					policy = fmt.Sprintf("pr: {publish_intent: %t}", *tc.publish)
				}
				trusted, err := config.LoadRepoFromBytes([]byte(policy))
				if err != nil {
					t.Fatal(err)
				}
				pushed, err := config.LoadRepoFromBytes([]byte(fmt.Sprintf("pr: {publish_intent: %t}", !tc.want)))
				if err != nil {
					t.Fatal(err)
				}
				sctx.Config.PR = config.Merge(config.DefaultGlobalConfig(), config.EffectiveRepoConfig(pushed, trusted, true)).PR
				sctx.UserIntent = "Entire original intent remains reviewer input."
				for _, limit := range []int{0, 4000} {
					got, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, scm.ProviderGitHub, limit)
					if err != nil || strings.Contains(got.Body, "## Intent") != tc.want || !strings.Contains(got.Body, "## What Changed") {
						t.Fatalf("fallback=%v limit=%d: %+v, %v", fallback, limit, got, err)
					}
					t.Logf("Generated PR markdown: policy=%s fallback=%v limit=%d\n%s", tc.name, fallback, limit, got.Body)
					if tc.want && !strings.Contains(got.Body, "## Intent\n\n"+sctx.UserIntent) {
						t.Fatalf("explicit publication lost intent: %s", got.Body)
					}
					if got := strings.Contains(ag.calls[len(ag.calls)-1].Prompt, sctx.UserIntent); got != tc.want {
						t.Fatalf("drafting prompt contains intent = %v, want %v", got, tc.want)
					}
				}
			}
		})
	}
}

// A supplied description is author narrative, not intent: the Intent section
// still lands in the appendix under the same publication controls.
func TestPRSuppliedBodyPublishesIntentUnderTheSameControls(t *testing.T) {
	t.Parallel()
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprintf("omit=%v", omit), func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			insertCompletedStep(t, sctx, types.StepReview, "", "")
			title, body := "Supplied title", "Supplied narrative."
			sctx.Run.PRTitle, sctx.Run.PRBody = &title, &body
			sctx.Run.OmitIntent = omit
			sctx.UserIntent = "Ship the supplied description."
			isPublished := true
			sctx.Config.PR.PublishIntent = &isPublished
			got, err := (&PRStep{}).buildSuppliedPRContent(sctx, "feature", "main", base, "", scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			parts, err := parsePROwnedBody(got.Body)
			if err != nil || parts.before != body+"\n\n" {
				t.Fatalf("narrative = %q, %v", parts.before, err)
			}
			if strings.Contains(parts.appendix, "## Intent\n\nShip the supplied description.") == omit {
				t.Fatalf("omit=%v: appendix Intent section wrong:\n%s", omit, parts.appendix)
			}
		})
	}
}
