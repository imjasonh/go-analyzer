// Copyright 2022 The go-analyzer Authors
// SPDX-License-Identifier: BSD-3-Clause

package pkgerrors

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

const doc = `pkgerrors analyzer analyzes and rewrites the github.com/pkg/errors (that has been deprecated) to the fmt.Errorf with %%w verb provided after the go1.13.`

var Analyzer = &analysis.Analyzer{
	Name:     "pkgerrors",
	Doc:      doc,
	Run:      run,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

const pkgerrosPath = "github.com/pkg/errors"

func run(pass *analysis.Pass) (interface{}, error) {
	inspected := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	nodeFilter := []ast.Node{
		(*ast.CallExpr)(nil), // filter only function expression
	}
	inspected.Preorder(nodeFilter, func(node ast.Node) {
		call := node.(*ast.CallExpr)

		// checks call expression is error
		if !isError(pass.TypesInfo.TypeOf(call)) {
			return
		}

		// filtered only of pkg/errors call expressions
		fnName, ok := isPkgErrorsCall(pass.TypesInfo, call)
		if !ok {
			return
		}

		// make a copy of the function declaration to avoid mutating the AST.
		// See https://go.dev/issue/46129.
		callCopy := &ast.CallExpr{}
		*callCopy = *call
		callCopy.Args = call.Args

		switch fnName {
		case "Cause": // errors.Cause
			callCopy.Fun.(*ast.SelectorExpr).Sel.Name = "Unwrap"

		case "New": // errors.New
			// nothing to do
			return

		case "Errorf": // errors.Errorf
			callCopy.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name = "fmt"

		case "WithStack": // errors.WithStack
			// not supported
			return

		case "WithMessage", "WithMessagef", "Wrap", "Wrapf": // errors.WithMessage{f}, errors.Wrap{f}
			callCopy.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name = "fmt"
			callCopy.Fun.(*ast.SelectorExpr).Sel.Name = "Errorf"
			callCopy.Args = reorderArgs(call.Args)
		}

		var buf bytes.Buffer
		if err := format.Node(&buf, pass.Fset, callCopy); err != nil {
			return
		}

		pass.Report(analysis.Diagnostic{
			Pos:      call.Pos(),
			End:      call.End(),
			Category: "???", // TODO(zchee): what is category?
			Message:  fmt.Sprintf("found use location of the deprecated %s", pkgerrosPath),
			SuggestedFixes: []analysis.SuggestedFix{{
				Message: "Use fmt.Errorf with %%w verb instead",
				TextEdits: []analysis.TextEdit{{
					Pos:     callCopy.Pos(),
					End:     callCopy.End(),
					NewText: buf.Bytes(),
				}},
			}},
		})
	})

	return nil, nil
}

// isError reports whether the typ is an error type.
func isError(typ types.Type) bool {
	if typ == nil {
		return false
	}

	return typ.String() == "error" || typ.Underlying().String() == "error"
}

func isPkgErrorsCall(info *types.Info, call *ast.CallExpr) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		obj := info.ObjectOf(fn.Sel)
		return obj.Name(), isPkgErrorsFunc(obj)

	case *ast.Ident:
		if declExpr, ok := findExpr(fn).(*ast.SelectorExpr); ok {
			obj := info.ObjectOf(declExpr.Sel)
			return obj.Name(), isPkgErrorsFunc(obj)
		}
	}

	return "", false
}

func isPkgErrorsFunc(obj types.Object) bool {
	if obj.Pkg().Path() != pkgerrosPath {
		return false
	}

	switch obj.Name() {
	case
		"Cause",        // errors.Cause
		"New",          // errors.New
		"Errorf",       // errors.Errorf
		"WithMessage",  // errors.WithMessage
		"WithMessagef", // errors.WithMessagef
		"WithStack",    // errors.WithStack
		"Wrap",         // errors.Wrap
		"Wrapf":        // errors.Wrapf
		return true
	}

	return false
}

func findExpr(arg *ast.Ident) ast.Expr {
	if arg.Obj == nil {
		return nil
	}

	switch as := arg.Obj.Decl.(type) {
	case *ast.AssignStmt:
		if len(as.Lhs) != len(as.Rhs) {
			return nil
		}

		for i, lhs := range as.Lhs {
			lid, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if lid.Obj == arg.Obj {
				return as.Rhs[i]
			}
		}

	case *ast.ValueSpec:
		if len(as.Names) != len(as.Values) {
			return nil
		}

		for i, name := range as.Names {
			if name.Obj == arg.Obj {
				return as.Values[i]
			}
		}
	}

	return nil
}

// vendorlessPath returns the devendorized version of the import path ipath.
// For example, VendorlessPath("foo/bar/vendor/a/b") returns "a/b".
//
// This function copid from https://github.com/golang/tools/blob/v0.1.10/internal/imports/fix.go#L1423-L1432
func vendorlessPath(ipath string) string {
	// Devendorize for use in import statement.
	if i := strings.LastIndex(ipath, "/vendor/"); i >= 0 {
		return ipath[i+len("/vendor/"):]
	}

	if strings.HasPrefix(ipath, "vendor/") {
		return ipath[len("vendor/"):]
	}

	return ipath
}

// verb assumes unquoted msg.
func verb(msg string) string {
	if strings.HasSuffix(msg, "%w") {
		return msg
	}
	// fmt.Printf("verb: %s\n", msg)
	// if strings.HasSuffix(msg, "%v") {
	// 	return strings.ReplaceAll(msg, "%v", "%w")
	// }
	// if msg[len(msg)-3:] == `%v` {
	// 	fmt.Printf("verb: %s\n", msg[len(msg)-2:]+`%w`)
	// 	return msg[len(msg)-2:] + `%w`
	// }

	return msg + ": %w"
}

// unquote assumes quoted s.
func unquote(s string) string {
	return s[1:len(s)-1] + ": %w" // skip first and last char
}

// reorderArgs re-orders pkg/errors args to fmt.Errorf format.
func reorderArgs(exprs []ast.Expr) []ast.Expr {
	errStmt := exprs[0]
	msg := exprs[1]
	args := exprs[2:]

	// adds %w verb to the end of msg
	s := msg.(*ast.BasicLit).Value
	s = verb(unquote(s))
	msg.(*ast.BasicLit).Value = strconv.Quote(s) // re-quoted

	return append(append([]ast.Expr{msg}, args...), errStmt)
}
