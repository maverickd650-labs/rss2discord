/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// reasonArgs are the calls that take a condition or Event reason, by function
// or method name, with the position of that argument.
var reasonArgs = map[string]int{
	"setFeedCondition": 3, // (fs, condType, status, reason, message, generation)
	"setCondition":     2, // feedAttempt: (condType, status, reason, message)
	"warn":             0, // feedAttempt: (reason, action, format, args...)
	"Eventf":           3, // events.EventRecorder: (regarding, related, type, reason, action, note, args...)
}

// reasonFields are the struct fields that hold a condition reason.
var reasonFields = []string{"Reason", "conditionReason"}

// checkReasonLiterals reports every place in file where a string literal
// reaches a condition or Event reason: a reasonFields field (keyed, assigned
// after construction, or the second element of an unkeyed failureClass
// literal), a reasonArgs argument, a variable whose name says it holds a
// reason, or a result of a function whose name does. It is syntactic, not a
// data-flow analysis: a literal laundered through a variable or helper not
// named for a reason gets past it, which naming review is left to catch. Named constants -- and
// variables, parameters and fields that only ever carry them -- pass, so a
// reason's value is always one of the declarations in conditions.go and
// classify.go, which are user-visible API.
func checkReasonLiterals(fset *token.FileSet, file *ast.File) []string {
	var problems []string
	flag := func(expr ast.Expr, where string) {
		if lit := stringLiteralIn(expr); lit != nil {
			problems = append(problems, fmt.Sprintf("%s: string literal %s as %s", fset.Position(lit.Pos()), lit.Value, where))
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok && slices.Contains(reasonFields, key.Name) {
				flag(n.Value, key.Name)
			}
		case *ast.CompositeLit:
			if typ, ok := n.Type.(*ast.Ident); ok && typ.Name == "failureClass" && len(n.Elts) > 1 {
				if _, keyed := n.Elts[1].(*ast.KeyValueExpr); !keyed {
					flag(n.Elts[1], "failureClass.conditionReason")
				}
			}
		case *ast.CallExpr:
			name := calleeName(n.Fun)
			if i, ok := reasonArgs[name]; ok && i < len(n.Args) {
				flag(n.Args[i], "the reason argument of "+name)
			}
		case *ast.AssignStmt:
			checkReasonAssignment(n, flag)
		case *ast.FuncDecl:
			checkReasonResults(n, flag)
		case *ast.GenDecl:
			if n.Tok != token.VAR {
				return true // consts are where reasons are declared
			}
			for _, spec := range n.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if namesAReason(id.Name) && i < len(vs.Values) {
						flag(vs.Values[i], id.Name)
					}
				}
			}
		}
		return true
	})
	return problems
}

// checkReasonAssignment flags, through flag, a literal assigned to a
// reason-named variable or to a reasonFields field after construction
// (cond.Reason = ...).
func checkReasonAssignment(n *ast.AssignStmt, flag func(ast.Expr, string)) {
	for i, lhs := range n.Lhs {
		if i >= len(n.Rhs) {
			return
		}
		switch dst := lhs.(type) {
		case *ast.Ident:
			if namesAReason(dst.Name) {
				flag(n.Rhs[i], dst.Name)
			}
		case *ast.SelectorExpr:
			if slices.Contains(reasonFields, dst.Sel.Name) {
				flag(n.Rhs[i], dst.Sel.Name)
			}
		}
	}
}

// checkReasonResults flags, through flag, a literal returned by a function
// whose name says it computes a reason (dominantErrorReason): it must return
// one of the constants too.
func checkReasonResults(fn *ast.FuncDecl, flag func(ast.Expr, string)) {
	if !namesAReason(fn.Name.Name) || fn.Body == nil {
		return
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok {
			for _, result := range ret.Results {
				flag(result, "a result of "+fn.Name.Name)
			}
		}
		return true
	})
}

