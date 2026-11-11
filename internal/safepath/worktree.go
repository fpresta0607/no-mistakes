package safepath

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/realpath"
)

// Run worktree placeholders. A run worktree lives at
// <NM_HOME>/worktrees/<repo-id>/<run-id>, where <repo-id> is a short hash of
// the absolute local checkout path: published, it lets anyone confirm a guessed
// account name and layout offline, and an NM_HOME outside the home directory
// publishes whole. Like Placeholder, neither token is angle-bracketed, because
// GitHub renders published .md evidence and drops unknown tags.
const (
	WorktreePlaceholder  = "{run-worktree}"
	WorktreesPlaceholder = "{worktrees}"
)

// EvidencePlaceholder replaces a run's evidence directory, <root>/<run-id>:
// home redaction alone leaves the run ID and the directory layout standing.
const EvidencePlaceholder = "{run-evidence}"

// RedactPublished applies every path rewrite this package owns to text that is
// about to leave the machine: the run's evidence directory, the run worktree
// paths, then the home directory. Both publication boundaries call it, so a
// rewrite added here reaches the pull request and the evidence branch alike. Any
// directory argument may be empty.
func RedactPublished(text, runWorktree, worktreesRoot, runEvidence string) string {
	return RedactText(RedactWorktrees(RedactRunEvidence(text, runEvidence), runWorktree, worktreesRoot))
}

// RedactWorktrees rewrites the run's worktree root, and as a fallback any path
// into worktreesRoot, to the worktree placeholders. Either argument may be
// empty.
func RedactWorktrees(text, runWorktree, worktreesRoot string) string {
	text = replaceDirectory(text, runWorktree, WorktreePlaceholder)
	for _, root := range publishedSpellings(worktreesRoot) {
		single := separatesWithSingleBackslash(root)
		text = replacePathPrefix(text, root, func(rest string) (int, string) {
			// <repo-id>/<run-id> names one run worktree; anything shorter
			// still drops the repo id.
			repoID := pathSegment(rest, single)
			if repoID == 0 {
				return 0, WorktreesPlaceholder
			}
			if runID := pathSegment(rest[repoID:], single); runID > 0 {
				return repoID + runID, WorktreePlaceholder
			}
			return repoID, WorktreesPlaceholder
		})
	}
	return text
}

// RedactRunEvidence rewrites the run's evidence directory to
// EvidencePlaceholder. An empty argument changes nothing.
func RedactRunEvidence(text, runEvidence string) string {
	return replaceDirectory(text, runEvidence, EvidencePlaceholder)
}

// replaceDirectory rewrites every spelling of dir to placeholder. Length caps
// run ahead of the publication boundary and can stop a path inside dir's last
// segment, the run ID, so a leading part of that segment counts as dir too.
func replaceDirectory(text, dir, placeholder string) string {
	for _, root := range publishedSpellings(dir) {
		text = replacePathPrefix(text, root, func(string) (int, string) { return 0, placeholder })
	}
	dir = filepath.Clean(dir)
	cut := strings.LastIndexAny(dir, `/\`)
	if cut < 0 {
		return text
	}
	name := dir[cut+1:]
	for _, parent := range publishedSpellings(dir[:cut]) {
		single := separatesWithSingleBackslash(parent)
		text = replacePathPrefix(text, parent, func(rest string) (int, string) {
			segment := pathSegment(rest, single)
			if segment > 0 && strings.HasPrefix(name, strings.TrimLeft(rest[:segment], `/\`)) {
				return segment, placeholder
			}
			return 0, parent
		})
	}
	return text
}

// publishedSpellings adds to pathSpellings what home redaction leaves of each
// spelling, the form a path has when its author already wrote it from "~".
func publishedSpellings(dir string) []string {
	spellings := pathSpellings(dir)
	for _, spelling := range spellings {
		spellings = append(spellings, RedactText(spelling))
	}
	return spellings
}

// separatesWithSingleBackslash reports whether a single backslash after root is
// a separator: it is only in text that spells root with single backslashes, and
// after "/" or "\\" it starts a JSON escape.
func separatesWithSingleBackslash(root string) bool {
	return strings.Contains(root, `\`) && !strings.Contains(root, `\\`)
}

// pathSegment returns the length of the separator plus path segment name at
// the start of rest, or 0 when rest does not start with one. A single
// backslash counts as a separator only when singleBackslash is set. Trailing
// dots stay out of the name: they end a sentence or mark a length cap's cut.
func pathSegment(rest string, singleBackslash bool) int {
	sep := 0
	switch {
	case strings.HasPrefix(rest, `\\`):
		sep = 2
	case strings.HasPrefix(rest, "/"), singleBackslash && strings.HasPrefix(rest, `\`):
		sep = 1
	default:
		return 0
	}
	n := sep
	for n < len(rest) && isPathNameByte(rest[n]) {
		n++
	}
	for n > sep && rest[n-1] == '.' {
		n--
	}
	if n == sep {
		return 0
	}
	return n
}

// pathSpellings lists the spellings an absolute directory can appear as in
// captured output, longest first, the same set homeCandidates builds.
func pathSpellings(dir string) []string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(candidate string) {
		if usableHomeCandidate(candidate) && !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	dir = filepath.Clean(dir)
	roots := []string{dir}
	if resolved, err := realpath.Resolve(dir); err == nil {
		roots = append(roots, filepath.Clean(resolved))
	}
	for _, root := range roots {
		addBothSeparatorSpellings(add, root)
		addDriveSpellings(add, root)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
