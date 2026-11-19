// Command vetstringptr is a `go vet -vettool` that reports a *string handed
// to a call that formats its arguments, such as t.Fatalf or fmt.Sprintf.
// Formatting verbs print a pointer's address, `go vet` accepts that, and a
// run's or step's Error is a *string, so a failing test printed an address
// where its error text belonged (issues #92 and #229).
//
// Usage:
//
//	go build -o bin/vetstringptr ./cmd/vetstringptr
//	go vet -tags e2e -vettool=bin/vetstringptr ./...
//
// `make lint` runs exactly that. The tool speaks the go command's vet tool
// protocol with the standard library alone: go vet builds each package and
// hands this tool a JSON config naming its files and its dependencies' export
// data, so the check decides by type rather than by field name. In vet mode
// the go command passes -json and reads the diagnostics back from the file
// the config names, which it caches and prints as file:line:column lines.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"strings"
)

const ANALYZER_NAME = "stringptr"

// vetConfig holds the fields this tool reads from the config go vet writes
// for each package (cmd/go/internal/work.vetConfig).
type vetConfig struct {
	ID                        string
	Compiler                  string
	ImportPath                string
	GoFiles                   []string
	ImportMap                 map[string]string
	PackageFile               map[string]string
	GoVersion                 string
	VetxOnly                  bool
	VetxOutput                string
	Stdout                    string
	SucceedOnTypecheckFailure bool
}

// jsonDiagnostic is the diagnostic shape the go command parses from a vet
// tool's -json output.
type jsonDiagnostic struct {
	Posn    string `json:"posn"`
	End     string `json:"end"`
	Message string `json:"message"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run answers the go command's probes or checks the package one config
// names, and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "-V=full":
		return printVersion(stdout, stderr)
	case len(args) == 1 && args[0] == "-flags":
		fmt.Fprintln(stdout, "[]")
		return 0
	case len(args) == 2 && args[0] == "-json" && strings.HasSuffix(args[1], ".cfg"):
		if err := checkPackage(args[1]); err != nil {
			fmt.Fprintf(stderr, "vetstringptr: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "vetstringptr: unexpected arguments %q; run it as go vet -vettool=<path to vetstringptr> <packages>\n", args)
		return 2
	}
}

// printVersion prints the version line go vet caches results under. The
// executable's own hash is the build ID, so a rebuilt tool never reuses
// results an older one produced.
func printVersion(stdout, stderr io.Writer) int {
	executable, err := os.Executable()
	if err == nil {
		var content []byte
		if content, err = os.ReadFile(executable); err == nil {
			fmt.Fprintf(stdout, "vetstringptr version devel buildID=%x\n", sha256.Sum256(content))
			return 0
		}
	}
	fmt.Fprintf(stderr, "vetstringptr: %v\n", err)
	return 1
}

func checkPackage(configPath string) error {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var config vetConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	// go vet caches a package's result only when this file exists. The tool
	// records no facts, so a dependency analyzed only for facts is done.
	if err := os.WriteFile(config.VetxOutput, nil, 0o644); err != nil {
		return err
	}
	if config.VetxOnly {
		return nil
	}

	fileSet := token.NewFileSet()
	files := make([]*ast.File, 0, len(config.GoFiles))
	for _, path := range config.GoFiles {
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, file)
	}

	exportImporter := importer.ForCompiler(fileSet, config.Compiler, func(path string) (io.ReadCloser, error) {
		file, ok := config.PackageFile[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %q", path)
		}
		return os.Open(file)
	})
	typesConfig := types.Config{
		Importer: importerFunc(func(importPath string) (*types.Package, error) {
			path, ok := config.ImportMap[importPath]
			if !ok {
				return nil, fmt.Errorf("cannot resolve import %q", importPath)
			}
			return exportImporter.Import(path)
		}),
		Sizes:     types.SizesFor(config.Compiler, build.Default.GOARCH),
		GoVersion: config.GoVersion,
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	if _, err := typesConfig.Check(config.ImportPath, fileSet, files, info); err != nil {
		if config.SucceedOnTypecheckFailure {
			return nil
		}
		return fmt.Errorf("type-check %s: %w", config.ImportPath, err)
	}

	var diagnostics []jsonDiagnostic
	for _, position := range findFormattedStringPointers(files, info) {
		location := fileSet.Position(position).String()
		diagnostics = append(diagnostics, jsonDiagnostic{
			Posn:    location,
			End:     location,
			Message: "a *string is formatted, which prints its address; pass the string it points to",
		})
	}
	if len(diagnostics) == 0 {
		return nil
	}
	output, err := json.Marshal(map[string]map[string][]jsonDiagnostic{config.ID: {ANALYZER_NAME: diagnostics}})
	if err != nil {
		return err
	}
	return os.WriteFile(config.Stdout, output, 0o644)
}

type importerFunc func(path string) (*types.Package, error)

func (function importerFunc) Import(path string) (*types.Package, error) { return function(path) }
