package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

const ipcPackagePath = "github.com/kunchenguid/no-mistakes/internal/ipc"

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
	fileSet := token.NewFileSet()
	scannedFixtureFiles := 0

	for _, file := range files {
		parsed, err := parser.ParseFile(fileSet, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if !importsIPC(parsed) {
			continue
		}
		scannedFixtureFiles++

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
					t.Errorf("%s: bind the listener before Serve starts in a goroutine: call startIPCServer (srv.Listen, then go srv.ServeReady()) so the first dial never races the bind", fileSet.Position(call.Pos()))
				}
				return true
			})
			return false
		})
	}

	if scannedFixtureFiles == 0 {
		t.Fatalf("no test file in the package imports %s", ipcPackagePath)
	}
}

func importsIPC(file *ast.File) bool {
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err == nil && path == ipcPackagePath {
			return true
		}
	}
	return false
}