// calleeName is the function or method name a call expression calls.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// namesAReason reports whether a variable's name says it holds a reason
// (reason, eventReason, prevReason, ...).
func namesAReason(name string) bool {
	return strings.Contains(strings.ToLower(name), "reason")
}

// stringLiteralIn returns the first non-empty string literal in expr, outside
// any function literal, or nil. The empty string is a zero value -- "no reason
// yet", as summarizeGroup starts the previous Ready reason -- not a reason.
func stringLiteralIn(expr ast.Expr) *ast.BasicLit {
	var found *ast.BasicLit
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BasicLit:
			if n.Kind == token.STRING && n.Value != `""` && found == nil {
				found = n
			}
		}
		return found == nil
	})
	return found
}

// TestProductionReasonsAreNamedConstants enforces CLAUDE.md's "every reason
// string in production code is a const": condition and Event reasons are
// user-visible API (kubectl, Events, dashboards, alerts), so each must be one
// of the named declarations, never a literal at the use site.
func TestProductionReasonsAreNamedConstants(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var problems []string
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		problems = append(problems, checkReasonLiterals(fset, file)...)
	}
	if checked == 0 {
		t.Fatal("no production files were checked")
	}
	if len(problems) > 0 {
		t.Fatalf("condition/Event reasons must be named constants (conditions.go, classify.go):\n%s",
			strings.Join(problems, "\n"))
	}
}

// TestCheckReasonLiterals_CatchesEverySink keeps the guard above from passing
// vacuously: each sink it covers is flagged when given a literal, and the
// constant forms production uses are not.
func TestCheckReasonLiterals_CatchesEverySink(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		flagged    bool
	}{
		{name: "Reason field", body: `_ = metav1.Condition{Reason: "Oops"}`, flagged: true},
		{name: "conditionReason field", body: `_ = failureClass{conditionReason: "Oops"}`, flagged: true},
		{name: "unkeyed failureClass", body: `_ = failureClass{metricOther, "Oops", false}`, flagged: true},
		{name: "setFeedCondition", body: `setFeedCondition(fs, t, s, "Oops", m, 1)`, flagged: true},
		{name: "setCondition", body: `a.setCondition(t, s, "Oops", m)`, flagged: true},
		{name: "warn", body: `a.warn("Oops", action, "f")`, flagged: true},
		{name: "Eventf", body: `r.Eventf(g, nil, typ, "Oops", action, "f")`, flagged: true},
		{name: "reason variable", body: `reason := "Oops"; _ = reason`, flagged: true},
		{name: "concatenated literal", body: `_ = metav1.Condition{Reason: reasonOther + "2"}`, flagged: true},
		{name: "reason var declaration", body: "var eventReason = \"Oops\"\n_ = eventReason", flagged: true},
		{name: "Reason field assigned", body: `cond.Reason = "Oops"`, flagged: true},
		{name: "conditionReason field assigned", body: `class.conditionReason = "Oops"`, flagged: true},
		{name: "reason helper result", body: "_ = 0\n}\nfunc dominantReason() string {\nreturn \"Oops\"", flagged: true},
		{name: "reason helper returning a constant", body: "_ = 0\n}\nfunc dominantReason() string {\nreturn reasonOther"},
		{name: "Reason field assigned a constant", body: `cond.Reason = reasonOther`},
		{name: "named constants", body: `reason := reasonReconciled; reason = reasonFeedErrors; _ = metav1.Condition{Reason: reason}; a.warn(reasonConfigError, action, "feed %s", u)`},
		{name: "empty zero value", body: `prevReason, prevMessage := "", ""; _, _ = prevReason, prevMessage`},
		{name: "failure class field", body: `a.setCondition(t, s, class.conditionReason, err.Error())`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "sink.go", "package controller\nfunc f() {\n"+tc.body+"\n}\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(checkReasonLiterals(fset, file)) > 0; got != tc.flagged {
				t.Fatalf("flagged = %v, want %v for %s", got, tc.flagged, tc.body)
			}
		})
	}
}
