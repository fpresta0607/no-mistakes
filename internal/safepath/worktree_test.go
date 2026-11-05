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

func TestRedactRunEvidence_RewritesEverySpellingOfTheDirectory(t *testing.T) {
	evidence := filepath.Join(t.TempDir(), "evidence", "01RUN")
	slashed := filepath.ToSlash(evidence)
	backslashed := strings.ReplaceAll(slashed, "/", `\`)
	doubled := strings.ReplaceAll(slashed, "/", `\\`)
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{name: "the directory itself", text: "kept in " + evidence, want: "kept in " + EvidencePlaceholder},
		{name: "forward slashes", text: slashed + "/test-scratch/x.log", want: EvidencePlaceholder + "/test-scratch/x.log"},
		{name: "backslashes", text: backslashed + `\x.log`, want: EvidencePlaceholder + `\x.log`},
		{name: "doubled backslashes", text: doubled + `\\x.log`, want: EvidencePlaceholder + `\\x.log`},
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

func TestRedactRunEvidence_EmptyDirectoryChangesNothing(t *testing.T) {
	text := "kept in /srv/evidence/01RUN"
	if got := RedactRunEvidence(text, ""); got != text {
		t.Errorf("RedactRunEvidence(%q, \"\") = %q", text, got)
	}
}
