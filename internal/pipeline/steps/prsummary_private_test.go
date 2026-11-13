package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCarriesPrivateRuntimeDetail(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		text      string
		isPrivate bool
	}{
		"repository-relative path":        {text: "internal/checkout/handler.go answered 200"},
		"dot-relative test selector":      {text: "`go test ./internal/checkout/...`"},
		"parent-relative path":            {text: "compared against ../fixtures/expected.json"},
		"https URL":                       {text: "see https://github.com/owner/repo/pull/7/checks"},
		"git object ID":                   {text: "tested at 0d6617b3519bb877448124f3e442ba08d7da1e3b"},
		"route with one segment":          {text: "GET /healthz returned 200"},
		"HTTP request line":               {text: " POST /api/jobs/inbound-mail 200"},
		"request line naming a home":      {text: "GET /home/someone/notes 404", isPrivate: true},
		"route in prose":                  {text: "the /api/jobs/ endpoint answered", isPrivate: true},
		"plain prose":                     {text: "the retry button is back and the receipt renders"},
		"empty":                           {text: ""},
		"posix absolute path":             {text: "ran /usr/local/bin/python3 -m pytest", isPrivate: true},
		"posix path inside JSON":          {text: `{"path":"/opt/toolchain/report.json"}`, isPrivate: true},
		"windows drive path":              {text: `interpreter C:\Toolchain\python.exe`, isPrivate: true},
		"windows path with forward slash": {text: "log at D:/build/out.log", isPrivate: true},
		"JSON-escaped windows path":       {text: `{"interpreter":"C:\\Toolchain\\python.exe"}`, isPrivate: true},
		"UNC share":                       {text: `copied to \\buildhost\share\out`, isPrivate: true},
		"home shorthand":                  {text: "cache at ~/.cache/checkout", isPrivate: true},
		"home variable":                   {text: "wrote $HOME/report.txt", isPrivate: true},
		"windows profile variable":        {text: `wrote %LOCALAPPDATA%\tool\log.txt`, isPrivate: true},
		"run identifier":                  {text: "run 01M7BBSFN5H455J7DH47NCGKR1 replayed", isPrivate: true},
		"pid field":                       {text: `{"pid":4711}`, isPrivate: true},
		"process ID in prose":             {text: "server started as process ID 4711", isPrivate: true},
		"conventional home root":          {text: "file:///Users/someone/report.html", isPrivate: true},
		"backtick-wrapped interpreter":    {text: "`/opt/toolchain/bin/python3 -m pytest`", isPrivate: true},
		"backtick-wrapped home shorthand": {text: "`~/.cache/checkout`", isPrivate: true},
		"stderr redirection":              {text: "go test ./... 2>/tmp/run/err.log", isPrivate: true},
		"stdout redirection":              {text: "echo ok >/var/tmp/x/out", isPrivate: true},
		"attached short flag":             {text: "cc -I/opt/include/ main.c", isPrivate: true},
		"path after JSON-escaped newline": {text: `{"log":"started\n/opt/toolchain/bin/python3"}`, isPrivate: true},
		"path after a brace":              {text: "{/opt/toolchain/bin/python3}", isPrivate: true},
		"path after a pipe":               {text: "cat x|/usr/local/bin/jq .", isPrivate: true},
		"file URL to a posix path":        {text: "opened file:///opt/report/index.html", isPrivate: true},
		"JSON-escaped UNC share":          {text: `{"out":"\\buildhost\share"}`, isPrivate: true},
		"drive path on a short flag":      {text: `cl.exe -IC:\SDK\include main.c`, isPrivate: true},
		"drive path after JSON escape":    {text: `{"log":"started\tC:\Toolchain\x"}`, isPrivate: true},
		"drive path in a file URL":        {text: "opened file:///D:/build/report.html", isPrivate: true},
		"column-aligned pid":              {text: "server up (pid:    4821)", isPrivate: true},
		"pretty-printed pid field":        {text: `{"pid" : 4821}`, isPrivate: true},
		"hyphenated relative directory":   {text: "see docs/part-a/intro/index.md"},
		"Next.js dynamic segment":         {text: "app/[slug]/edit/page.tsx"},
		"Next.js route group":             {text: "app/(marketing)/about/page.tsx"},
		"glob":                            {text: "src/**/fixtures/case.json"},
		"TS path alias":                   {text: "@/components/Button"},
		"brace expansion":                 {text: "src/{a,b}/x/"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := carriesPrivateRuntimeDetail(tc.text)

			if got != tc.isPrivate {
				t.Fatalf("carriesPrivateRuntimeDetail(%q) = %v, want %v", tc.text, got, tc.isPrivate)
			}
		})
	}
}

func TestWithheldEntriesNoticeCountsTheEntries(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		0: "",
		1: "1 Test evidence entry withheld from this pull request: it carries a local path or a run or process identifier. The raw evidence is kept privately with the run.",
		3: "3 Test evidence entries withheld from this pull request: they carry local paths or run or process identifiers. The raw evidence is kept privately with the run.",
	}
	for count, want := range cases {
		if got := withheldEntriesNotice(count); got != want {
			t.Errorf("withheldEntriesNotice(%d) = %q, want %q", count, got, want)
		}
	}
}

func TestCollectTestingSummaryDoesNotFallBackPastAWithheldSummary(t *testing.T) {
	t.Parallel()
	earlier := `{"findings":[],"testing_summary":"Baseline go test failed: TestCheckout panics.","tested":["ran /opt/toolchain/bin/python3"]}`
	latest := `{"findings":[],"testing_summary":"All scenarios pass; server log at /tmp/srv/out.log"}`
	sr := &db.StepResult{FindingsJSON: &latest}
	rounds := []*db.StepRound{{Round: 1, FindingsJSON: &earlier}, {Round: 2, FindingsJSON: &latest}}
	withheld := map[string]bool{}

	got := collectTestingSummary(sr, rounds, withheld)

	if got != "" {
		t.Fatalf("collectTestingSummary() = %q, want the withheld latest summary to publish nothing", got)
	}
	if len(withheld) != 1 {
		t.Fatalf("withheld %d entries, want only the latest summary: %v", len(withheld), withheld)
	}
}

func TestCollectTestingSummaryFallsBackWhenTheLatestHasNoSummary(t *testing.T) {
	t.Parallel()
	earlier := `{"findings":[],"testing_summary":"Checkout flow passed."}`
	latest := `{"findings":[]}`
	sr := &db.StepResult{FindingsJSON: &latest}
	rounds := []*db.StepRound{{Round: 1, FindingsJSON: &earlier}, {Round: 2, FindingsJSON: &latest}}

	got := collectTestingSummary(sr, rounds, map[string]bool{})

	if got != "Checkout flow passed." {
		t.Fatalf("collectTestingSummary() = %q, want the earlier round's summary", got)
	}
}

func TestBuildTestingSummaryForPRCountsAWithheldSummaryOnce(t *testing.T) {
	t.Parallel()
	findings := `{"findings":[],"testing_summary":"All scenarios pass; log at /tmp/srv/out.log\n","tested":["go test ./internal/checkout/..."]}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}}}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), t.TempDir(), nil)

	if !strings.Contains(md, "1 Test evidence entry withheld from this pull request") {
		t.Fatalf("expected one withheld entry for the one private summary, got:\n%s", md)
	}
	if strings.Contains(md, "/tmp/srv") {
		t.Fatalf("the private summary was published:\n%s", md)
	}
}
