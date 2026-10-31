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
// or method name, with the position of that argument. The checker adds to
// them every function that passes one of its own parameters on to one.
var reasonArgs = map[string]int{
	"setFeedCondition": 3, // (fs, condType, status, reason, message, generation)
	"setCondition":     2, // feedAttempt: (condType, status, reason, message)
	"warn":             0, // feedAttempt: (reason, action, format, args...)
	"Eventf":           3, // events.EventRecorder: (regarding, related, type, reason, action, note, args...)
}

// reasonFields are the struct fields that hold a condition reason.
var reasonFields = []string{"Reason", "conditionReason"}

// reasonChecker finds string literals that reach a condition or Event reason
// anywhere in a package. A reason sink -- a reasonFields field (keyed in a
// literal, assigned after construction, or the reason element of an unkeyed
// failureClass literal) or a reason argument -- is traced back through the
// package: local variables to everything assigned to them, whatever they are
// called; package-level variables to their initializers; calls to the
// package's functions to what those return (by result position); and a
// function's own parameters to every caller's argument, by turning that
// parameter into a sink. Struct fields are not traced through, since every
// write to one is itself a sink. Named constants pass, so a reason's value is
// always one of the declarations in conditions.go and classify.go, which are
// user-visible API. Two name-based checks back it up: a variable named for a
// reason must not be given a literal, and neither may a function named for
// one return it.
type reasonChecker struct {
	fset     *token.FileSet
	files    []*ast.File
	funcs    map[string][]*ast.FuncDecl // by function or method name
	globals  map[string]ast.Expr        // package-level var initializers
	sinks    map[string]map[int]bool    // reason parameters, by callee name
	traced   map[string]bool            // what trace has already followed
	problems map[string]bool
}

// checkReasonLiterals runs a reasonChecker over the files of one package and
// returns its findings, sorted.
func checkReasonLiterals(fset *token.FileSet, files []*ast.File) []string {
	c := &reasonChecker{
		fset: fset, files: files,
		funcs: map[string][]*ast.FuncDecl{}, globals: map[string]ast.Expr{},
		sinks: map[string]map[int]bool{}, traced: map[string]bool{}, problems: map[string]bool{},
	}
	for name, i := range reasonArgs {
		c.sinks[name] = map[int]bool{i: true}
	}
	c.index()
	// A parameter found to feed a sink makes a new sink, whose callers must
	// then be checked too: repeat until no new one appears.
	for before := -1; before != c.sinkCount(); {
		before = c.sinkCount()
		for _, file := range files {
			c.checkFile(file)
		}
	}
	return slices.Sorted(func(yield func(string) bool) {
		for p := range c.problems {
			if !yield(p) {
				return
			}
		}
	})
}

// index records the package's functions and package-level variables.
func (c *reasonChecker) index() {
	for _, file := range c.files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				c.funcs[d.Name.Name] = append(c.funcs[d.Name.Name], d)
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if i < len(vs.Values) {
							c.globals[id.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}
}

func (c *reasonChecker) sinkCount() int {
	n := 0
	for _, params := range c.sinks {
		n += len(params)
	}
	return n
}

// checkFile checks every reason sink in file, each in the function it is in
// (nil at package level).
func (c *reasonChecker) checkFile(file *ast.File) {
	for _, decl := range file.Decls {
		fn, _ := decl.(*ast.FuncDecl)
		if fn != nil {
			c.checkReasonResults(fn)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if gd, ok := n.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				return false // consts are where reasons are declared
			}
			c.checkNode(n, fn)
			return true
		})
	}
}

// checkNode checks n if it is a reason sink, inside fn.
func (c *reasonChecker) checkNode(n ast.Node, fn *ast.FuncDecl) {
	switch n := n.(type) {
	case *ast.KeyValueExpr:
		if key, ok := n.Key.(*ast.Ident); ok && slices.Contains(reasonFields, key.Name) {
			c.trace(n.Value, fn, key.Name)
		}
	case *ast.CompositeLit:
		if isFailureClass(n.Type) {
			c.checkFailureClassLit(n, fn)
		}
		if elem := elemType(n.Type); isFailureClass(elem) {
			// []failureClass{{metric, reason, permanent}}: the inner
			// literals elide their type.
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					elt = kv.Value
				}
				if inner, ok := elt.(*ast.CompositeLit); ok && inner.Type == nil {
					c.checkFailureClassLit(inner, fn)
				}
			}
		}
	case *ast.CallExpr:
		name := calleeName(n.Fun)
		for i := range c.sinks[name] {
			if i < len(n.Args) {
				c.trace(n.Args[i], fn, "the reason argument of "+name)
			}
		}
	case *ast.AssignStmt:
		c.checkAssignment(n, fn)
	case *ast.ValueSpec:
		for i, id := range n.Names {
			if namesAReason(id.Name) && i < len(n.Values) {
				c.trace(n.Values[i], fn, id.Name)
			}
		}
	}
}

