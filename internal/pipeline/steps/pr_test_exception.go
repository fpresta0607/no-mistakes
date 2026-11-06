package steps

import (
	"html"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

const (
	testExceptionStart = "<!-- no-mistakes-test-exception:v1 -->"
	testExceptionEnd   = "<!-- /no-mistakes-test-exception:v1 -->"
)

func renderTestExceptionSection(steps []*db.StepResult) string {
	for _, step := range steps {
		if exception := step.TestOverrideReason(); exception != "" {
			quoted := strings.ReplaceAll(html.EscapeString(neutralizePRControls(exception)), "\n", "\n> ")
			return testExceptionStart + "\n### Test approval exception\n\n> " + quoted + "\n" + testExceptionEnd
		}
	}
	return ""
}
