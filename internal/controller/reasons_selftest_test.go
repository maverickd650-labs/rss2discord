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
	"go/types"
	"slices"
	"strings"
	"testing"
)

// reasonPrelude declares what the self-test's snippets use: the sinks, a
// condition type and some constants, as the production package has them.
const reasonPrelude = `package controller

import (
	"ext"
	metav1 "meta"
)

type failureClass struct {
	metricReason, conditionReason string
	permanent                     bool
}
type feedAttempt struct{}

func (feedAttempt) setCondition(condType, status, reason, message string) {}
func (feedAttempt) warn(reason, action, format string, args ...any)      {}
// setFeedCondition stands in for the production helper, whose trailing message
// and generation arguments follow the reason and do not matter to the guard.
func setFeedCondition(fs any, condType, status, reason string) {}

type recorder interface {
	Eventf(regarding, related any, eventtype, reason, action, note string, args ...any)
}

const (
	reasonOther       = "Other"
	reasonReconciled  = "Recon" + "ciled"
	reasonFeedErrors  = "FeedErrors"
	reasonConfigError = "ConfigError"
	reasonOops        = "Oops2"
	metricOther       = "other"
)

var (
	a                       feedAttempt
	r                       recorder
	fs, g                   any
	t, s, m, action, u, typ string
	c                       bool
	cond                    metav1.Condition
	class                   failureClass
	err                     error
	_                       = ext.Upper
)
`

// snippetHeader starts each snippet's file, with the prelude's imports.
const snippetHeader = "package controller\n\nimport (\n\t\"ext\"\n\tmetav1 \"meta\"\n)\n\nvar (\n\t_ metav1.Condition\n\t_ = ext.Upper\n)\n\n"

// fakeImports are the packages the prelude imports: a condition type, and a
// function of another package.
var fakeImports = map[string]string{
	"meta": "package meta\ntype Condition struct {\n\tType, Status, Reason, Message string\n\tObservedGeneration int64\n}\n",
	"ext":  "package ext\nfunc Upper(s string) string { return s }\nfunc Split(s string) (string, int) { return s, 0 }\nfunc Count(s string) int { return len(s) }\n",
}

type fakeImporter map[string]*types.Package

func (imp fakeImporter) Import(path string) (*types.Package, error) {
	if p, ok := imp[path]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no package %q", path)
}

