package steps

import (
	"html"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	testExceptionStart = "<!-- no-mistakes-test-exception:v1 -->"
	testExceptionEnd   = "<!-- /no-mistakes-test-exception:v1 -->"
)

func renderTestExceptionSection(steps []*db.StepResult, provider scm.Provider, workDir, runEvidence string) string {
	for _, step := range steps {
		if exception := step.TestOverrideReason(); exception != "" {
			exception = redactPRContent(prContent{Body: exception}, workDir, nmWorktreesRoot(), runEvidence).Body
			quoted := strings.ReplaceAll(closingGrammarFor(provider).neutralize(html.EscapeString(exception)), "\n", "\n> ")
			return testExceptionStart + "\n### Test approval exception\n\n> " + quoted + "\n" + testExceptionEnd
		}
	}
	return ""
}

func extractTestExceptionSection(body string) string {
	start := strings.Index(body, testExceptionStart)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], testExceptionEnd)
	if end < 0 {
		return ""
	}
	return body[start : start+end+len(testExceptionEnd)]
}
