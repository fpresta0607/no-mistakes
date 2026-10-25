package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func testList(packages ...packageTests) []packageTests {
	return packages
}

func names(part packageTests) []string {
	return part.Names
}

// capturedRelister adapts a captured list to the relister verifyParts takes, so
// these tests exercise the same per-package re-listing the live tool does.
func capturedRelister(list []packageTests) func(pkg, pattern string) ([]string, error) {
	return func(pkg, pattern string) ([]string, error) {
		return matchCapturedList(list, pkg, pattern)
	}
}

func TestPartitionSplitsEveryPackageExactlyOnce(t *testing.T) {
	t.Parallel()

	list := testList(
		packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG"}},
		packageTests{ImportPath: "example/steps/citest", Names: []string{"TestH", "TestI", "TestJ"}},
	)
	for _, parts := range []int{2, 3, 5} {
		partitioned, err := partitionTests(list, parts)
		if err != nil {
			t.Fatalf("partitionTests(%d) error = %v", parts, err)
		}
		covered := map[string]int{}
		for _, part := range partitioned {
			for _, pkg := range part {
				for _, name := range names(pkg) {
					covered[pkg.ImportPath+" "+name]++
				}
			}
		}
		for _, pkg := range list {
			for _, name := range names(pkg) {
				key := pkg.ImportPath + " " + name
				if count := covered[key]; count != 1 {
					t.Errorf("parts=%d: %s appears in %d parts, want exactly one", parts, key, count)
				}
			}
		}
		if len(covered) != 10 {
			t.Errorf("parts=%d: covered %d tests, want 10", parts, len(covered))
		}
	}
}

func TestPartitionKeepsSortedIndexModuloOrder(t *testing.T) {
	t.Parallel()

	partitioned, err := partitionTests(testList(packageTests{ImportPath: "example/steps", Names: []string{"TestC", "TestA", "TestB", "TestE", "TestD"}}), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"TestA", "TestD"}, {"TestB", "TestE"}, {"TestC"}}
	for index := range want {
		var got []string
		for _, pkg := range partitioned[index] {
			got = append(got, names(pkg)...)
		}
		if !slices.Equal(got, want[index]) {
			t.Errorf("part %d = %v, want %v", index+1, got, want[index])
		}
	}
}