// checkSnippet type-checks src beside reasonPrelude and runs checkReasons
// over the two.
func checkSnippet(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	imp := fakeImporter{}
	for path, code := range fakeImports {
		file, err := parser.ParseFile(fset, path+".go", code, 0)
		if err != nil {
			t.Fatal(err)
		}
		if imp[path], err = (&types.Config{}).Check(path, fset, []*ast.File{file}, nil); err != nil {
			t.Fatal(err)
		}
	}
	files := make([]*ast.File, 0, 2)
	for _, f := range []struct{ name, code string }{{"prelude.go", reasonPrelude}, {"sink.go", snippetHeader + src + "\n"}} {
		file, err := parser.ParseFile(fset, f.name, f.code, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Implicits: map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{}, Scopes: map[ast.Node]*types.Scope{},
		Instances: map[*ast.Ident]types.Instance{}, FileVersions: map[*ast.File]string{},
	}
	pkg, err := (&types.Config{Importer: imp}).Check("controller", fset, files, info)
	if err != nil {
		t.Fatalf("snippet does not type-check: %v\n%s", err, src)
	}
	return checkReasons(fset, buildSSA(fset, pkg, files, info), info, files)
}

// planted is the string (quoted) most cases plant at a sink.
const (
	planted        = `"Oops"`
	wantReconciled = `"Reconciled"`
)

// sinkCase is one snippet checkSnippet is run over.
type sinkCase struct {
	name, src string
	// want is the quoted string a problem must name ("" for none).
	want string
}

// directSinkCases plant a literal at each sink, or spell a named one.
var directSinkCases = []sinkCase{
	{name: "Reason field", src: `func f() { _ = metav1.Condition{Reason: "Oops"} }`, want: planted},
	{name: "unkeyed Condition", src: `func f() { _ = metav1.Condition{t, s, "Oops", m, 1} }`, want: planted},
	{name: "conditionReason field", src: `func f() { _ = failureClass{conditionReason: "Oops"} }`, want: planted},
	{name: "unkeyed failureClass", src: `var cs = failureClass{metricOther, "Oops", false}`, want: planted},
	{name: "setFeedCondition", src: `func f() { setFeedCondition(fs, t, s, "Oops") }`, want: planted},
	{name: "setCondition", src: `func f() { a.setCondition(t, s, "Oops", m) }`, want: planted},
	{name: "warn", src: `func f() { a.warn("Oops", action, "f") }`, want: planted},
	{name: "Eventf", src: `func f() { r.Eventf(g, nil, typ, "Oops", action, "f") }`, want: planted},
	{name: "concatenated constant", src: `func f() { _ = metav1.Condition{Reason: reasonOther + "2"} }`, want: `"Other2"`},
	{name: "concatenated at run time", src: `func f() { _ = metav1.Condition{Reason: u + "Oops"} }`, want: planted},
	{name: "Reason field assigned", src: `func f() { cond.Reason = "Oops" }`, want: planted},
	{name: "conditionReason field assigned", src: `func f() { class.conditionReason = "Oops" }`, want: planted},
	{name: "reason helper result", src: `func dominantReason() string { return "Oops" }`, want: planted},
	{name: "a local constant", src: "func f() {\nconst x = \"Oops\"\na.warn(x, action, \"f\")\n}", want: planted},
	{name: "a constant's value spelled out", src: `func f() { a.warn("Other", action, "f") }`, want: `"Other"`},
	{name: "split canonical reason spelling", src: `func f() { a.warn("Recon" + "ciled", action, "f") }`, want: wantReconciled},
	{name: "split canonical reason constant outside declaration", src: `const splitReason = "Recon" + "ciled"
func f() { a.warn(splitReason, action, "f") }`, want: wantReconciled},
	{name: "through another package", src: `func f() { a.warn(ext.Upper("Oops"), action, "f") }`, want: planted},
}

// indirectSinkCases reach a sink through names that say nothing about reasons.
var indirectSinkCases = []sinkCase{
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
	{name: "through two parameters", src: `func h(r string) { k(r) }
func k(r string) { a.warn(r, action, "f") }
func f() { h("Oops") }`, want: planted},
	{name: "through a method parameter", src: `func (a feedAttempt) h(x int, r string) { a.warn(r, action, "f") }
func f() { a.h(1, "Oops") }`, want: planted},
	{name: "an elided failureClass in a slice", src: "var cs = []failureClass{{metricOther, \"Oops\", false}}", want: planted},
	{name: "an elided failureClass in a map", src: "var cs = map[string]failureClass{\"k\": {metricOther, \"Oops\", false}}", want: planted},
	{name: "through a map value", src: `func f() { m := map[string]string{"k": "Oops"}; a.warn(m["k"], action, "f") }`, want: planted},
	{name: "through a slice element", src: `func f() { xs := []string{reasonOther}; xs[0] = "Oops"; a.warn(xs[0], action, "f") }`, want: planted},
}

// syntacticMissCases are paths a syntactic trace missed.
var syntacticMissCases = []sinkCase{
	{name: "through a closure's result", src: `func f() { pick := func() string { return "Oops" }; a.warn(pick(), action, "f") }`, want: planted},
	{name: "through a closure's write", src: `func f() { x := reasonOther; set := func() { x = "Oops" }; set(); a.warn(x, action, "f") }`, want: planted},
	{name: "through a package variable set in init", src: `var bad string
func init() { bad = "Oops" }
func f() { a.warn(bad, action, "f") }`, want: planted},
	{name: "through a forwarded tuple", src: `func pair() (string, string) { return reasonOther, "Oops" }
func fwd() (string, string) { return pair() }
func f() { _, r := fwd(); a.warn(r, action, "f") }`, want: planted},
	{name: "through a neutral field", src: `type holder struct{ v string }
func f() { h := &holder{}; h.v = "Oops"; a.warn(h.v, action, "f") }`, want: planted},
	{name: "through a tuple-initialized global", src: `func pair() (string, string) { return reasonOther, "Oops" }
var x, chosen = pair()
func f() { a.warn(chosen, action, "f") }`, want: planted},
	{name: "through a method's result", src: `func (feedAttempt) pick() string { return "Oops" }
func f() { a.warn(a.pick(), action, "f") }`, want: planted},
	{name: "through an interface method", src: `type picker interface{ pick() string }
type lit struct{}
func (lit) pick() string { return "Oops" }
func f(p picker) { a.warn(p.pick(), action, "f") }`, want: planted},
	{name: "through a shadowed parameter", src: `func h(r string) { if c { r := "Oops"; a.warn(r, action, "f") } }
func f() { h(reasonOther) }`, want: planted},
	{name: "through a tuple assignment", src: `func pair() (string, string) { return reasonOther, "Oops" }
func f() { var other, x string; other, x = pair(); a.warn(x, action, other) }`, want: planted},
	{name: "through a named result", src: `func pick() (out string) { out = "Oops"; return }
func f() { a.warn(pick(), action, "f") }`, want: planted},
	{name: "through a range key", src: `func f() { for k := range map[string]int{"Oops": 1} { a.warn(k, action, "f") } }`, want: planted},
	{name: "through a range key of a parameter", src: `func h(m map[string]int) { for k := range m { a.warn(k, action, "f") } }
func f() { h(map[string]int{"Oops": 1}) }`, want: planted},
	{name: "through a range key of a package map", src: `var keys map[string]int
func init() { keys = map[string]int{"Oops": 1} }
func f() { for k := range keys { a.warn(k, action, "f") } }`, want: planted},
	{name: "through a range key of a captured map", src: `func f() {
	m := map[string]int{"Oops": 1}
	g := func() {
		for k := range m { a.warn(k, action, "f") }
	}
	g()
}`, want: planted},
	{name: "through a range key of a struct field map", src: `type reasonHolder struct { reasons map[string]int }
func f() {
	h := &reasonHolder{}
	h.reasons = map[string]int{"Oops": 1}
	for k := range h.reasons { a.warn(k, action, "f") }
}`, want: planted},
	{name: "through a range key of a locally allocated map variable", src: `func f() {
	var keys map[string]int
	ptr := &keys
	*ptr = map[string]int{"Oops": 1}
	for k := range keys { a.warn(k, action, "f") }
}`, want: planted},
	{name: "a constant declared outside the reason files", src: `const fieldOwner = "feedgroup-controller"
func f() { a.warn(fieldOwner, action, "f") }`, want: `"feedgroup-controller"`},
	{name: "a duplicate reason constant", src: `const alias = "Reconciled"
func f() { a.warn(alias, action, "f") }`, want: wantReconciled},
	{name: "through a pointer", src: `func set(p *string) { *p = "Oops" }
func f() { x := reasonOther; set(&x); a.warn(x, action, "f") }`, want: planted},
}

// productionCases are what production does, which must pass.
var productionCases = []sinkCase{
	{name: "named constants", src: `func f() { reason := reasonReconciled; if c { reason = reasonFeedErrors }; _ = metav1.Condition{Reason: reason}; a.warn(reasonConfigError, action, "feed %s", u) }`},
	{name: "a helper's other result", src: `func pair() (string, string) { return reasonOther, "message" }
func f() { r, m := pair(); a.warn(r, action, m) }`},
	{name: "a parameter given constants", src: `func h(r string) { a.warn(r, action, "f") }
func f() { h(reasonOther) }`},
	{name: "a field given constants", src: `func f() { a.setCondition(t, s, class.conditionReason, err.Error()) }`},
	{name: "empty zero value", src: `func f() { var r string; if c { r = reasonOther }; a.warn(r, action, "f") }`},
	{name: "a message beside the reason", src: `func f() { msg := "message"; a.setCondition(t, s, reasonOther, msg) }`},
	{name: "Reason field assigned a constant", src: `func f() { cond.Reason = reasonOther }`},
	{name: "reason helper returning a constant", src: `func dominantReason() string { return reasonOther }`},
	{name: "a map key and index", src: `func f() { m := map[string]string{"k": reasonOther}; a.warn(m["k"], action, "f") }`},
	{name: "a map key through a phi", src: `func f() { m := map[string]string{"k": reasonOther}; if c { m = map[string]string{"j": reasonFeedErrors} }; a.warn(m["k"], action, "f") }`},
	{name: "a map key through a call", src: `func reasons() map[string]string { return map[string]string{"k": reasonOther} }
func f() { a.warn(reasons()["k"], action, "f") }`},
	{name: "a map key beside a range value", src: `func f() { for _, r := range map[string]string{"k": reasonOther} { a.warn(r, action, "f") } }`},
	{name: "a helper's unrelated input", src: `func pick(key string) string { return reasonOther }
func f() { a.warn(pick("key"), action, "f") }`},
	{name: "elided failureClass constants", src: "var cs = []failureClass{{metricOther, reasonOther, false}}"},
	{name: "a local constant naming a reason", src: "func f() {\nconst reason = reasonOops\na.warn(reason, action, \"f\")\n}"},
}

// TestCheckReasons_CatchesEverySink keeps the guard above from passing
// vacuously: each sink and each path it traces is flagged when it carries a
// literal, and the constant forms production uses are not.
func TestCheckReasons_CatchesEverySink(t *testing.T) {
	for _, tc := range slices.Concat(directSinkCases, indirectSinkCases, syntacticMissCases, productionCases) {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkSnippet(t, tc.src)
			if tc.want == "" {
				if len(problems) > 0 {
					t.Fatalf("flagged %v, want nothing for:\n%s", problems, tc.src)
				}
				return
			}
			// Each problem names its string; the one the case plants must be
			// among them, not just some other string in the snippet.
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "string "+tc.want+" ") }) {
				t.Fatalf("problems %v do not report %s for:\n%s", problems, tc.want, tc.src)
			}
		})
	}
}
