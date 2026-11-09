package realpath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/testlink"
)

func TestResolve_PlainDirectoryIsItsOwnCanonicalForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	got, err := Resolve(dir)

	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("Resolve(%q) = %q, want an absolute path", dir, got)
	}
	assertSameFile(t, got, dir)
	again, err := Resolve(got)
	if err != nil || again != got {
		t.Fatalf("Resolve(%q) = %q, %v; want it unchanged", got, again, err)
	}
}

func TestResolve_RelativePathBecomesAbsolute(t *testing.T) {
	dir := t.TempDir()
	want, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got, err := Resolve(".")

	if err != nil || got != want {
		t.Fatalf("Resolve(.) = %q, %v; want %q", got, err, want)
	}
}

func TestResolve_MissingPathFails(t *testing.T) {
	t.Parallel()

	_, err := Resolve(filepath.Join(t.TempDir(), "missing", "child"))

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Resolve of a missing path: %v, want fs.ErrNotExist", err)
	}
}

func TestResolve_FollowsLinksBelowThePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		targetRoot func(t *testing.T, linkRoot string) string
		link       func(t *testing.T, link, target string)
	}{
		{"symlink", sameVolume, symlink},
		{"junction", sameVolume, testlink.Junction},
		{"junction to another volume", otherVolume, testlink.Junction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			linkRoot := t.TempDir()
			target := filepath.Join(tc.targetRoot(t, linkRoot), "target")
			if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(linkRoot, "link")
			tc.link(t, link, target)
			want, err := Resolve(filepath.Join(target, "child"))
			if err != nil {
				t.Fatal(err)
			}

			got, err := Resolve(filepath.Join(link, "child"))

			if err != nil || got != want {
				t.Fatalf("Resolve through %s = %q, %v; want %q", tc.name, got, err, want)
			}
		})
	}
}

func sameVolume(t *testing.T, _ string) string { return t.TempDir() }

func otherVolume(t *testing.T, linkRoot string) string {
	return testlink.OtherVolumeDir(t, linkRoot)
}

func symlink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here (Windows needs Developer Mode or administrator rights): %v", err)
	}
}

func assertSameFile(t *testing.T, a, b string) {
	t.Helper()
	infoA, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(infoA, infoB) {
		t.Fatalf("%q and %q are different files", a, b)
	}
}