// checkFailureClassLit checks the reason element of an unkeyed failureClass
// literal (keyed ones are reasonFields).
func (c *reasonChecker) checkFailureClassLit(lit *ast.CompositeLit, fn *ast.FuncDecl) {
	if len(lit.Elts) > 1 {
		if _, keyed := lit.Elts[1].(*ast.KeyValueExpr); !keyed {
			c.trace(lit.Elts[1], fn, "failureClass.conditionReason")
		}
	}
}

// isFailureClass reports whether typ names the failureClass type.
func isFailureClass(typ ast.Expr) bool {
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == "failureClass"
}

// elemType is the element type of a slice, array or map type expression, or
// nil.
func elemType(typ ast.Expr) ast.Expr {
	switch t := typ.(type) {
	case *ast.ArrayType:
		return t.Elt
	case *ast.MapType:
		return t.Value
	}
	return nil
}

// checkAssignment checks a value assigned to a reason-named variable or to a
// reasonFields field after construction (cond.Reason = ...).
func (c *reasonChecker) checkAssignment(n *ast.AssignStmt, fn *ast.FuncDecl) {
	if len(n.Lhs) != len(n.Rhs) {
		return
	}
	for i, lhs := range n.Lhs {
		switch dst := lhs.(type) {
		case *ast.Ident:
			if namesAReason(dst.Name) {
				c.trace(n.Rhs[i], fn, dst.Name)
			}
		case *ast.SelectorExpr:
			if slices.Contains(reasonFields, dst.Sel.Name) {
				c.trace(n.Rhs[i], fn, dst.Sel.Name)
			}
		}
	}
}

// checkReasonResults checks what a function named for a reason
// (dominantErrorReason) returns.
func (c *reasonChecker) checkReasonResults(fn *ast.FuncDecl) {
	if namesAReason(fn.Name.Name) && fn.Body != nil {
		c.traceResults(fn, 0, "a result of "+fn.Name.Name)
	}
}

// trace reports a non-empty string literal in expr, which reaches a reason
// sink (described by where) from inside fn, and follows the identifiers and
// local calls expr is made of back to their values.
func (c *reasonChecker) trace(expr ast.Expr, fn *ast.FuncDecl, where string) {
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit, *ast.SelectorExpr:
			// A closure's body runs elsewhere; a field read is covered where
			// the field is written.
			return false
		case *ast.IndexExpr:
			// m["key"]: what is looked up is not the value.
			c.trace(n.X, fn, where)
			return false
		case *ast.KeyValueExpr:
			// map[string]string{"key": reasonX}: only the value can arrive.
			c.trace(n.Value, fn, where)
			return false
		case *ast.BasicLit:
			if n.Kind == token.STRING && n.Value != `""` {
				c.problems[fmt.Sprintf("%s: string literal %s reaches %s", c.fset.Position(n.Pos()), n.Value, where)] = true
			}
		case *ast.Ident:
			c.traceIdent(n.Name, fn, where)
		case *ast.CallExpr:
			for _, arg := range n.Args {
				c.trace(arg, fn, where)
			}
			if id, ok := n.Fun.(*ast.Ident); ok {
				for _, g := range c.funcs[id.Name] {
					c.traceResults(g, 0, where)
				}
			}
			return false
		}
		return true
	})
}

// traceIdent follows name, used inside fn, to its values: fn's parameter (which
// becomes a sink), a variable assigned in fn, or a package-level variable.
// Constants and anything else (types, builtins) need nothing.
func (c *reasonChecker) traceIdent(name string, fn *ast.FuncDecl, where string) {
	key := fmt.Sprintf("%p/%s", fn, name)
	if c.traced[key] {
		return
	}
	c.traced[key] = true
	if fn != nil {
		if i, ok := paramIndex(fn, name); ok {
			if c.sinks[fn.Name.Name] == nil {
				c.sinks[fn.Name.Name] = map[int]bool{}
			}
			c.sinks[fn.Name.Name][i] = true
			return
		}
		if fn.Body != nil && c.traceLocal(name, fn, where) {
			return
		}
	}
	if value, ok := c.globals[name]; ok {
		c.trace(value, nil, where)
	}
}

