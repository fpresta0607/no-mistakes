package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
					if !strings.Contains(content.Body, "Request parser regression passed.") || !strings.Contains(content.Body, "Closes #50") {
						t.Error("public body lost test evidence or the requested issue reference")
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
