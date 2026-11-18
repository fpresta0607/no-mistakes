package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestParseSkipPushOptions(t *testing.T) {
	got, err := parseSkipPushOptions([]string{
		"ci.skip",
		"no-mistakes.skip=test,lint",
	})
	if err != nil {
		t.Fatalf("parseSkipPushOptions() error = %v", err)
	}
	want := []types.StepName{types.StepTest, types.StepLint}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestParseSkipPushOptionsRejectsUnknownStep(t *testing.T) {
	_, err := parseSkipPushOptions([]string{"no-mistakes.skip=test,deploy"})
	if err == nil {
		t.Fatal("expected unknown step to fail")
	}
}

func TestNormalizeNotifyGatePathResolvesLegacyDotGate(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "repo123.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(bare); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(oldDir); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	t.Setenv("PWD", ".")

	got, err := normalizeNotifyGatePath(".")
	if err != nil {
		t.Fatalf("normalizeNotifyGatePath: %v", err)
	}
	if got == "." || !filepath.IsAbs(got) {
		t.Fatalf("normalizeNotifyGatePath(.) = %q, want absolute path", got)
	}
	want, err := realpath.Resolve(bare)
	if err != nil {
		want = bare
	}
	gotResolved, err := realpath.Resolve(got)
	if err != nil {
		gotResolved = got
	}
	if gotResolved != want {
		t.Fatalf("normalizeNotifyGatePath(.) = %q (resolved %q), want %q", got, gotResolved, want)
	}
}

func TestFormatSkipPushOptions(t *testing.T) {
	got := formatSkipPushOptions([]types.StepName{types.StepTest, types.StepLint})
	want := []string{"no-mistakes.skip=test,lint"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestIntentPushOptionRoundTrip(t *testing.T) {
	// Multi-line, comma- and colon-bearing intent must survive the
	// line-oriented push-option transport intact.
	intent := "add retry to the uploader\n\nwhy: flaky network, commas, colons: ok"
	opt := formatIntentPushOption(intent)
	if opt == "" {
		t.Fatal("formatIntentPushOption returned empty for a non-empty intent")
	}
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", opt})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != intent {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, intent)
	}
}

func TestFormatIntentPushOptionEmpty(t *testing.T) {
	if got := formatIntentPushOption("   "); got != "" {
		t.Fatalf("formatIntentPushOption(blank) = %q, want empty", got)
	}
}

func TestParseIntentPushOptionsNone(t *testing.T) {
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", "ci.skip"})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != "" {
		t.Fatalf("parseIntentPushOptions(no intent) = %q, want empty", got)
	}
}

func TestProofAndPRBaseBranchPushOptionsRoundTrip(t *testing.T) {
	nonce := "request-7f3"
	generation := "generation-7"
	nonceOpt := formatLaunchNoncePushOption(nonce)
	generationOpt := formatValidationGenerationPushOption(generation)
	baseBranchOpt := formatPRBaseBranchPushOption("epic/feature")
	gotNonce, err := parseLaunchNoncePushOptions([]string{nonceOpt, baseBranchOpt})
	if err != nil {
		t.Fatal(err)
	}
	gotGeneration, err := parseValidationGenerationPushOptions([]string{generationOpt, baseBranchOpt})
	if err != nil {
		t.Fatal(err)
	}
	gotBaseBranch, err := parsePRBaseBranchPushOptions([]string{nonceOpt, generationOpt, baseBranchOpt})
	if err != nil {
		t.Fatal(err)
	}
	if gotNonce != nonce || gotGeneration != generation || gotBaseBranch != "epic/feature" {
		t.Fatalf("push options = nonce %q generation %q base branch %q", gotNonce, gotGeneration, gotBaseBranch)
	}
	if _, err := parseValidationGenerationPushOptions([]string{generationOpt, formatValidationGenerationPushOption("generation-8")}); err == nil {
		t.Fatal("conflicting validation generations were accepted")
	}
}

func TestReconciledPreviousHeadPushOptionRoundTrip(t *testing.T) {
	head := "1234567890abcdef1234567890abcdef12345678"
	opt := formatReconciledPreviousHeadPushOption(head)
	if opt == "" {
		t.Fatal("reconciled previous head produced no push option")
	}
	got, err := parseReconciledPreviousHeadPushOptions([]string{"no-mistakes.skip=review", opt})
	if err != nil {
		t.Fatal(err)
	}
	if got != head {
		t.Fatalf("parsed previous head = %q, want %q", got, head)
	}
	if formatReconciledPreviousHeadPushOption("   ") != "" {
		t.Fatal("an empty previous head produced a push option")
	}
	if got, err := parseReconciledPreviousHeadPushOptions(nil); err != nil || got != "" {
		t.Fatalf("absent option = %q, %v", got, err)
	}
	if _, err := parseReconciledPreviousHeadPushOptions([]string{"no-mistakes.reconciled-previous-head=refs/heads/main"}); err == nil {
		t.Fatal("a non-SHA previous head claim was accepted")
	}
}

func TestClosingIssueRefsPushOptionsRoundTrip(t *testing.T) {
	options := formatClosingIssueRefsPushOptions([]string{"42", "owner/repo#9"})
	got, err := parseClosingIssueRefsPushOptions(append([]string{"ci.skip"}, options...))
	if err != nil {
		t.Fatalf("parseClosingIssueRefsPushOptions() error = %v", err)
	}
	if refs := strings.Join(got, ","); refs != "42,owner/repo#9" {
		t.Fatalf("parseClosingIssueRefsPushOptions() = %q, want both references", refs)
	}
}

func TestParseClosingIssueRefsPushOptionsRejectsInvalidValue(t *testing.T) {
	_, err := parseClosingIssueRefsPushOptions([]string{"no-mistakes.closes=42 Fixes #99"})
	if err == nil {
		t.Fatal("expected invalid closing issue references push option to fail")
	}
}

func TestTestOutputDirsPushOptionsRoundTrip(t *testing.T) {
	dirs := []string{`C:\operator\logs`, "/operator/provenance dir"}
	options := formatTestOutputDirsPushOptions(dirs)

	got, err := parseTestOutputDirsPushOptions(append([]string{"ci.skip", "no-mistakes.closes=42"}, options...))

	if err != nil {
		t.Fatalf("parseTestOutputDirsPushOptions() error = %v", err)
	}
	if !reflect.DeepEqual(got, dirs) {
		t.Fatalf("parseTestOutputDirsPushOptions() = %q, want %q", got, dirs)
	}
}

func TestParseTestOutputDirsPushOptionsRejectsAnEmptyValue(t *testing.T) {
	if _, err := parseTestOutputDirsPushOptions([]string{"no-mistakes.test-output-dir= "}); err == nil {
		t.Fatal("an empty test output directory push option was accepted")
	}
}

func TestAgentEnvPushOptionsRoundTrip(t *testing.T) {
	agentEnv := map[string]string{
		"UV_EXCLUDE_NEWER":      "7 days",
		"npm_config_before":     "2026-11-01T12:30:00Z",
		"PIP_UPLOADED_PRIOR_TO": "2026-11-01",
	}
	options := formatAgentEnvPushOptions(agentEnv)

	wantOptions := []string{
		"no-mistakes.agent-env=PIP_UPLOADED_PRIOR_TO=2026-11-01",
		"no-mistakes.agent-env=UV_EXCLUDE_NEWER=7 days",
		"no-mistakes.agent-env=npm_config_before=2026-11-01T12:30:00Z",
	}
	if !reflect.DeepEqual(options, wantOptions) {
		t.Fatalf("formatAgentEnvPushOptions() = %q, want %q", options, wantOptions)
	}
	got, err := parseAgentEnvPushOptions(append([]string{"ci.skip", "no-mistakes.closes=42"}, options...))
	if err != nil {
		t.Fatalf("parseAgentEnvPushOptions() error = %v", err)
	}
	if !reflect.DeepEqual(got, agentEnv) {
		t.Fatalf("parseAgentEnvPushOptions() = %q, want %q", got, agentEnv)
	}
}

func TestParseAgentEnvPushOptionsWithoutThemIsNil(t *testing.T) {
	got, err := parseAgentEnvPushOptions([]string{"ci.skip", "no-mistakes.test-output-dir=/operator/logs"})
	if err != nil || got != nil {
		t.Fatalf("parseAgentEnvPushOptions() = %q, %v; want nil and no error", got, err)
	}
}

func TestParseAgentEnvPushOptionsRefusesWhatTheCLIWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []string
		want    string
	}{
		{"name_outside_the_list", []string{"no-mistakes.agent-env=PATH=/evil"}, `"PATH"`},
		{"no_equals", []string{"no-mistakes.agent-env=UV_EXCLUDE_NEWER"}, "NAME=VALUE"},
		{"empty_value", []string{"no-mistakes.agent-env=UV_EXCLUDE_NEWER="}, "nonempty"},
		{"duplicate_name", []string{"no-mistakes.agent-env=UV_EXCLUDE_NEWER=a", "no-mistakes.agent-env=UV_EXCLUDE_NEWER=b"}, "more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAgentEnvPushOptions(tc.options)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseAgentEnvPushOptions() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
