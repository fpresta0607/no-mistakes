package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestErrorPointersReachCallsOnlyThroughDeref keeps a run's or step's Error,
// a *string, out of every call but deref. A formatting verb prints the
// pointer's address for it, vet accepts that, and the failure message is
// usually the only evidence left once cleanup removes the temporary daemon and
// checkout (issue #92). This file carries no build tag so plain `go test ./...`
// runs the check, and the parser reads the tagged files regardless.
func TestErrorPointersReachCallsOnlyThroughDeref(t *testing.T) {
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 2 {
		t.Fatalf("found %d test files, want the e2e suite beside this one", len(paths))
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if callee, ok := call.Fun.(*ast.Ident); ok && callee.Name == "deref" {
				return true
			}
			for _, argument := range call.Args {
				if selector, ok := argument.(*ast.SelectorExpr); ok && selector.Sel.Name == "Error" {
					t.Errorf("%s: an Error pointer is passed to a call bare, so a failure prints its address; wrap it in deref", fset.Position(argument.Pos()))
				}
			}
			return true
		})
	}
}
