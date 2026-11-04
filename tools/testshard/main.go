// Command testshard partitions the top-level tests of Go packages into N parts
// and prints the go test -run pattern for one part.
//
// Why it exists: internal/pipeline/steps is a single package whose ~1000
// top-level tests cost about 1600-1850s of test time on a 4-core Windows
// runner, more than the 15m go test budget, and Windows process creation is the
// wall. The CI workflow runs one part per job and keeps -timeout and the job cap
// unchanged, with no test skipped and no parallelism lowered.
//
// The list is the packages' own `go test -list '^Test'` output, so a test added
// later is inside a part automatically. -verify proves the parts cover every
// listed test exactly once by re-listing each part through Go's own -run
// matching, and refuses a pattern that would not fit a Windows command line.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// defaultPatternBytes is the largest per-part -run pattern this tool will emit.
// Windows caps a whole command line at 32767 characters, and the pattern travels
// to the test binary inside it, so a part must stay far below that.
const defaultPatternBytes = 24000

type packageTests struct {
	ImportPath string
	Names      []string
}

type options struct {
	packages        string
	parts           int
	part            int
	verify          bool
	dumpList        bool
	listFile        string
	maxPatternBytes int
}

func main() {
	opts := options{}
	flag.StringVar(&opts.packages, "packages", "./internal/pipeline/steps/...", "comma-separated Go package patterns to shard")
	flag.IntVar(&opts.parts, "parts", 3, "number of parts to split the tests into")
	flag.IntVar(&opts.part, "part", 0, "1-based part whose -run pattern is printed")
	flag.BoolVar(&opts.verify, "verify", false, "verify that the parts cover every listed test exactly once, without printing a pattern")
	flag.BoolVar(&opts.dumpList, "dump-list", false, "print the package<TAB>test list the parts are computed from")
	flag.StringVar(&opts.listFile, "list-file", "", "read a captured package<TAB>test list instead of running go test -list")
	flag.IntVar(&opts.maxPatternBytes, "max-pattern-bytes", defaultPatternBytes, "largest -run pattern a part may emit")
	flag.Parse()

	if err := run(opts, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "testshard: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options, out io.Writer) error {
	if opts.parts < 1 {
		return fmt.Errorf("-parts must be at least 1, got %d", opts.parts)
	}
	var (
		list   []packageTests
		relist func(pkg, pattern string) ([]string, error)
		err    error
	)
	if opts.listFile != "" {
		list, err = readListFile(opts.listFile)
		if err != nil {
			return err
		}
		relist = func(pkg, pattern string) ([]string, error) {
			return matchCapturedList(list, pkg, pattern)
		}
	} else {
		packages, err := listPackages(opts.packages)
		if err != nil {
			return err
		}
		list, err = listAllTests(packages)
		if err != nil {
			return err
		}
		relist = func(pkg, pattern string) ([]string, error) {
			return listMatchingTests(pkg, pattern)
		}
	}

	parts, err := partitionTests(list, opts.parts)
	if err != nil {
		return err
	}
	if opts.dumpList {
		for _, pkg := range list {
			for _, name := range pkg.Names {
				if _, err := fmt.Fprintf(out, "%s\t%s\n", pkg.ImportPath, name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if opts.verify {
		return verifyParts(list, parts, opts.maxPatternBytes, relist, out)
	}
	if opts.part < 1 || opts.part > opts.parts {
		return fmt.Errorf("-part must be between 1 and %d, got %d", opts.parts, opts.part)
	}
	pattern, err := partPattern(parts[opts.part-1])
	if err != nil {
		return err
	}
	if len(pattern) > opts.maxPatternBytes {
		return fmt.Errorf("part %d pattern is %d bytes, over the %d byte limit", opts.part, len(pattern), opts.maxPatternBytes)
	}
	_, err = fmt.Fprintln(out, pattern)
	return err
}

// partitionTests splits every package's sorted test names across the parts by
// index modulo the part count, so each package is covered exactly once and each
// part gets an even share of its names.
func partitionTests(list []packageTests, parts int) ([][]packageTests, error) {
	if parts < 1 {
		return nil, fmt.Errorf("parts must be at least 1, got %d", parts)
	}
	buckets := make([][]packageTests, parts)
	for _, pkg := range list {
		names := slices.Clone(pkg.Names)
		sort.Strings(names)
		for index, name := range names {
			part := index % parts
			if len(buckets[part]) == 0 || buckets[part][len(buckets[part])-1].ImportPath != pkg.ImportPath {
				buckets[part] = append(buckets[part], packageTests{ImportPath: pkg.ImportPath})
			}
			last := &buckets[part][len(buckets[part])-1]
			last.Names = append(last.Names, name)
		}
	}
	return buckets, nil
}

// partPattern renders one part as the -run pattern the workflow passes to go
// test. Names repeat across packages only when the same test name exists in more
// than one package, where matching both is what a single -run invocation does.
func partPattern(part []packageTests) (string, error) {
	seen := make(map[string]struct{})
	var names []string
	for _, pkg := range part {
		for _, name := range pkg.Names {
			if _, done := seen[name]; done {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", errors.New("part selects no tests")
	}
	sort.Strings(names)
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(quoted, "|") + ")$", nil
}

// verifyParts proves the parts are a partition: every listed test is selected by
// exactly one part, no part selects a test the listing did not have, and every
// part pattern fits the command line budget. The check re-lists each part through
// the same matching go test uses, so an escaping bug cannot pass silently.
func verifyParts(list []packageTests, parts [][]packageTests, maxPatternBytes int, relist func(pkg, pattern string) ([]string, error), out io.Writer) error {
	seen := make(map[string]map[string]int)
	for _, pkg := range list {
		seen[pkg.ImportPath] = make(map[string]int, len(pkg.Names))
	}
	total := 0
	for _, pkg := range list {
		total += len(pkg.Names)
	}
	for index, part := range parts {
		pattern, err := partPattern(part)
		if err != nil {
			return fmt.Errorf("part %d: %w", index+1, err)
		}
		if len(pattern) > maxPatternBytes {
			return fmt.Errorf("part %d pattern is %d bytes, over the %d byte limit: add a part instead of growing it", index+1, len(pattern), maxPatternBytes)
		}
		names := 0
		for _, pkg := range part {
			names += len(pkg.Names)
			selected, err := relist(pkg.ImportPath, pattern)
			if err != nil {
				return fmt.Errorf("part %d: re-list %s: %w", index+1, pkg.ImportPath, err)
			}
			expected := slices.Clone(pkg.Names)
			got := slices.Clone(selected)
			sort.Strings(expected)
			sort.Strings(got)
			if !slices.Equal(expected, got) {
				return fmt.Errorf("part %d pattern selects %v from %s, want %v", index+1, got, pkg.ImportPath, expected)
			}
			for _, name := range pkg.Names {
				seen[pkg.ImportPath][name]++
			}
		}
		fmt.Fprintf(out, "part %d/%d: %d tests, %d byte pattern\n", index+1, len(parts), names, len(pattern))
	}
	for _, pkg := range list {
		for _, name := range pkg.Names {
			switch count := seen[pkg.ImportPath][name]; count {
			case 1:
			case 0:
				return fmt.Errorf("%s %s is in no part", pkg.ImportPath, name)
			default:
				return fmt.Errorf("%s %s is in %d parts, want exactly one", pkg.ImportPath, name, count)
			}
		}
		if len(pkg.Names) != len(seen[pkg.ImportPath]) {
			return fmt.Errorf("%s: parts cover %d distinct tests, listing has %d", pkg.ImportPath, len(seen[pkg.ImportPath]), len(pkg.Names))
		}
	}
	fmt.Fprintf(out, "verified %d tests across %d parts\n", total, len(parts))
	return nil
}

func listPackages(patterns string) ([]string, error) {
	var packages []string
	for _, pattern := range strings.Split(patterns, ",") {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}", pattern)
		output, err := cmd.Output()
		if err != nil {
			var stderr strings.Builder
			if exitErr, ok := err.(*exec.ExitError); ok {
				stderr.Write(exitErr.Stderr)
			}
			return nil, fmt.Errorf("go list %s: %w: %s", pattern, err, strings.TrimSpace(stderr.String()))
		}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !slices.Contains(packages, line) {
				packages = append(packages, line)
			}
		}
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("no packages matched %q", patterns)
	}
	sort.Strings(packages)
	return packages, nil
}

func listAllTests(packages []string) ([]packageTests, error) {
	list := make([]packageTests, 0, len(packages))
	for _, pkg := range packages {
		names, err := listMatchingTests(pkg, "^Test")
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			continue
		}
		list = append(list, packageTests{ImportPath: pkg, Names: names})
	}
	if len(list) == 0 {
		return nil, errors.New("no top-level tests found")
	}
	return list, nil
}

// listMatchingTests returns the package's top-level tests that go test -run
// would select for the pattern, using go test's own -list so the tool and the
// workflow cannot disagree about what a name means. TestMain is not a test and
// is never selected.
func listMatchingTests(pkg, pattern string) ([]string, error) {
	cmd := exec.Command("go", "test", "-list", pattern, pkg)
	output, err := cmd.Output()
	if err != nil {
		var stderr strings.Builder
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr.Write(exitErr.Stderr)
		}
		return nil, fmt.Errorf("go test -list %q %s: %w: %s", pattern, pkg, err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == "TestMain" || !strings.HasPrefix(name, "Test") {
			continue
		}
		switch {
		case strings.HasPrefix(name, "ok "), strings.HasPrefix(name, "?"), strings.Contains(name, " "):
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func readListFile(path string) ([]packageTests, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read list file: %w", err)
	}
	byPackage := make(map[string][]string)
	var order []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("list file line %q is not package<TAB>test", line)
		}
		pkg, name := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		if _, done := byPackage[pkg]; !done {
			order = append(order, pkg)
		}
		byPackage[pkg] = append(byPackage[pkg], name)
	}
	if len(order) == 0 {
		return nil, errors.New("list file is empty")
	}
	sort.Strings(order)
	list := make([]packageTests, 0, len(order))
	for _, pkg := range order {
		names := byPackage[pkg]
		sort.Strings(names)
		list = append(list, packageTests{ImportPath: pkg, Names: names})
	}
	return list, nil
}

// matchCapturedList applies the pattern to a captured list with go's own
// matching rules, so -list-file runs exercise the same check as a live listing.
func matchCapturedList(list []packageTests, pkg, pattern string) ([]string, error) {
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}
	var names []string
	for _, entry := range list {
		if entry.ImportPath != pkg {
			continue
		}
		for _, name := range entry.Names {
			if compiled.MatchString(name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}