// traceLocal traces every value assigned to the local variable name in fn,
// and reports whether name is one.
func (c *reasonChecker) traceLocal(name string, fn *ast.FuncDecl, where string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					found = true
					c.traceAssigned(n.Rhs, i, len(n.Lhs), fn, where)
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Name == name {
					found = true
					c.traceAssigned(n.Values, i, len(n.Names), fn, where)
				}
			}
		}
		return true
	})
	return found
}

// traceAssigned traces the value the i-th of n destinations gets from rhs:
// rhs[i], or the i-th result of a single call (r, m := pair()).
func (c *reasonChecker) traceAssigned(rhs []ast.Expr, i, n int, fn *ast.FuncDecl, where string) {
	switch {
	case len(rhs) == n:
		c.trace(rhs[i], fn, where)
	case len(rhs) == 1:
		if call, ok := rhs[0].(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				for _, g := range c.funcs[id.Name] {
					c.traceResults(g, i, where)
				}
			}
		}
	}
}

// traceResults traces the result at position i of every return in fn.
func (c *reasonChecker) traceResults(fn *ast.FuncDecl, i int, where string) {
	key := fmt.Sprintf("%p/result%d", fn, i)
	if c.traced[key] || fn.Body == nil {
		return
	}
	c.traced[key] = true
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if i < len(n.Results) {
				c.trace(n.Results[i], fn, where)
			}
		}
		return true
	})
}

// paramIndex is the position of the parameter name in fn's signature (the
// receiver aside), as the call's argument index.
func paramIndex(fn *ast.FuncDecl, name string) (int, bool) {
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, id := range field.Names {
			if id.Name == name {
				return i, true
			}
			i++
		}
	}
	return 0, false
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

// namesAReason reports whether a variable's or function's name says it holds
// or computes a reason (reason, eventReason, dominantErrorReason, ...).
func namesAReason(name string) bool {
	return strings.Contains(strings.ToLower(name), "reason")
}

// TestProductionReasonsAreNamedConstants enforces CLAUDE.md's "every reason
// string in production code is a const": condition and Event reasons are
// user-visible API (kubectl, Events, dashboards, alerts), so each must be one
// of the named declarations, never a literal that reaches a reason, however
// indirectly.
func TestProductionReasonsAreNamedConstants(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no production files were checked")
	}
	if problems := checkReasonLiterals(fset, files); len(problems) > 0 {
		t.Fatalf("condition/Event reasons must be named constants (conditions.go, classify.go):\n%s",
			strings.Join(problems, "\n"))
	}
}

