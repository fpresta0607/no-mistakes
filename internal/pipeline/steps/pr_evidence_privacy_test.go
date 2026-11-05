package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const privateEvidenceRunID = "01RUNIDFIXTURE0000000000AB"

// TestPRStep_ExecuteKeepsCapturedRuntimeContextOutOfThePublishedBody reproduces
// issue #57 on the bytes handed to the provider CLI: a Test artifact carrying a
// captured agent prompt and local execution context used to be inlined as a text
// evidence block, and home redaction left its per-run path standing.
func TestPRStep_ExecuteKeepsCapturedRuntimeContextOutOfThePublishedBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		isCompact bool
	}{
		{name: "compact", isCompact: true},
		{name: "expanded", isCompact: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			env, logFile := fakeGH(t, "")
			evidenceDir := filepath.Join(t.TempDir(), "evidence", privateEvidenceRunID)
			scratchPath := filepath.Join(evidenceDir, "test-scratch", "gate-4711")
			capturedPrompt := agent.WorktreeSteering(evidenceDir) + agent.ExecutionContextPromptSection(scratchPath)
			markdownPath := writeTempEvidenceFile(t, evidenceDir, "prompts.md", []byte(capturedPrompt))
			htmlPath := writeTempEvidenceFile(t, evidenceDir, "report.html", []byte("<html><pre>"+capturedPrompt+"</pre></html>"))
			binaryPath := writeTempEvidenceFile(t, evidenceDir, "capture.dat", []byte{0x00, 0x01, 0xff})

			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					payload, err := json.Marshal(prContent{
						Title: "fix(api): keep evidence private",
						Body:  "## What Changed\n\n- evidence now lands under " + markdownPath,
					})
					if err != nil {
						return nil, err
					}
					return &agent.Result{Output: json.RawMessage(payload)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.EvidenceDir = evidenceDir
			sctx.Config.Test.Evidence.Compact = tc.isCompact

			insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
				TestingSummary: "Rendered the PR body from a stub agent.",
				Verdict:        types.TestVerdictInconclusive,
				Scenarios: []types.TestScenario{{
					Name:   "publish the rendered body",
					Result: types.ScenarioResultUntested,
					Reason: "no live forge in this environment",
				}},
				Artifacts: []types.TestArtifact{
					{Kind: "log", Label: "Rendered markdown", Path: markdownPath},
					{Kind: "other", Label: "Rendered report", Path: htmlPath},
					{Kind: "other", Label: "Binary capture", Path: binaryPath},
					{Kind: "command-output", Label: "Pasted prompt", Content: capturedPrompt},
					{Kind: "command-output", Label: "Curated excerpt", Content: "publish ok in " + scratchPath},
				},
			}), "")

			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			published := string(data)
			for _, leak := range []string{
				"Workspace boundary",
				"Execution context",
				"isolated git worktree",
				privateEvidenceRunID,
				"local file:",
				filepath.ToSlash(evidenceDir),
			} {
				if strings.Contains(filepath.ToSlash(published), leak) {
					t.Errorf("published PR content leaked %q:\n%s", leak, published)
				}
			}
			for _, want := range []string{
				"Live validation: ",
				types.TestVerdictInconclusive,
				"0 of 1 scenarios driven live",
				pipelineAttestationCommentPrefix,
				headSHA,
				"Rendered markdown",
				fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(capturedPrompt))),
				"Binary capture",
				withheldArtifactContent,
				"publish ok in " + safepath.EvidencePlaceholder,
				"evidence now lands under " + safepath.EvidencePlaceholder,
			} {
				if !strings.Contains(filepath.ToSlash(published), want) {
					t.Errorf("expected %q in published PR content:\n%s", want, published)
				}
			}
		})
	}
}

// An evidence file keeps a public trace in every presentation: its label and the
// digest of the exact bytes, spelled for the provider's markup.
func TestBuildPRTestingSummary_ReferencesPrivateEvidenceWithoutItsBytes(t *testing.T) {
	t.Parallel()
	fileBody := "RENDERED WIZARD SCREEN"
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(fileBody)))
	for _, tc := range []struct {
		name      string
		provider  scm.Provider
		isCompact bool
		want      string
	}{
		{name: "github compact", provider: scm.ProviderGitHub, isCompact: true, want: "- Evidence: Wizard log (private run evidence, <code>" + digest + "</code>)"},
		{name: "github expanded", provider: scm.ProviderGitHub, want: "- Evidence: Wizard log (private run evidence, <code>" + digest + "</code>)"},
		{name: "bitbucket compact", provider: scm.ProviderBitbucket, isCompact: true, want: "- Evidence: Wizard log (private run evidence, `" + digest + "`)"},
		{name: "bitbucket expanded", provider: scm.ProviderBitbucket, want: "- Evidence: Wizard log (private run evidence, `" + digest + "`)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evidenceRoot := filepath.Join(t.TempDir(), "evidence", privateEvidenceRunID)
			localPath := writeTempEvidenceFile(t, evidenceRoot, "wizard.txt", []byte(fileBody))
			steps, rounds := testStepWithArtifacts(fmt.Sprintf(`{"kind":"log","label":"Wizard log","path":%q}`, localPath))

			md := buildPRTestingSummary(steps, rounds, "", "abc123", t.TempDir(), evidenceRoot, nil, tc.provider, nil, tc.isCompact)

			if !strings.Contains(md, tc.want) {
				t.Fatalf("expected %q, got:\n%s", tc.want, md)
			}
			for _, private := range []string{fileBody, localPath, privateEvidenceRunID} {
				if strings.Contains(md, private) {
					t.Fatalf("did not expect %q, got:\n%s", private, md)
				}
			}
		})
	}
}
