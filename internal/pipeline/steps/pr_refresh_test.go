package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type refreshPRHost struct {
	scm.Host
	pr                    scm.PR
	body                  string
	state                 scm.PRState
	observedHead          string
	readError, writeError error
	isBodyChanged         bool
	isReadbackChanged     bool
	moveHeadAt, checks    int
	reads, writes         int
	writtenTitle          string
}

func (host *refreshPRHost) Capabilities() scm.Capabilities { return scm.Capabilities{} }
func (host *refreshPRHost) Provider() scm.Provider         { return scm.ProviderGitHub }

func (host *refreshPRHost) FindPR(context.Context, string, string) (*scm.PR, error) {
	copy := host.pr
	return &copy, nil
}

func (host *refreshPRHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return host.state, nil
}

func (host *refreshPRHost) GetChecks(_ context.Context, pr *scm.PR) ([]scm.Check, error) {
	host.checks++
	if pr.HeadSHA != testPipelineHeadSHA || pr.ObservedHeadSHA != "" {
		return nil, errors.New("head observation requires the expected head and a cleared observation")
	}
	pr.ObservedHeadSHA = host.observedHead
	if host.moveHeadAt != 0 && host.checks >= host.moveHeadAt {
		pr.ObservedHeadSHA = strings.Repeat("d", 40)
	}
	return nil, nil
}

func (host *refreshPRHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	host.reads++
	if host.isBodyChanged && host.reads == 2 {
		host.body += "\nAuthor added a note."
	}
	if host.isReadbackChanged && host.reads == 3 {
		host.body += "\nAuthor added a note after the write."
	}
	return scm.PRContent{Title: "author title", Body: host.body}, host.readError
}

func (host *refreshPRHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	host.writes++
	host.writtenTitle = content.Title
	if host.writeError != nil {
		return nil, host.writeError
	}
	host.body = content.Body
	return pr, nil
}

func TestRefreshPublishedPR(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		change    func(*refreshPRHost)
		isRefused bool
	}{
		{"exact", func(*refreshPRHost) {}, false},
		{"owned", func(host *refreshPRHost) {
			host.body = "Author prefix\n\n" + wrapPRAppendix(host.body) + "\nAuthor suffix"
		}, false},
		{"stale-discovered-head", func(host *refreshPRHost) {
			host.pr.HeadSHA = strings.Repeat("e", 40)
			host.pr.ObservedHeadSHA = strings.Repeat("f", 40)
		}, false},
		{"wrong-pr", func(host *refreshPRHost) { host.pr.URL = "https://github.com/owner/repo/pull/18" }, true},
		{"moved-head", func(host *refreshPRHost) { host.observedHead = strings.Repeat("b", 40) }, true},
		{"head-moved-before-write", func(host *refreshPRHost) { host.moveHeadAt = 2 }, true},
		{"head-moved-after-write", func(host *refreshPRHost) { host.moveHeadAt = 3 }, true},
		{"unobserved-head", func(host *refreshPRHost) { host.observedHead = "" }, true},
		{"closed", func(host *refreshPRHost) { host.state = scm.PRStateClosed }, true},
		{"missing-marker", func(host *refreshPRHost) { host.body = "author text" }, true},
		{"wrong-attested-head", func(host *refreshPRHost) {
			host.body = strings.ReplaceAll(host.body, testPipelineHeadSHA, strings.Repeat("c", 40))
		}, true},
		{"ambiguous-marker", func(host *refreshPRHost) { host.body += host.body }, true},
		{"foreign-command-override", func(host *refreshPRHost) {
			host.body = strings.ReplaceAll(host.body, `"status":"completed"`, `"status":"completed","override_reason":"foreign failure"`)
		}, true},
		{"edited-exception", func(host *refreshPRHost) {
			host.body += "\n" + testExceptionStart + "\nAuthor note\n" + testExceptionEnd
		}, true},
		{"concurrent-edit", func(host *refreshPRHost) { host.isBodyChanged = true }, true},
		{"unreadable", func(host *refreshPRHost) { host.readError = errors.New("unreadable") }, true},
		{"write-error", func(host *refreshPRHost) { host.writeError = errors.New("uncertain write") }, true},
		{"readback-edited", func(host *refreshPRHost) { host.isReadbackChanged = true }, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reason := "Accept retained checks; seven repair contracts remain untested"
			findings := `{"verdict":"inconclusive","findings":[]}`
			step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &reason}
			originalMarker := pipelineAttestationCommentPrefix + `{"head_sha":"` + testPipelineHeadSHA + `","steps":[{"step":"test","status":"completed"}],"future":{"keep":true}}` + pipelineAttestationCommentClosingToken
			original := "Author narrative and original failed evidence.\n\n" + originalMarker + "\n\nSeven untested scenarios and historical rounds."
			host := &refreshPRHost{pr: scm.PR{Number: "17", URL: "https://github.com/owner/repo/pull/17"}, body: original, state: scm.PRStateOpen, observedHead: testPipelineHeadSHA}
			sctx := &pipeline.StepContext{Ctx: context.Background(), Run: &db.Run{Branch: "feature", HeadSHA: testPipelineHeadSHA, PRURL: &host.pr.URL}}
			// The stored run identity is independent of the provider's mutable reply.
			url := host.pr.URL
			sctx.Run.PRURL = &url
			testCase.change(host)
			isChanged, err := refreshPublishedPRWithHost(sctx, host, []*db.StepResult{step})
			if testCase.isRefused {
				expectedWrites := 0
				if host.writeError != nil || host.isReadbackChanged || host.moveHeadAt == 3 {
					expectedWrites = 1
				}
				if err == nil || isChanged || host.writes != expectedWrites {
					t.Fatalf("unsafe refresh: changed=%v error=%v writes=%d", isChanged, err, host.writes)
				}
				return
			}
			if err != nil || !isChanged || host.writes != 1 || host.writtenTitle != "" {
				t.Fatalf("refresh: changed=%v error=%v writes=%d title=%q", isChanged, err, host.writes, host.writtenTitle)
			}
			for _, expected := range []string{"test_exception", "### Test approval exception", reason, "Seven untested scenarios and historical rounds.", "Author narrative and original failed evidence.", `"future":{"keep":true}`} {
				if !strings.Contains(host.body, expected) {
					t.Fatalf("refresh omitted %q: %s", expected, host.body)
				}
			}
			if strings.Contains(host.body, "override_reason") || strings.Contains(host.body, "live_validation") {
				t.Fatalf("refresh invented a command waiver or live validation: %s", host.body)
			}
			if testCase.name == "owned" {
				if _, err := parsePROwnedBody(host.body); err != nil || !strings.HasPrefix(host.body, "Author prefix\n\n") || !strings.HasSuffix(host.body, "\nAuthor suffix") {
					t.Fatalf("owned body or author bytes changed: error=%v", err)
				}
			}
			isChanged, err = refreshPublishedPRWithHost(sctx, host, []*db.StepResult{step})
			if err != nil || isChanged || host.writes != 1 {
				t.Fatalf("repeat refreshed again: changed=%v error=%v writes=%d", isChanged, err, host.writes)
			}
			if testCase.name == "exact" || testCase.name == "owned" {
				body := host.body
				var parts prOwnedBody
				if testCase.name == "owned" {
					parts, err = parsePROwnedBody(host.body)
					if err != nil {
						t.Fatal(err)
					}
					body = parts.appendix
				}
				start := strings.Index(body, testExceptionStart)
				end := strings.Index(body, testExceptionEnd) + len(testExceptionEnd)
				host.body = body[:start] + body[end:]
				if testCase.name == "owned" {
					host.body = parts.before + wrapPRAppendix(host.body) + parts.after
				}
				isChanged, err = refreshPublishedPRWithHost(sctx, host, []*db.StepResult{step})
				if err == nil || isChanged || host.writes != 1 {
					t.Fatalf("deleted human section was recreated: changed=%v error=%v writes=%d", isChanged, err, host.writes)
				}
			}
		})
	}
}

