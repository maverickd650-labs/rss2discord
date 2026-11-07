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
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// stringConstant returns the value of expr when Go's type checker knows it
// is a constant string expression.
func stringConstant(info *types.Info, expr ast.Expr) (string, bool) {
	if info == nil {
		return "", false
	}
	value, ok := info.Types[expr]
	if !ok || value.Value == nil || value.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value.Value), true
}

// containsNonEmptyStringLiteral reports whether expr contains a non-empty
// string literal. Empty strings appended to a named reason constant do not
// turn that named value into a split literal spelling.
func containsNonEmptyStringLiteral(expr ast.Expr) bool {
	for n := range ast.Preorder(expr) {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil && value != "" {
				return true
			}
		}
	}
	return false
}

// effectiveInitializers maps each spec of a const declaration to the
// initializer it evaluates: its own, or for an implicit repeat the previous
// explicit one in the group.
func effectiveInitializers(decl *ast.GenDecl) map[*ast.ValueSpec][]ast.Expr {
	effective := map[*ast.ValueSpec][]ast.Expr{}
	var inherited []ast.Expr
	for _, spec := range decl.Specs {
		values, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if len(values.Values) > 0 {
			inherited = values.Values
		}
		effective[values] = inherited
	}
	return effective
}

// checkReasonSpelling reports a reason spelling outside its one canonical
// const declaration. It evaluates typed constant expressions as well as
// individual literals, so concatenated fragments cannot spell a reason
// without using its named constant.
func checkReasonSpelling(fset *token.FileSet, files []*ast.File, info *types.Info, reasons, problems map[string]bool) {
	type spelling struct {
		pos       token.Pos
		canonical bool
	}

	constSpellings := map[string][]spelling{}
	for _, file := range files {
		filename := filepath.Base(fset.Position(file.Pos()).Filename)
		packageConsts := map[*ast.GenDecl]bool{}
		for _, decl := range file.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				packageConsts[gen] = true
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GenDecl:
				if n.Tok == token.CONST {
					// Count each declared constant by its evaluated value. This also
					// catches aliases and split-literal initialisers, even when the
					// reason spelling does not appear as one BasicLit.
					effective := effectiveInitializers(n)
					for _, spec := range n.Specs {
						values, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						initializers := effective[values]
						for _, name := range values.Names {
							declared, ok := info.Defs[name].(*types.Const)
							if !ok || declared.Val() == nil || declared.Val().Kind() != constant.String {
								continue
							}
							s := constant.StringVal(declared.Val())
							if !reasons[s] {
								continue
							}
							canonical := packageConsts[n] && reasonFiles[filename]
							if !canonical && !slices.ContainsFunc(initializers, containsNonEmptyStringLiteral) {
								// A local alias of a named constant (const reason =
								// reasonOops) adds no new spelling of the value.
								continue
							}
							constSpellings[s] = append(constSpellings[s], spelling{
								pos: name.Pos(), canonical: canonical,
							})
							if !canonical {
								problems[fmt.Sprintf("%s: string %s is declared by constant %s outside its canonical declaration", fset.Position(name.Pos()), strconv.Quote(s), name.Name)] = true
							}
						}
					}
					return false
				}
				return n.Tok != token.IMPORT
			case *ast.BasicLit:
				if s, ok := stringConstant(info, n); ok && reasons[s] {
					problems[fmt.Sprintf("%s: string %s spells a reason; use its constant", fset.Position(n.Pos()), n.Value)] = true
				}
			case ast.Expr:
				// A direct identifier may be the approved named constant. Other
				// expressions that fold to a reason and contain literal fragments
				// must not bypass the check.
				if _, isIdent := n.(*ast.Ident); isIdent {
					return true
				}
				if s, ok := stringConstant(info, n); ok && reasons[s] && containsNonEmptyStringLiteral(n) {
					problems[fmt.Sprintf("%s: string %s is spelled by a constant expression; use its named constant", fset.Position(n.Pos()), strconv.Quote(s))] = true
					return false
				}
			}
			return true
		})
	}

	for reason, spellings := range constSpellings {
		canonicalSeen := false
		for _, spelling := range spellings {
			if !spelling.canonical {
				continue
			}
			if canonicalSeen {
				problems[fmt.Sprintf("%s: constant value %s duplicates a reason spelling; keep it in one canonical const", fset.Position(spelling.pos), strconv.Quote(reason))] = true
				continue
			}
			canonicalSeen = true
		}
	}
}

// TestCheckReasonSpellingRejectsDuplicateConstSpellings ensures that even
// the designated reason files contain only one literal declaration per value.
func TestCheckReasonSpellingRejectsDuplicateConstSpellings(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "conditions.go", `package controller
const (
	reasonReconciled = "Reconciled"
	aliasReconciled  = "Reconciled"
)`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
	}
	if _, err := (&types.Config{}).Check("controller", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}

	problems := map[string]bool{}
	checkReasonSpelling(fset, []*ast.File{file}, info, map[string]bool{"Reconciled": true}, problems)
	if len(problems) == 0 {
		t.Fatal("duplicate reason spelling was not reported")
	}
}

// TestCheckReasonSpellingConstAliases pins which constant declarations outside
// the canonical one count as a spelling: a literal does, an alias of a named
// reason does not, and an implicit repeat counts as whatever the group's last
// explicit initializer is.
func TestCheckReasonSpellingConstAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string // constant names that must be reported
	}{
		{name: "alias of a named constant", src: "const reasonOops = \"Oops\"\nfunc f() { const x = reasonOops; _ = x }"},
		{name: "implicit repeat of an alias", src: "const reasonOops = \"Oops\"\nfunc f() { const (\nx = reasonOops\ny\n); _, _ = x, y }"},
		{name: "implicit repeat of a literal", src: "const reasonOops = \"Oops\"\nfunc f() { const (\nx = \"Oops\"\ny\n); _, _ = x, y }", want: []string{"constant x ", "constant y "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "other.go", "package controller\n"+tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}}
			if _, err := (&types.Config{}).Check("controller", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			problems := map[string]bool{}
			checkReasonSpelling(fset, []*ast.File{file}, info, map[string]bool{"Oops": true}, problems)
			// reasonOops itself sits in a non-reason file here, so it is reported
			// too; only the local constants are under test.
			for _, name := range tc.want {
				if !slices.ContainsFunc(slices.Collect(maps.Keys(problems)), func(p string) bool { return strings.Contains(p, name) }) {
					t.Fatalf("%q not reported in %v", name, problems)
				}
			}
			if len(tc.want) == 0 {
				for p := range problems {
					if strings.Contains(p, "constant x ") || strings.Contains(p, "constant y ") {
						t.Fatalf("alias reported: %s", p)
					}
				}
			}
		})
	}
}
