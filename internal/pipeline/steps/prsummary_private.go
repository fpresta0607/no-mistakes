package steps

import (
	"fmt"
	"regexp"

	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// privateRuntimeDetailPatterns name what a public pull request must never carry
// from the Test step's agent-written evidence (issue #125): an absolute or
// home-relative local path in any spelling, a home or temp directory variable,
// a run identifier (a 26-character ULID), and a process identifier. Path
// redaction only rewrites known prefixes, so a home-relative cache path, an
// interpreter location or a run ID inside serialized JSON survived it. A
// repository-relative path matches none of these and stays public. The rules
// fail closed: a false positive withholds one entry the notice still counts.
var privateRuntimeDetailPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:^|[^A-Za-z0-9])[A-Za-z]:[\\/]`),
	regexp.MustCompile(privatePathBoundary + `[A-Za-z]:[\\/]`),
	regexp.MustCompile(privatePathBoundary + `(?i:file://)?/[A-Za-z0-9._~-]+/`),
	regexp.MustCompile(privatePathBoundary + `\\\\(?:\\\\)?[A-Za-z0-9]`),
	regexp.MustCompile(privatePathBoundary + `~[\\/]`),
	regexp.MustCompile(`(?i)\$\{?(?:HOME|TMPDIR|USERPROFILE)\b|%(?:USERPROFILE|HOMEPATH|LOCALAPPDATA|APPDATA|TEMP|TMP)%`),
	regexp.MustCompile(`\b[0-9A-Z]{26}\b`),
	regexp.MustCompile(`(?i)\b(?:pid|process[ _-]?id)s?\b[^\w\n]{0,8}\d+`),
}

// privatePathBoundary is what may precede a local path: anything that does not
// continue a relative path or URL (including a "]", ")", "}", "*" or "@" that
// ends a route segment, glob or alias), a JSON-escaped control character, or
// an attached short flag ("-I/opt/include/").
const privatePathBoundary = `(?:^|\\[nrt]|[^A-Za-z0-9_./\\\-\])}*@])(?:-[A-Za-z])?`

// httpRequestTarget is an HTTP request line's method and target, as a dev
// server logs it ("POST /api/jobs 200"): a URL path, not a filesystem one, so
// it is set aside before the path patterns run. The home-root check still sees
// the original text.
var httpRequestTarget = regexp.MustCompile(`\b(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+/\S*`)

// withheldScenarioText stands in for a scenario field that carried a private
// runtime detail; the scenario keeps its result and liveness.
const withheldScenarioText = "withheld (private runtime detail)"

func carriesPrivateRuntimeDetail(text string) bool {
	if text == "" {
		return false
	}
	if safepath.RedactText(text) != text {
		return true
	}
	text = httpRequestTarget.ReplaceAllString(text, "")
	for _, pattern := range privateRuntimeDetailPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// withholdPrivateRuntimeDetails returns findings with every Test evidence entry
// that carries a private runtime detail removed: the testing summary, a tested
// entry, an artifact whose label, content or URL carries one, and a scenario
// field (replaced, so the scenario still counts). Each withheld entry is added
// to withheld, keyed by its text, so an entry repeated across rounds counts once.
func withholdPrivateRuntimeDetails(findings types.Findings, withheld map[string]bool) types.Findings {
	withhold := func(kind, text string) bool {
		if !carriesPrivateRuntimeDetail(text) {
			return false
		}
		withheld[kind+"\x00"+text] = true
		return true
	}
	if withhold("summary", findings.TestingSummary) {
		findings.TestingSummary = ""
	}
	var tested []string
	for _, detail := range findings.Tested {
		if !withhold("tested", detail) {
			tested = append(tested, detail)
		}
	}
	findings.Tested = tested
	var artifacts []types.TestArtifact
	for _, artifact := range findings.Artifacts {
		key := artifact.Label + "\x00" + artifact.Content + "\x00" + artifact.URL
		if !withhold("artifact", key) {
			artifacts = append(artifacts, artifact)
		}
	}
	findings.Artifacts = artifacts
	scenarios := make([]types.TestScenario, len(findings.Scenarios))
	for i, scenario := range findings.Scenarios {
		if withhold("scenario-name", scenario.Name) {
			scenario.Name = withheldScenarioText
		}
		if withhold("scenario-reason", scenario.Reason) {
			scenario.Reason = withheldScenarioText
		}
		if withhold("scenario-evidence", scenario.Evidence) {
			scenario.Evidence = withheldScenarioText
		}
		scenarios[i] = scenario
	}
	if findings.Scenarios != nil {
		findings.Scenarios = scenarios
	}
	return findings
}

// withheldEntriesNotice tells a reader how many entries were held back, so
// nothing is dropped silently, and that the raw evidence still exists.
func withheldEntriesNotice(count int) string {
	switch count {
	case 0:
		return ""
	case 1:
		return "1 Test evidence entry withheld from this pull request: it carries a local path or a run or process identifier. The raw evidence is kept privately with the run."
	}
	return fmt.Sprintf("%d Test evidence entries withheld from this pull request: they carry local paths or run or process identifiers. The raw evidence is kept privately with the run.", count)
}
