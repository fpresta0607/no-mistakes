package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestIPCFixturesBindBeforeTheirFirstDial keeps fake-daemon fixtures from
// starting srv.Serve in a goroutine: Serve binds the endpoint inside that
// goroutine, so a dial right after it races the bind (slow on a loaded Windows
// runner) and needs a readiness deadline. srv.Listen binds synchronously.
func TestIPCFixturesBindBeforeTheirFirstDial(t *testing.T) {
	// Arrange
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test files found in the package directory")
	}
	fileSet := token.NewFileSet()

	for _, file := range files {
		parsed, err := parser.ParseFile(fileSet, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		// Act
		ast.Inspect(parsed, func(node ast.Node) bool {
			goStatement, isGoStatement := node.(*ast.GoStmt)
			if !isGoStatement {
				return true
			}
			ast.Inspect(goStatement, func(inner ast.Node) bool {
				call, isCall := inner.(*ast.CallExpr)
				if !isCall {
					return true
				}
				selector, isSelector := call.Fun.(*ast.SelectorExpr)
				if isSelector && selector.Sel.Name == "Serve" {
					// Assert
					t.Errorf("%s: Serve runs in a goroutine, so the first dial races the bind; bind with startIPCServer (srv.Listen) before dialing", fileSet.Position(call.Pos()))
				}
				return true
			})
			return false
		})
	}
}
