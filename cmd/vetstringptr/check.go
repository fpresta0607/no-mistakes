package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// PRINT_FUNCTION_NAMES are the fmt, log and testing calls that format their
// variadic arguments without a format parameter. A call that has one is
// recognized by that parameter's name instead, which also covers this
// repository's own format-taking helpers.
var PRINT_FUNCTION_NAMES = map[string]bool{
	"Append": true, "Appendln": true,
	"Error": true,
	"Fatal": true, "Fatalln": true,
	"Fprint": true, "Fprintln": true,
	"Log":   true,
	"Panic": true, "Panicln": true,
	"Print": true, "Println": true,
	"Skip":   true,
	"Sprint": true, "Sprintln": true,
}

// findFormattedStringPointers returns the position of every *string passed
// as a formatted argument in files, which info must describe.
func findFormattedStringPointers(files []*ast.File, info *types.Info) []token.Pos {
	var positions []token.Pos
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			formattedFrom, ok := formattedArgumentsStart(call, info)
			if !ok {
				return true
			}
			for _, argument := range call.Args[formattedFrom:] {
				if isStringPointer(info.TypeOf(argument)) {
					positions = append(positions, argument.Pos())
				}
			}
			return true
		})
	}
	return positions
}

// formattedArgumentsStart reports whether call formats its trailing ...any
// arguments, and the index of the first of them.
func formattedArgumentsStart(call *ast.CallExpr, info *types.Info) (int, bool) {
	var name *ast.Ident
	switch callee := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		name = callee
	case *ast.SelectorExpr:
		name = callee.Sel
	default:
		return 0, false
	}
	function, ok := info.Uses[name].(*types.Func)
	if !ok {
		return 0, false
	}
	signature := function.Signature()
	parameters := signature.Params()
	if !signature.Variadic() {
		return 0, false
	}
	last := parameters.Len() - 1
	slice, ok := parameters.At(last).Type().(*types.Slice)
	if !ok {
		return 0, false
	}
	if element, ok := slice.Elem().Underlying().(*types.Interface); !ok || !element.Empty() {
		return 0, false
	}
	isScan := strings.Contains(strings.ToLower(function.Name()), "scan")
	hasFormatParameter := last > 0 && parameters.At(last-1).Name() == "format" && !isScan
	if !hasFormatParameter && !PRINT_FUNCTION_NAMES[function.Name()] {
		return 0, false
	}
	return last, true
}

func isStringPointer(argumentType types.Type) bool {
	if argumentType == nil {
		return false
	}
	pointer, ok := argumentType.Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	basic, ok := pointer.Elem().Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}
