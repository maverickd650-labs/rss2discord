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
	value := info.Types[expr].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
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

// spelling is one declaration of a reason's value as a constant.
type spelling struct {
	pos       token.Pos
	canonical bool
}

// spellingCheck is one checkReasonSpelling run.
type spellingCheck struct {
	fset     *token.FileSet
	info     *types.Info
	reasons  map[string]bool
	problems map[string]bool
	// constSpellings are the declarations of each reason's value.
	constSpellings map[string][]spelling
}

// checkReasonSpelling reports a reason spelling outside its one canonical
// const declaration. It evaluates typed constant expressions as well as
// individual literals, so concatenated fragments cannot spell a reason
// without using its named constant.
func checkReasonSpelling(fset *token.FileSet, files []*ast.File, info *types.Info, reasons, problems map[string]bool) {
	c := &spellingCheck{
		fset: fset, info: info, reasons: reasons, problems: problems,
		constSpellings: map[string][]spelling{},
	}
	for _, file := range files {
		c.checkFile(file)
	}
	c.reportDuplicates()
}

// checkFile checks every constant declaration, literal and constant
// expression of file.
func (c *spellingCheck) checkFile(file *ast.File) {
	inReasonFile := reasonFiles[filepath.Base(c.fset.Position(file.Pos()).Filename)]
	packageConsts := packageConstDecls(file)
	ast.Inspect(file, func(n ast.Node) bool {
		return c.visit(n, packageConsts[n] && inReasonFile)
	})
}

// packageConstDecls are the package-level const declarations of file, as
// nodes.
func packageConstDecls(file *ast.File) map[ast.Node]bool {
	decls := map[ast.Node]bool{}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			decls[gen] = true
		}
	}
	return decls
}

// visit checks node n and says whether to descend into it; canonical is
// whether n is a const declaration where reasons are declared.
func (c *spellingCheck) visit(n ast.Node, canonical bool) bool {
	switch n := n.(type) {
	case *ast.GenDecl:
		if n.Tok == token.CONST {
			c.checkConstDecl(n, canonical)
			return false
		}
		return n.Tok != token.IMPORT
	case *ast.BasicLit:
		c.checkLiteral(n)
	case ast.Expr:
		return c.checkExpr(n)
	}
	return true
}

// checkLiteral reports a string literal that spells a reason.
func (c *spellingCheck) checkLiteral(lit *ast.BasicLit) {
	if s, ok := stringConstant(c.info, lit); ok && c.reasons[s] {
		c.problems[fmt.Sprintf("%s: string %s spells a reason; use its constant", c.fset.Position(lit.Pos()), lit.Value)] = true
	}
}

// checkExpr reports an expression that folds to a reason and contains literal
// fragments, and says whether to descend into it. A direct identifier may be
// the approved named constant; other expressions must not bypass the check.
func (c *spellingCheck) checkExpr(expr ast.Expr) (descend bool) {
	if _, isIdent := expr.(*ast.Ident); isIdent {
		return true
	}
	s, ok := stringConstant(c.info, expr)
	if !ok || !c.reasons[s] || !containsNonEmptyStringLiteral(expr) {
		return true
	}
	c.problems[fmt.Sprintf("%s: string %s is spelled by a constant expression; use its named constant", c.fset.Position(expr.Pos()), strconv.Quote(s))] = true
	return false
}

// checkConstDecl counts each constant declared by decl by its evaluated value.
// This also catches aliases and split-literal initialisers, even when the
// reason spelling does not appear as one BasicLit.
func (c *spellingCheck) checkConstDecl(decl *ast.GenDecl, canonical bool) {
	effective := effectiveInitializers(decl)
	for _, spec := range decl.Specs {
		values, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, name := range values.Names {
			c.checkConstName(name, effective[values], canonical)
		}
	}
}

// checkConstName records the spelling name gives its value, if that is a
// reason, and reports it unless it is the canonical declaration.
func (c *spellingCheck) checkConstName(name *ast.Ident, initializers []ast.Expr, canonical bool) {
	s, ok := declaredString(c.info, name)
	if !ok || !c.reasons[s] {
		return
	}
	if !canonical && !slices.ContainsFunc(initializers, containsNonEmptyStringLiteral) {
		// A local alias of a named constant (const reason = reasonOops) adds no
		// new spelling of the value.
		return
	}
	c.constSpellings[s] = append(c.constSpellings[s], spelling{pos: name.Pos(), canonical: canonical})
	if !canonical {
		c.problems[fmt.Sprintf("%s: string %s is declared by constant %s outside its canonical declaration", c.fset.Position(name.Pos()), strconv.Quote(s), name.Name)] = true
	}
}

// declaredString is the value of the string constant name declares.
func declaredString(info *types.Info, name *ast.Ident) (string, bool) {
	declared, ok := info.Defs[name].(*types.Const)
	if !ok {
		return "", false
	}
	value := declared.Val()
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
}

// reportDuplicates reports every canonical declaration of a value after the
// first.
func (c *spellingCheck) reportDuplicates() {
	for reason, spellings := range c.constSpellings {
		canonical := slices.DeleteFunc(slices.Clone(spellings), func(s spelling) bool { return !s.canonical })
		for _, dup := range canonical[min(1, len(canonical)):] {
			c.problems[fmt.Sprintf("%s: constant value %s duplicates a reason spelling; keep it in one canonical const", c.fset.Position(dup.pos), strconv.Quote(reason))] = true
		}
	}
}

// checkSpellingSource type-checks src as the file filename of package
// controller and runs checkReasonSpelling over it with the given reasons.
func checkSpellingSource(t *testing.T, filename, src string, reasons ...string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{}).Check("controller", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	problems := map[string]bool{}
	checkReasonSpelling(fset, []*ast.File{file}, info, setOf(reasons), problems)
	return problems
}

// setOf is the set of values.
func setOf(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// reports reports whether some problem mentions substr.
func reports(problems map[string]bool, substr string) bool {
	return slices.ContainsFunc(slices.Collect(maps.Keys(problems)), func(p string) bool { return strings.Contains(p, substr) })
}

// TestCheckReasonSpellingRejectsDuplicateConstSpellings ensures that even
// the designated reason files contain only one literal declaration per value.
func TestCheckReasonSpellingRejectsDuplicateConstSpellings(t *testing.T) {
	problems := checkSpellingSource(t, "conditions.go", `package controller
const (
	reasonReconciled = "Reconciled"
	aliasReconciled  = "Reconciled"
)`, "Reconciled")
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
			// reasonOops itself sits in a non-reason file here, so it is reported
			// too; only the local constants are under test.
			problems := checkSpellingSource(t, "other.go", "package controller\n"+tc.src, "Oops")
			for _, name := range tc.want {
				if !reports(problems, name) {
					t.Fatalf("%q not reported in %v", name, problems)
				}
			}
			if len(tc.want) == 0 && (reports(problems, "constant x ") || reports(problems, "constant y ")) {
				t.Fatalf("alias reported: %v", problems)
			}
		})
	}
}