// TestCheckReasonLiterals_CatchesEverySink keeps the guard above from passing
// vacuously: each sink and each path it traces is flagged when it carries a
// literal, and the constant forms production uses are not.
func TestCheckReasonLiterals_CatchesEverySink(t *testing.T) {
	// planted is the literal (as source text) most cases plant at a sink.
	const planted = `"Oops"`
	for _, tc := range []struct {
		name, src string
		// want is the literal the checker must report ("" for none).
		want string
	}{
		{name: "Reason field", src: `func f() { _ = metav1.Condition{Reason: "Oops"} }`, want: planted},
		{name: "conditionReason field", src: `func f() { _ = failureClass{conditionReason: "Oops"} }`, want: planted},
		{name: "unkeyed failureClass", src: `var c = failureClass{metricOther, "Oops", false}`, want: planted},
		{name: "setFeedCondition", src: `func f() { setFeedCondition(fs, t, s, "Oops", m, 1) }`, want: planted},
		{name: "setCondition", src: `func f() { a.setCondition(t, s, "Oops", m) }`, want: planted},
		{name: "warn", src: `func f() { a.warn("Oops", action, "f") }`, want: planted},
		{name: "Eventf", src: `func f() { r.Eventf(g, nil, typ, "Oops", action, "f") }`, want: planted},
		{name: "concatenated literal", src: `func f() { _ = metav1.Condition{Reason: reasonOther + "2"} }`, want: `"2"`},
		{name: "Reason field assigned", src: `func f() { cond.Reason = "Oops" }`, want: planted},
		{name: "conditionReason field assigned", src: `func f() { class.conditionReason = "Oops" }`, want: planted},
		{name: "reason variable", src: `func f() { reason := "Oops"; _ = reason }`, want: planted},
		{name: "reason var declaration", src: "func f() {\nvar eventReason = \"Oops\"\n_ = eventReason\n}", want: planted},
		{name: "reason helper result", src: `func dominantReason() string { return "Oops" }`, want: planted},
		{name: "a local constant used as a reason", src: "func f() {\nconst x = \"Oops\"\na.warn(x, action, \"f\")\n}", want: planted},

		// Indirect paths, through names that say nothing about reasons.
		{name: "through a local variable", src: `func f() { x := "Oops"; a.warn(x, action, "f") }`, want: planted},
		{name: "through a reassigned local", src: `func f() { x := reasonOther; if c { x = "Oops" }; a.warn(x, action, "f") }`, want: planted},
		{name: "through a helper's result", src: `func pick() string { return "Oops" }
func f() { a.warn(pick(), action, "f") }`, want: planted},
		{name: "through one of a helper's results", src: `func pair() (string, string) { return "Oops", reasonOther }
func f() { r, m := pair(); a.warn(r, action, m) }`, want: planted},
		{name: "through a package variable", src: `var bad = "Oops"
func f() { a.warn(bad, action, "f") }`, want: planted},
		{name: "through a parameter", src: `func h(r string) { a.warn(r, action, "f") }
func f() { h("Oops") }`, want: planted},
		{name: "through two parameters", src: `func h(r string) { g(r) }
func g(r string) { a.warn(r, action, "f") }
func f() { h("Oops") }`, want: planted},
		{name: "through a method parameter", src: `func (a feedAttempt) h(x int, r string) { a.warn(r, action, "f") }
func f() { a.h(1, "Oops") }`, want: planted},

		{name: "an elided failureClass in a slice", src: "var cs = []failureClass{{metricOther, \"Oops\", false}}", want: planted},
		{name: "an elided failureClass in a map", src: "var cs = map[string]failureClass{\"k\": {metricOther, \"Oops\", false}}", want: planted},
		{name: "through a map value", src: `func f() { m := map[string]string{"k": "Oops"}; a.warn(m["k"], action, "f") }`, want: planted},

		// What production does, which must pass.
		{name: "named constants", src: `func f() { reason := reasonReconciled; reason = reasonFeedErrors; _ = metav1.Condition{Reason: reason}; a.warn(reasonConfigError, action, "feed %s", u) }`},
		{name: "a helper's other result", src: `func pair() (string, string) { return reasonOther, "message" }
func f() { r, m := pair(); a.warn(r, action, m) }`},
		{name: "a parameter given constants", src: `func h(r string) { a.warn(r, action, "f") }
func f() { h(reasonOther) }`},
		{name: "a field read", src: `func f() { a.setCondition(t, s, class.conditionReason, err.Error()) }`},
		{name: "empty zero value", src: `func f() { prevReason, prevMessage := "", ""; _, _ = prevReason, prevMessage }`},
		{name: "a message beside the reason", src: `func f() { msg := "message"; a.setCondition(t, s, reasonOther, msg) }`},
		{name: "Reason field assigned a constant", src: `func f() { cond.Reason = reasonOther }`},
		{name: "reason helper returning a constant", src: `func dominantReason() string { return reasonOther }`},
		{name: "a map key and index", src: `func f() { m := map[string]string{"k": reasonOther}; a.warn(m["k"], action, "f") }`},
		{name: "elided failureClass constants", src: "var cs = []failureClass{{metricOther, reasonOther, false}}"},
		{name: "reason constants declared", src: "const reasonOops = \"Oops\"\nfunc f() {\nconst reason = reasonOops\na.warn(reason, action, \"f\")\n}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "sink.go", "package controller\n"+tc.src+"\n", 0)
			if err != nil {
				t.Fatal(err)
			}
			problems := checkReasonLiterals(fset, []*ast.File{file})
			if tc.want == "" {
				if len(problems) > 0 {
					t.Fatalf("flagged %v, want nothing for:\n%s", problems, tc.src)
				}
				return
			}
			// Each problem names its literal; the one the case plants must be
			// among them, not just some other literal in the snippet.
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "literal "+tc.want+" ") }) {
				t.Fatalf("problems %v do not report %s for:\n%s", problems, tc.want, tc.src)
			}
		})
	}
}
