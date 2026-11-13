package steps

import "testing"

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
