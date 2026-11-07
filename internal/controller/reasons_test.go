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
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// reasonFiles are the files whose string constants may be reasons
// (prelude.go is the self-test's stand-in for them).
var reasonFiles = map[string]bool{"conditions.go": true, "classify.go": true, "prelude.go": true}

// checkReasons runs reasonFlow and checkReasonSpelling over pkg, whose syntax
// is files, and returns their findings, sorted.
func checkReasons(fset *token.FileSet, pkg *ssa.Package, info *types.Info, files []*ast.File) []string {
	f := &reasonFlow{
		fset: fset, named: map[string]bool{},
		globalWrites: map[*ssa.Global][]ssa.Value{}, fieldWrites: map[*types.Var][]ssa.Value{},
		bindings: map[*ssa.Function][]*ssa.MakeClosure{},
		seen:     map[ssa.Value]bool{}, seenKeys: map[ssa.Value]bool{},
		reached: map[string]bool{}, problems: map[string]bool{},
	}
	scope := pkg.Pkg.Scope()
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if ok && c.Val().Kind() == constant.String && reasonFiles[filepath.Base(fset.Position(c.Pos()).Filename)] {
			f.named[constant.StringVal(c.Val())] = true
		}
	}
	f.index(pkg)
	f.checkSinks()
	checkReasonSpelling(fset, files, info, f.reached, f.problems)
	return slices.Sorted(maps.Keys(f.problems))
}

// buildSSA builds the SSA form of a type-checked package, with types-only
// packages for everything it imports.
func buildSSA(fset *token.FileSet, pkg *types.Package, files []*ast.File, info *types.Info) *ssa.Package {
	prog := ssa.NewProgram(fset, 0)
	var create func([]*types.Package)
	create = func(pkgs []*types.Package) {
		for _, p := range pkgs {
			if prog.Package(p) == nil {
				prog.CreatePackage(p, nil, nil, true)
				create(p.Imports())
			}
		}
	}
	create(pkg.Imports())
	ssaPkg := prog.CreatePackage(pkg, files, info, false)
	ssaPkg.Build()
	return ssaPkg
}

// TestCheckReasons_UnresolvedStringResults pins what a call out of the package
// does to a reason: its string result is not a named constant, whatever its
// arguments were, while a result of another type is not a reason at all.
func TestCheckReasons_UnresolvedStringResults(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		flagged   bool
	}{
		{name: "transformed constant", src: `func f() { a.warn(ext.Upper(reasonReconciled), action, "f") }`, flagged: true},
		{name: "string element of a tuple", src: `func f() { r, _ := ext.Split(reasonOther); a.warn(r, action, "f") }`, flagged: true},
		// The non-string results below reach the reason itself, so only the type
		// check keeps them from being reported.
		{name: "non-string element of a tuple", src: `func f() { _, n := ext.Split(reasonOther); a.warn(string(rune(n)), action, "f") }`},
		{name: "non-string result", src: `func f() { n := ext.Count(reasonOther); a.warn(string(rune(n)), action, "f") }`},
		// And a string result that only reaches the message is not a reason.
		{name: "string result off the reason path", src: `func f() { s := ext.Upper(reasonOther); a.warn(reasonOther, action, s) }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkSnippet(t, tc.src)
			got := slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, "string result of an unresolved call") })
			if got != tc.flagged {
				t.Fatalf("problems %v, flagged=%v want %v for:\n%s", problems, got, tc.flagged, tc.src)
			}
		})
	}
}

// TestProductionReasonsAreNamedConstants enforces CLAUDE.md's "every reason
// string in production code is a const": condition and Event reasons are
// user-visible API (kubectl, Events, dashboards, alerts), so each must be one
// of the named declarations, never a literal that reaches a reason, however
// indirectly.
func TestProductionReasonsAreNamedConstants(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{Mode: packages.LoadSyntax}, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || len(pkgs[0].Errors) > 0 || len(pkgs[0].Syntax) == 0 {
		t.Fatalf("loading the package: %v", pkgs)
	}
	p := pkgs[0]
	if problems := checkReasons(p.Fset, buildSSA(p.Fset, p.Types, p.Syntax, p.TypesInfo), p.TypesInfo, p.Syntax); len(problems) > 0 {
		t.Fatalf("condition/Event reasons must be named constants (conditions.go, classify.go):\n%s",
			strings.Join(problems, "\n"))
	}
}
