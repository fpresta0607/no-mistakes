package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// FLAGGED_MARKER ends each fixture line whose *string argument a formatting
// verb would print as an address, which is what the tool must report.
const FLAGGED_MARKER = "// flagged"

var fixtureModule = map[string]string{
	"go.mod": "module example.com/fixture\n\ngo 1.26\n",
	"flagged/cases.go": `package flagged

import (
	"fmt"
	"log"
)

type Run struct{ Error *string }

type Status string

type row struct{}

func (row) Scan(destinations ...any) error { return nil }

func refuse(code, format string, args ...any) error { return fmt.Errorf(code+format, args...) }

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cases(run Run, status *Status, count *int, text string) {
	_ = fmt.Sprintf("error = %v", run.Error) // flagged
	_ = fmt.Sprintf("error = %#v", run.Error) // flagged
	_ = fmt.Sprintln("error", run.Error) // flagged
	_ = (fmt.Sprintf)("error = %v", run.Error) // flagged
	log.Printf("status = %v", status) // flagged
	_ = refuse("code", "error = %v", run.Error) // flagged
	_ = fmt.Sprintf("error = %v", deref(run.Error))
	_ = fmt.Sprintf("count = %v, text = %s", count, text)
	_ = row{}.Scan(run.Error)
	_, _ = fmt.Sscanf(text, "%s", &text)
	_, _ = fmt.Scanf("%s", run.Error)
}
`,
	"flagged/cases_test.go": `package flagged

import "testing"

func TestCases(t *testing.T) {
	var run Run
	t.Fatalf("run error = %v", run.Error) // flagged
	t.Error("run error", run.Error) // flagged
	t.Logf("run error = %v", deref(run.Error))
}
`,
	"clean/clean.go": `package clean

import "fmt"

func describe(err *string) string {
	if err == nil {
		return "no error"
	}
	return fmt.Sprintf("error = %v", *err)
}
`,
}

var diagnosticLine = regexp.MustCompile(`^(.+\.go):(\d+):\d+: a \*string is formatted`)

func TestVetToolReportsFormattedStringPointersThroughGoVet(t *testing.T) {
	// Arrange
	tool := buildVetTool(t)
	moduleDir := t.TempDir()
	for name, content := range fixtureModule {
		path := filepath.Join(moduleDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("reports every marked line and nothing else", func(t *testing.T) {
		// Act
		output, err := runGo(moduleDir, "vet", "-vettool="+tool, "./...")

		// Assert
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("go vet = %v, want a non-zero exit for the flagged package\n%s", err, output)
		}
		want := flaggedLines(t)
		got := reportedLines(output)
		if !slices.Equal(got, want) {
			t.Fatalf("reported lines = %q, want %q\n%s", got, want, output)
		}
	})

	t.Run("passes a package that formats only strings", func(t *testing.T) {
		// Act
		output, err := runGo(moduleDir, "vet", "-vettool="+tool, "./clean")

		// Assert
		if err != nil {
			t.Fatalf("go vet ./clean = %v, want success\n%s", err, output)
		}
		if reported := reportedLines(output); len(reported) != 0 {
			t.Fatalf("reported lines = %q, want none\n%s", reported, output)
		}
	})
}

func buildVetTool(t *testing.T) string {
	t.Helper()
	tool := filepath.Join(t.TempDir(), "vetstringptr")
	if runtime.GOOS == "windows" {
		tool += ".exe"
	}
	if output, err := runGo(".", "build", "-o", tool, "."); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return tool
}

func runGo(dir string, args ...string) (string, error) {
	command := exec.Command("go", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return output.String(), err
}

// flaggedLines returns "file:line" for every fixture line carrying the
// marker, sorted.
func flaggedLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	for name, content := range fixtureModule {
		scanner := bufio.NewScanner(strings.NewReader(content))
		for number := 1; scanner.Scan(); number++ {
			if strings.HasSuffix(scanner.Text(), FLAGGED_MARKER) {
				lines = append(lines, filepath.Base(name)+":"+strconv.Itoa(number))
			}
		}
	}
	if len(lines) == 0 {
		t.Fatal("the fixture marks no line")
	}
	slices.Sort(lines)
	return lines
}

// reportedLines returns "file:line" for every diagnostic go vet printed,
// sorted.
func reportedLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if match := diagnosticLine.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			lines = append(lines, filepath.Base(match[1])+":"+match[2])
		}
	}
	slices.Sort(lines)
	return lines
}
