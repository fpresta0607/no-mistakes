package safepath

import (
	"path/filepath"
	"strings"
	"testing"
)

// A JSON string escape right after a repo-level worktrees path is the escape,
// not a separator plus a run id, so the next word survives.
func TestRedactWorktrees_KeepsJSONEscapeAfterRepoLevelPath(t *testing.T) {
	worktrees := filepath.Join(t.TempDir(), "worktrees")
	for _, esc := range []string{`\n`, `\r`, `\t`} {
		// A slash-spelled root: a backslash after it can only start an escape.
		text := filepath.ToSlash(filepath.Join(worktrees, "ab12")) + esc + "next"
		if got, want := RedactWorktrees(text, "", worktrees), WorktreesPlaceholder+esc+"next"; got != want {
			t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
		}
	}
}

// In text that spells the root with single backslashes (native Windows
// output), a backslash before a run id that starts with n, r or t is still a
// separator.
func TestRedactWorktrees_SingleBackslashRootKeepsBackslashSeparators(t *testing.T) {
	worktrees := filepath.Join(t.TempDir(), "worktrees")
	native := strings.ReplaceAll(worktrees, "/", `\`)
	text := native + `\ab12\run-0\out.log`
	if got, want := RedactWorktrees(text, "", worktrees), WorktreePlaceholder+`\out.log`; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
	}
}

// Both publication boundaries call RedactPublished, so it has to apply every
// rewrite the package owns: one left out reaches neither the pull request nor
// the evidence branch.
func TestRedactPublished_AppliesEveryPathRewrite(t *testing.T) {
	text := "ran /nm/worktrees/ab12cd34ef56/run-1/bin/tool, kept /nm/evidence/01RUN/out.log, read /home/alice/notes.txt"
	got := RedactPublished(text, "/nm/worktrees/ab12cd34ef56/run-1", "/nm/worktrees", "/nm/evidence/01RUN")
	want := "ran " + WorktreePlaceholder + "/bin/tool, kept " + EvidencePlaceholder + "/out.log, read " + Placeholder + "/notes.txt"
	if got != want {
		t.Errorf("RedactPublished(%q) = %q, want %q", text, got, want)
	}
}

func TestRedactRunEvidence_RewritesEverySpellingOfTheDirectory(t *testing.T) {
	evidence := filepath.Join(t.TempDir(), "evidence", "01RUN")
	slashed := filepath.ToSlash(evidence)
	backslashed := strings.ReplaceAll(slashed, "/", `\`)
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{name: "the directory itself", text: "kept in " + evidence, want: "kept in " + EvidencePlaceholder},
		{name: "forward slashes", text: slashed + "/test-scratch/x.log", want: EvidencePlaceholder + "/test-scratch/x.log"},
		{name: "backslashes", text: backslashed + `\x.log`, want: EvidencePlaceholder + `\x.log`},
		{name: "every occurrence", text: slashed + " and " + slashed, want: EvidencePlaceholder + " and " + EvidencePlaceholder},
		{name: "a sibling sharing the prefix", text: slashed + "-other/x.log", want: slashed + "-other/x.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactRunEvidence(tc.text, evidence); got != tc.want {
				t.Errorf("RedactRunEvidence(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// JSON escaping doubles the backslashes a Windows directory already has. A POSIX
// directory has none to double, so the fixture is Windows on every platform.
func TestRedactRunEvidence_RewritesTheJSONEscapedSpelling(t *testing.T) {
	const text = `C:\\nm\\evidence\\01RUN\\x.log`
	got := RedactRunEvidence(text, `C:\nm\evidence\01RUN`)
	if want := EvidencePlaceholder + `\\x.log`; got != want {
		t.Errorf("RedactRunEvidence(%q) = %q, want %q", text, got, want)
	}
}

func TestRedactRunEvidence_EmptyDirectoryChangesNothing(t *testing.T) {
	text := "kept in /srv/evidence/01RUN"
	if got := RedactRunEvidence(text, ""); got != text {
		t.Errorf("RedactRunEvidence(%q, \"\") = %q", text, got)
	}
}

// A Windows run's directory reaches published text through shells and tools
// that spell it differently from the daemon, whichever case the daemon's drive
// letter has.
func TestRedactors_MatchEveryDriveSpelling(t *testing.T) {
	for _, dir := range []string{
		`C:\Users\operator\.no-mistakes\evidence\01RUN`,
		`c:\Users\operator\.no-mistakes\evidence\01RUN`,
	} {
		for _, tc := range []struct {
			name     string
			spelling string
		}{
			{name: "upper-case drive", spelling: `C:\Users\operator\.no-mistakes\evidence\01RUN`},
			{name: "upper-case drive forward slashes", spelling: "C:/Users/operator/.no-mistakes/evidence/01RUN"},
			{name: "lower-case drive", spelling: `c:\Users\operator\.no-mistakes\evidence\01RUN`},
			{name: "lower-case drive forward slashes", spelling: "c:/Users/operator/.no-mistakes/evidence/01RUN"},
			{name: "lower-case drive doubled backslashes", spelling: `c:\\Users\\operator\\.no-mistakes\\evidence\\01RUN`},
			{name: "git bash", spelling: "/c/Users/operator/.no-mistakes/evidence/01RUN"},
			{name: "git bash upper-case drive", spelling: "/C/Users/operator/.no-mistakes/evidence/01RUN"},
			{name: "cygwin", spelling: "/cygdrive/c/Users/operator/.no-mistakes/evidence/01RUN"},
			{name: "wsl", spelling: "/mnt/c/Users/operator/.no-mistakes/evidence/01RUN"},
		} {
			t.Run(dir[:1]+" "+tc.name, func(t *testing.T) {
				text := "kept in " + tc.spelling + "/out.log"
				if got, want := RedactRunEvidence(text, dir), "kept in "+EvidencePlaceholder+"/out.log"; got != want {
					t.Errorf("RedactRunEvidence(%q) = %q, want %q", text, got, want)
				}
				if got, want := RedactWorktrees(text, dir, ""), "kept in "+WorktreePlaceholder+"/out.log"; got != want {
					t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
				}
			})
		}
	}
}

func TestRedactRunEvidence_RewritesADriveFileURL(t *testing.T) {
	const text = "opened file:///C:/Users/operator/.no-mistakes/evidence/01RUN/report.html"
	got := RedactRunEvidence(text, `C:\Users\operator\.no-mistakes\evidence\01RUN`)
	if want := "opened file:///" + EvidencePlaceholder + "/report.html"; got != want {
		t.Errorf("RedactRunEvidence(%q) = %q, want %q", text, got, want)
	}
}

// Text an agent wrote from "~" never holds the absolute directory, and home
// redaction leaves that form, run ID included, exactly as it found it.
func TestRedactors_MatchTheHomeRelativeSpelling(t *testing.T) {
	const home = `C:\Users\operator`
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const dir = home + `\.no-mistakes\evidence\01RUN`
	for _, spelling := range []string{
		"~/.no-mistakes/evidence/01RUN",
		`~\.no-mistakes\evidence\01RUN`,
		`~\\.no-mistakes\\evidence\\01RUN`,
	} {
		t.Run(spelling, func(t *testing.T) {
			text := "kept in " + spelling + "/out.log"
			if got, want := RedactRunEvidence(text, dir), "kept in "+EvidencePlaceholder+"/out.log"; got != want {
				t.Errorf("RedactRunEvidence(%q) = %q, want %q", text, got, want)
			}
			if got, want := RedactWorktrees(text, dir, ""), "kept in "+WorktreePlaceholder+"/out.log"; got != want {
				t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
			}
		})
	}

	const text = "~/.no-mistakes/worktrees/ab12/01RUN/out.log"
	if got, want := RedactWorktrees(text, "", home+`\.no-mistakes\worktrees`), WorktreePlaceholder+"/out.log"; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
	}
}

// Length caps run ahead of the boundary, so a path can arrive stopped inside
// the run ID, while a different directory beside it must survive.
func TestRedactors_RewriteAPathCutInsideTheRunID(t *testing.T) {
	const posix = "/srv/nm/evidence/01RUNIDFIXTURE"
	const windows = `C:\nm\evidence\01RUNIDFIXTURE`
	for _, tc := range []struct {
		name string
		dir  string
		text string
		want string
	}{
		{name: "before an ellipsis", dir: posix, text: "log at /srv/nm/evidence/01RUN…", want: "log at " + EvidencePlaceholder + "…"},
		{name: "before three dots", dir: posix, text: "log at /srv/nm/evidence/01RUN...", want: "log at " + EvidencePlaceholder + "..."},
		{name: "at the end of the text", dir: posix, text: "/srv/nm/evidence/01R", want: EvidencePlaceholder},
		{name: "backslashes", dir: windows, text: `C:\nm\evidence\01RUN…`, want: EvidencePlaceholder + "…"},
		{name: "doubled backslashes", dir: windows, text: `C:\\nm\\evidence\\01RUN…`, want: EvidencePlaceholder + "…"},
		{name: "git bash", dir: windows, text: "/c/nm/evidence/01RUN…", want: EvidencePlaceholder + "…"},
		{name: "before the run ID starts", dir: posix, text: "/srv/nm/evidence/…", want: "/srv/nm/evidence/…"},
		{name: "before the run ID starts, three dots", dir: posix, text: "/srv/nm/evidence/...", want: "/srv/nm/evidence/..."},
		{name: "another run's directory", dir: posix, text: "/srv/nm/evidence/01OTHER/out.log", want: "/srv/nm/evidence/01OTHER/out.log"},
		{name: "a name that only starts alike", dir: posix, text: "/srv/nm/evidence/01RUNX…", want: "/srv/nm/evidence/01RUNX…"},
		{name: "a name that only starts alike, before a period", dir: posix, text: "/srv/nm/evidence/01RUNX.", want: "/srv/nm/evidence/01RUNX."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactRunEvidence(tc.text, tc.dir); got != tc.want {
				t.Errorf("RedactRunEvidence(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}

	const text = "/srv/trees/ab12/01RUN…"
	if got, want := RedactWorktrees(text, "/srv/trees/ab12/01RUNIDFIXTURE", ""), WorktreePlaceholder+"…"; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", text, got, want)
	}
}

// A period is a name byte, so a directory that ends a sentence reads as a longer
// name unless the period stays out of the comparison, and it has to survive for
// the sentence to read the same.
func TestRedactors_RewriteADirectoryEndingASentence(t *testing.T) {
	const dir = "/srv/nm/evidence/01RUNIDFIXTURE"
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{name: "at the end of the text", text: "Logs are in /srv/nm/evidence/01RUNIDFIXTURE.", want: "Logs are in " + EvidencePlaceholder + "."},
		{name: "before more prose", text: "In /srv/nm/evidence/01RUNIDFIXTURE. Then done.", want: "In " + EvidencePlaceholder + ". Then done."},
		{name: "inside parentheses", text: "(see /srv/nm/evidence/01RUNIDFIXTURE.)", want: "(see " + EvidencePlaceholder + ".)"},
		{name: "a longer name", text: "Logs are in /srv/nm/evidence/01RUNIDFIXTURE2.", want: "Logs are in /srv/nm/evidence/01RUNIDFIXTURE2."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactRunEvidence(tc.text, dir); got != tc.want {
				t.Errorf("RedactRunEvidence(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}

	// A configured placement is <root>/<run-id>, outside any worktrees root, so
	// no fallback sees it.
	const placed = "kept in /srv/trees/01RUNIDFIXTURE."
	if got, want := RedactWorktrees(placed, "/srv/trees/01RUNIDFIXTURE", ""), "kept in "+WorktreePlaceholder+"."; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", placed, got, want)
	}
	const fallback = "saw /nm/worktrees/ab12/run-0."
	if got, want := RedactWorktrees(fallback, "", "/nm/worktrees"), "saw "+WorktreePlaceholder+"."; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", fallback, got, want)
	}
	const root = "all under /nm/worktrees."
	if got, want := RedactWorktrees(root, "", "/nm/worktrees"), "all under "+WorktreesPlaceholder+"."; got != want {
		t.Errorf("RedactWorktrees(%q) = %q, want %q", root, got, want)
	}
}