func TestRefreshPublishedPR_RedactsStoredException(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"D:/fixtures/private-worktree", "D:/private/R&D/run", `D:\private\R&D\run`, "D:/private/O'Brien/run", "D:/private/<task>/run"} {
		t.Run(root, func(t *testing.T) {
			reason := "Retain checks under " + root + "/evidence; credential absent at /home/private-person/work."
			findings := `{"verdict":"inconclusive","findings":[]}`
			step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, ApprovalReason: &reason}
			body := buildPipelineAttestation([]*db.StepResult{{StepName: types.StepTest, Status: types.StepStatusCompleted}}, nil, testPipelineHeadSHA)
			updated, err := refreshPRTestException(body, testPipelineHeadSHA, []*db.StepResult{step}, root)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private-worktree", "private-person", "D:/private", `D:\private`, `D:\\private`} {
				if strings.Contains(updated, private) {
					t.Fatalf("refresh published a private path: %s", updated)
				}
			}
			if _, err := refreshPRTestException(updated, testPipelineHeadSHA, []*db.StepResult{step}, root); err != nil {
				t.Fatalf("redacted refresh is not repeatable: %v", err)
			}
		})
	}
	t.Run("command-override", func(t *testing.T) {
		root := "D:/private/R&D/run"
		reason := "Accept retained command failure"
		override := "  test wrapper refused " + root + "/check and /home/private-person/work\x1b[31m\x01  "
		step := &db.StepResult{StepName: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: &override, ApprovalReason: &reason}
		body := redactPRContent(prContent{Body: buildPipelineAttestationWithPolicy([]*db.StepResult{step}, nil, testPipelineHeadSHA, pipelineAttestationPolicy{AllowTestCommandOverride: "existing opt-in"})}, root, "").Body
		attestation := parsePipelineAttestationForTest(t, body)
		attestation.Steps[0].TestException = ""
		payloadBytes, err := json.Marshal(attestation)
		if err != nil {
			t.Fatal(err)
		}
		body = pipelineAttestationCommentPrefix + string(payloadBytes) + pipelineAttestationCommentClosingToken
		updated, err := refreshPRTestException(body, testPipelineHeadSHA, []*db.StepResult{step}, root)
		if err != nil {
			t.Fatal(err)
		}
		published := parsePipelineAttestationForTest(t, updated)
		if published.Steps[0].OverrideReason != "test wrapper refused {run-worktree}/check and ~/work\ufffd" || published.AllowTestCommandOverride != "existing opt-in" {
			t.Fatalf("refresh changed command-only policy: %+v", published)
		}
	})
}
