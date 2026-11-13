package testoutputdirs

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/realpath"
	"github.com/kunchenguid/no-mistakes/internal/testlink"
)

type admissionFixture struct {
	repo, home, outside string
}

func newAdmissionFixture(t *testing.T) admissionFixture {
	t.Helper()
	fixture := admissionFixture{repo: t.TempDir(), home: t.TempDir(), outside: t.TempDir()}
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

	got, err := Validate([]string{fixture.outside}, fixture.repo, fixture.home)

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

	got, err := Validate([]string{fixture.outside, other, spelled, fixture.outside}, fixture.repo, fixture.home)

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
			got, err := Validate([]string{fixture.outside, tc.value}, fixture.repo, fixture.home)

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
	home := filepath.Join(fixture.outside, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Validate([]string{fixture.outside}, fixture.repo, home)

	want := "--test-output-dir " + fixture.outside + " holds the no-mistakes home; choose a directory that does not contain it"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
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

	_, err := Validate([]string{link}, fixture.repo, fixture.home)

	if err == nil || !strings.Contains(err.Error(), "is inside the repository") {
		t.Fatalf("a link into the repository = %v, want refused as inside it", err)
	}
}

func TestValidateWithNothingRequestedAdmitsNothing(t *testing.T) {
	fixture := newAdmissionFixture(t)

	got, err := Validate(nil, fixture.repo, fixture.home)

	if err != nil || got != nil {
		t.Fatalf("Validate(nil) = %q, %v; want nil, nil", got, err)
	}
}
