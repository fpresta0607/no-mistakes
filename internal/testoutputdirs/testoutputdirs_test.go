package testoutputdirs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/testlink"
)

type admissionFixture struct {
	repo, home, outside string
}

func newAdmissionFixture(t *testing.T) admissionFixture {
	t.Helper()
	fixture := admissionFixture{repo: t.TempDir(), home: t.TempDir(), outside: t.TempDir()}
	runGit(t, fixture.repo, "init")
	runGit(t, fixture.repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	for _, dir := range []string{filepath.Join(fixture.repo, "out"), filepath.Join(fixture.home, "worktrees")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.outside, "file.txt"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeGlobalConfig(t *testing.T, home, yaml string) {
	t.Helper()
	if err := os.WriteFile(paths.WithRoot(home).ConfigFile(), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f admissionFixture) validate(values ...string) ([]string, error) {
	return Validate(context.Background(), values, f.repo, paths.WithRoot(f.home))
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := realpath.Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestValidateAcceptsAnExistingAbsoluteDirectoryOutsideTheRepositoryAndHome(t *testing.T) {
	fixture := newAdmissionFixture(t)

	got, err := fixture.validate(fixture.outside)

	if err != nil {
		t.Fatalf("Validate refused an admissible directory: %v", err)
	}
	if want := []string{canonical(t, fixture.outside)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate = %q, want %q", got, want)
	}
}

func TestValidateDeduplicatesSpellingsOfOneDirectory(t *testing.T) {
	fixture := newAdmissionFixture(t)
	other := t.TempDir()
	spelled := filepath.Join(fixture.outside, "sub", "..") + string(filepath.Separator)
	if err := os.MkdirAll(filepath.Join(fixture.outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := fixture.validate(fixture.outside, other, spelled, fixture.outside)

	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want := []string{canonical(t, fixture.outside), canonical(t, other)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate = %q, want %q", got, want)
	}
}

func TestValidateRefusesWithTheValueTheRuleAndTheCorrection(t *testing.T) {
	fixture := newAdmissionFixture(t)
	volumeRoot := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		volumeRoot = filepath.VolumeName(fixture.outside) + string(filepath.Separator)
	}
	for _, tc := range []struct {
		name, value, wantRule, wantCorrection string
	}{
		{"relative", filepath.Join("relative", "out"), "is not an absolute path", "pass the directory's absolute path"},
		{"missing", filepath.Join(fixture.outside, "missing"), "does not exist", "create it first or pass an existing directory"},
		{"file", filepath.Join(fixture.outside, "file.txt"), "is not a directory", "pass a directory"},
		{"filesystem_root", volumeRoot, "is a filesystem root", "choose a directory below it"},
		{"inside_repository", filepath.Join(fixture.repo, "out"), "is inside the repository", "choose a directory outside it"},
		{"repository_itself", fixture.repo, "is inside the repository", "choose a directory outside it"},
		{"inside_home", filepath.Join(fixture.home, "worktrees"), "is inside the no-mistakes home", "choose a directory outside it"},
		{"holds_repository", filepath.Dir(fixture.repo), "holds the repository", "choose a directory that does not contain it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fixture.validate(fixture.outside, tc.value)

			if err == nil {
				t.Fatalf("Validate admitted %q: %q", tc.value, got)
			}
			want := "--test-output-dir " + tc.value + " " + tc.wantRule + "; " + tc.wantCorrection
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
		})
	}
}

func TestValidateRefusesADirectoryHoldingTheHome(t *testing.T) {
	fixture := newAdmissionFixture(t)
	fixture.home = filepath.Join(fixture.outside, "home")
	if err := os.MkdirAll(fixture.home, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := fixture.validate(fixture.outside)

	want := "--test-output-dir " + fixture.outside + " holds the no-mistakes home; choose a directory that does not contain it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestValidateRefusesARunWorktreeRootOutsideTheHome(t *testing.T) {
	fixture := newAdmissionFixture(t)
	root := t.TempDir()
	writeGlobalConfig(t, fixture.home, "worktree_roots:\n  '"+fixture.repo+"': '"+root+"'\n")

	_, err := fixture.validate(root)

	want := "--test-output-dir " + root + " is inside a run worktree root; choose a directory outside it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestValidateRefusesAnEvidenceRootOutsideTheHome(t *testing.T) {
	fixture := newAdmissionFixture(t)
	evidence := t.TempDir()
	writeGlobalConfig(t, fixture.home, "evidence_dir: '"+evidence+"'\n")

	_, err := fixture.validate(evidence)

	want := "--test-output-dir " + evidence + " is inside the evidence root; choose a directory outside it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestValidateRefusesADirectoryInsideALinkedWorktreeOfTheRepository(t *testing.T) {
	fixture := newAdmissionFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, fixture.repo, "worktree", "add", "-b", "linked", linked)
	value := filepath.Join(linked, "out")
	if err := os.MkdirAll(value, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := fixture.validate(value)

	want := "--test-output-dir " + value + " is inside a worktree of the repository; choose a directory outside it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestValidateFailsClosedWhenTheProtectedLocationsCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, fixture *admissionFixture)
	}{
		{"unreadable_global_config", func(t *testing.T, fixture *admissionFixture) {
			writeGlobalConfig(t, fixture.home, "not_a_known_key: true\n")
		}},
		{"checkout_is_not_a_git_repository", func(t *testing.T, fixture *admissionFixture) {
			fixture.repo = t.TempDir()
			t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(fixture.repo))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAdmissionFixture(t)
			tc.arrange(t, &fixture)

			got, err := fixture.validate(fixture.outside)

			if err == nil || !strings.HasPrefix(err.Error(), "--test-output-dir cannot be checked: ") {
				t.Fatalf("Validate = %q, %v; want refused as uncheckable", got, err)
			}
		})
	}
}

func TestValidateResolvesALinkIntoTheRepositoryAsInside(t *testing.T) {
	fixture := newAdmissionFixture(t)
	link := filepath.Join(fixture.outside, "link")
	target := filepath.Join(fixture.repo, "out")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatalf("symlink: %v", err)
		}
		testlink.Junction(t, link, target)
	}

	_, err := fixture.validate(link)

	if err == nil || !strings.Contains(err.Error(), "is inside the repository") {
		t.Fatalf("a link into the repository = %v, want refused as inside it", err)
	}
}

func TestValidateWithNothingRequestedAdmitsNothing(t *testing.T) {
	fixture := newAdmissionFixture(t)

	got, err := fixture.validate()

	if err != nil || got != nil {
		t.Fatalf("Validate(nil) = %q, %v; want nil, nil", got, err)
	}
}