func TestPartPatternSelectsOnlyItsOwnTests(t *testing.T) {
	t.Parallel()

	partitioned, err := partitionTests(testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB", "TestC", "TestD", "TestE"}}), 2)
	if err != nil {
		t.Fatal(err)
	}
	for index, part := range partitioned {
		pattern, err := partPattern(part)
		if err != nil {
			t.Fatalf("part %d pattern: %v", index+1, err)
		}
		compiled := regexp.MustCompile(pattern)
		for _, name := range []string{"TestA", "TestB", "TestC", "TestD", "TestE"} {
			selected := compiled.MatchString(name)
			inPart := false
			for _, pkg := range part {
				if slices.Contains(names(pkg), name) {
					inPart = true
				}
			}
			if selected != inPart {
				t.Errorf("part %d pattern %q selects %s = %v, want %v", index+1, pattern, name, selected, inPart)
			}
		}
	}
}

func TestPartPatternDeduplicatesANameSharedByTwoPackages(t *testing.T) {
	t.Parallel()

	partitioned, err := partitionTests(testList(
		packageTests{ImportPath: "example/a", Names: []string{"TestShared"}},
		packageTests{ImportPath: "example/b", Names: []string{"TestShared"}},
	), 1)
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := partPattern(partitioned[0])
	if err != nil {
		t.Fatal(err)
	}
	if pattern != "^(TestShared)$" {
		t.Fatalf("pattern = %q, want one alternation entry for the shared name", pattern)
	}
}

func TestVerifyPartsAcceptsARealPartition(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB", "TestC", "TestD"}}, packageTests{ImportPath: "example/steps/citest", Names: []string{"TestE"}})
	partitioned, err := partitionTests(list, 3)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := verifyParts(list, partitioned, defaultPatternBytes, capturedRelister(list), &out); err != nil {
		t.Fatalf("verifyParts error = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "verified 5 tests across 3 parts") {
		t.Errorf("verify summary = %q, want the coverage line", out.String())
	}
}

// A pattern that fails to select one of its own tests must fail verification,
// which is what a future escaping or quoting bug would look like.
func TestVerifyPartsRejectsAPatternThatMissesATest(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB"}})
	partitioned, err := partitionTests(list, 1)
	if err != nil {
		t.Fatal(err)
	}
	relist := func(string, string) ([]string, error) { return []string{"TestA"}, nil }
	err = verifyParts(list, partitioned, defaultPatternBytes, relist, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "want [TestA TestB]") {
		t.Fatalf("verifyParts error = %v, want the missing test named", err)
	}
}

// A pattern that selects a test the listing no longer has is not a partition of
// that listing, so verification must refuse it.
func TestVerifyPartsRejectsAnExtraSelection(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB"}})
	partitioned, err := partitionTests(list, 1)
	if err != nil {
		t.Fatal(err)
	}
	relist := func(string, string) ([]string, error) { return []string{"TestA", "TestB", "TestGone"}, nil }
	err = verifyParts(list, partitioned, defaultPatternBytes, relist, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "TestGone") {
		t.Fatalf("verifyParts error = %v, want the extra selection named", err)
	}
}

func TestVerifyPartsRejectsAPatternThatCouldNotFitAWindowsCommandLine(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB"}})
	partitioned, err := partitionTests(list, 1)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyParts(list, partitioned, 8, capturedRelister(list), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "add a part instead of growing it") {
		t.Fatalf("verifyParts error = %v, want the command line budget refusal", err)
	}
}

// A partition that lost its only test cannot even be rendered as a pattern, and
// that must be a refusal rather than an empty -run that silently runs nothing.
func TestVerifyPartsRejectsAPartitionThatLostItsOnlyTest(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA"}})
	partitioned, err := partitionTests(list, 1)
	if err != nil {
		t.Fatal(err)
	}
	partitioned[0] = []packageTests{{ImportPath: "example/steps"}}
	err = verifyParts(list, partitioned, defaultPatternBytes, capturedRelister(list), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "part selects no tests") {
		t.Fatalf("verifyParts error = %v, want the empty part refused", err)
	}
}

// A test selected by two parts would run twice in three jobs, so verification
// must refuse the duplicate instead of reporting a partition.
func TestVerifyPartsRejectsATestSelectedByTwoParts(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB"}})
	partitioned := [][]packageTests{
		{{ImportPath: "example/steps", Names: []string{"TestA", "TestB"}}},
		{{ImportPath: "example/steps", Names: []string{"TestA", "TestB"}}},
	}
	err := verifyParts(list, partitioned, defaultPatternBytes, capturedRelister(list), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "is in 2 parts, want exactly one") {
		t.Fatalf("verifyParts error = %v, want the duplicated test refused", err)
	}
}

func TestRunPrintsTheRequestedPartAndVerifies(t *testing.T) {
	t.Parallel()

	var list bytes.Buffer
	list.WriteString("example/steps\tTestA\n")
	list.WriteString("example/steps\tTestB\n")
	list.WriteString("example/steps\tTestC\n")

	for part := 1; part <= 3; part++ {
		var out bytes.Buffer
		if err := run(options{parts: 3, part: part, listFile: writeTempList(t, list.String()), maxPatternBytes: defaultPatternBytes}, &out); err != nil {
			t.Fatalf("part %d: %v", part, err)
		}
		if !strings.HasPrefix(strings.TrimSpace(out.String()), "^(") {
			t.Errorf("part %d output = %q, want a -run pattern", part, out.String())
		}
	}

	var out bytes.Buffer
	if err := run(options{parts: 3, verify: true, listFile: writeTempList(t, list.String()), maxPatternBytes: defaultPatternBytes}, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, out.String())
	}
}

func TestRunRejectsAPartOutsideTheRange(t *testing.T) {
	t.Parallel()

	var list bytes.Buffer
	list.WriteString("example/steps\tTestA\n")
	err := run(options{parts: 2, part: 3, listFile: writeTempList(t, list.String())}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "between 1 and 2") {
		t.Fatalf("run error = %v, want the part range refusal", err)
	}
}

func writeTempList(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyPartsReportsThePatternBudgetPerPart(t *testing.T) {
	t.Parallel()

	list := testList(packageTests{ImportPath: "example/steps", Names: []string{"TestA", "TestB", "TestC", "TestD"}})
	partitioned, err := partitionTests(list, 2)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := verifyParts(list, partitioned, defaultPatternBytes, capturedRelister(list), &out); err != nil {
		t.Fatalf("verifyParts error = %v", err)
	}
	for part := 1; part <= 2; part++ {
		line := fmt.Sprintf("part %d/2:", part)
		if !strings.Contains(out.String(), line) {
			t.Errorf("verify output %q does not report %q", out.String(), line)
		}
	}
}
