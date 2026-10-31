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
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// reasonArgs are the calls that take a condition or Event reason, by function
// or method name, with the position of that argument (the receiver aside).
var reasonArgs = map[string]int{
	"setFeedCondition": 3, // (fs, condType, status, reason, message, generation)
	"setCondition":     2, // feedAttempt: (condType, status, reason, message)
	"warn":             0, // feedAttempt: (reason, action, format, args...)
	"Eventf":           3, // events.EventRecorder: (regarding, related, type, reason, action, note, args...)
}

// reasonFields are the struct fields that hold a condition reason.
var reasonFields = []string{"Reason", "conditionReason"}

// reasonFlow finds the string constants that reach a condition or Event reason
// in one package, on its SSA form (golang.org/x/tools/go/ssa), where every
// value has exactly one definition: shadowing, tuples, named results, closures
// and methods are all plain values and calls there.
//
// The sinks are a reasonArgs argument, a store to a reasonFields field (keyed,
// unkeyed and elided literals and later assignments all lower to one), and
// what a function named for a reason (dominantErrorReason) returns. From each,
// the value is traced back through phis, conversions and operators; a call to
// the package's own functions, methods or closures to what they return; a
// parameter or free variable to every caller's argument or closure's binding;
// a load to every store to that variable, global or field (any field, not only
// the reason ones); and a map, slice or pointer to everything written into it.
// A call into another package is traced through its arguments, since its
// result can only be made from them.
//
// SSA folds named constants into their values, so a constant that arrives is
// accepted only if its value is that of one of the package's named string
// constants, and checkReasonSpelling then requires that value to be spelled
// nowhere but in its const declaration. Together: every reason is one of the
// declarations in conditions.go and classify.go, which are user-visible API.
type reasonFlow struct {
	fset         *token.FileSet
	named        map[string]bool // values of the package's string constants
	funcs        []*ssa.Function // the package's, closures included
	calls        []ssa.CallInstruction
	globalWrites map[*ssa.Global][]ssa.Value
	fieldWrites  map[*types.Var][]ssa.Value
	bindings     map[*ssa.Function][]*ssa.MakeClosure
	seen         map[ssa.Value]bool
	seenValues   map[ssa.Value]bool // maps whose values traceMapValues followed
	reached      map[string]bool    // named values found at a sink
	problems     map[string]bool
	where        string    // the sink being traced
	pos          token.Pos // and its position
}

// checkReasons runs reasonFlow and checkReasonSpelling over pkg, whose syntax
// is files, and returns their findings, sorted.
func checkReasons(fset *token.FileSet, pkg *ssa.Package, files []*ast.File) []string {
	f := &reasonFlow{
		fset: fset, named: map[string]bool{},
		globalWrites: map[*ssa.Global][]ssa.Value{}, fieldWrites: map[*types.Var][]ssa.Value{},
		bindings: map[*ssa.Function][]*ssa.MakeClosure{},
		seen:     map[ssa.Value]bool{}, seenValues: map[ssa.Value]bool{},
		reached: map[string]bool{}, problems: map[string]bool{},
	}
	scope := pkg.Pkg.Scope()
	for _, name := range scope.Names() {
		if c, ok := scope.Lookup(name).(*types.Const); ok && c.Val().Kind() == constant.String {
			f.named[constant.StringVal(c.Val())] = true
		}
	}
	f.index(pkg)
	f.checkSinks()
	checkReasonSpelling(fset, files, f.reached, f.problems)
	return slices.Sorted(func(yield func(string) bool) {
		for p := range f.problems {
			if !yield(p) {
				return
			}
		}
	})
}

// index records pkg's functions (closures included), call sites, and every
// value stored to a package variable or a struct field, or bound into a
// closure.
func (f *reasonFlow) index(pkg *ssa.Package) {
	var add func(fn *ssa.Function)
	add = func(fn *ssa.Function) {
		if fn == nil || fn.Pkg != pkg || slices.Contains(f.funcs, fn) {
			return
		}
		f.funcs = append(f.funcs, fn)
		for _, anon := range fn.AnonFuncs {
			add(anon)
		}
	}
	for _, member := range pkg.Members {
		switch m := member.(type) {
		case *ssa.Function:
			add(m)
			if m.Name() == "init" {
				// The package initializer calls the init funcs declared.
				for _, block := range m.Blocks {
					for _, instr := range block.Instrs {
						if call, ok := instr.(*ssa.Call); ok {
							add(call.Call.StaticCallee())
						}
					}
				}
			}
		case *ssa.Type:
			for _, t := range []types.Type{m.Type(), types.NewPointer(m.Type())} {
				for method := range pkg.Prog.MethodSets.MethodSet(t).Methods() {
					if fn := pkg.Prog.MethodValue(method); fn != nil && fn.Synthetic == "" {
						add(fn) // not the wrappers *T has for T's methods
					}
				}
			}
		}
	}
	for _, fn := range f.funcs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				f.indexInstr(instr)
			}
		}
	}
}

func (f *reasonFlow) indexInstr(instr ssa.Instruction) {
	switch in := instr.(type) {
	case ssa.CallInstruction:
		f.calls = append(f.calls, in)
	case *ssa.MakeClosure:
		fn := in.Fn.(*ssa.Function)
		f.bindings[fn] = append(f.bindings[fn], in)
	case *ssa.Store:
		switch addr := in.Addr.(type) {
		case *ssa.Global:
			f.globalWrites[addr] = append(f.globalWrites[addr], in.Val)
		case *ssa.FieldAddr:
			field := fieldOf(addr.X.Type(), addr.Field)
			f.fieldWrites[field] = append(f.fieldWrites[field], in.Val)
		}
	}
}

// fieldOf is field i of the struct typ is or points to.
func fieldOf(typ types.Type, i int) *types.Var {
	if ptr, ok := typ.Underlying().(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	return typ.Underlying().(*types.Struct).Field(i)
}

// checkSinks traces every reason sink in the package.
func (f *reasonFlow) checkSinks() {
	for _, call := range f.calls {
		common := call.Common()
		name, offset := "", 0
		if common.IsInvoke() {
			name = common.Method.Name()
		} else if callee := common.StaticCallee(); callee != nil {
			name = callee.Name()
			if callee.Signature.Recv() != nil {
				offset = 1 // a static method call's receiver is Args[0]
			}
		}
		if i, ok := reasonArgs[name]; ok && i+offset < len(common.Args) {
			f.sink(common.Args[i+offset], call.Pos(), "the reason argument of "+name)
		}
	}
	for _, fn := range f.funcs {
		reasonNamed := strings.Contains(strings.ToLower(fn.Name()), "reason")
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				switch in := instr.(type) {
				case *ssa.Store:
					if fa, ok := in.Addr.(*ssa.FieldAddr); ok {
						if name := fieldOf(fa.X.Type(), fa.Field).Name(); slices.Contains(reasonFields, name) {
							f.sink(in.Val, in.Pos(), "the "+name+" field")
						}
					}
				case *ssa.Return:
					if reasonNamed {
						for _, result := range in.Results {
							f.sink(result, in.Pos(), "a result of "+fn.Name())
						}
					}
				}
			}
		}
	}
}

// sink traces v, which reaches the sink described by where at pos.
func (f *reasonFlow) sink(v ssa.Value, pos token.Pos, where string) {
	f.where, f.pos = where, pos
	f.trace(v)
}

// trace follows v back to the constants it is made from.
func (f *reasonFlow) trace(v ssa.Value) {
	if v == nil || f.seen[v] {
		return
	}
	f.seen[v] = true
	f.traceDef(v)
	switch v.Type().Underlying().(type) {
	case *types.Map, *types.Slice, *types.Pointer:
		f.traceWrites(v, false)
	}
}

// traceDef follows v to the values its definition is made from.
func (f *reasonFlow) traceDef(v ssa.Value) {
	switch v := v.(type) {
	case *ssa.Const:
		f.checkConst(v)
	case *ssa.Call:
		f.traceCallResult(v, 0)
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok {
			f.traceCallResult(call, v.Index)
		} else {
			f.trace(v.Tuple)
		}
	case *ssa.Parameter:
		f.traceCallers(v)
	case *ssa.FreeVar:
		fn := v.Parent()
		i := slices.Index(fn.FreeVars, v)
		for _, mc := range f.bindings[fn] {
			f.trace(mc.Bindings[i])
		}
	case *ssa.Global:
		for _, w := range f.globalWrites[v] {
			f.trace(w)
		}
	case *ssa.Field:
		f.traceField(fieldOf(v.X.Type(), v.Field))
	case *ssa.FieldAddr:
		f.traceField(fieldOf(v.X.Type(), v.Field))
	case *ssa.Lookup:
		if _, isMap := v.X.Type().Underlying().(*types.Map); isMap {
			f.traceMapValues(v.X) // the key looked up is not the value
		} else {
			f.trace(v.X)
		}
	case *ssa.IndexAddr:
		f.trace(v.X)
	case *ssa.Index:
		f.trace(v.X)
	case *ssa.Alloc, *ssa.MakeMap, *ssa.MakeSlice, *ssa.MakeChan, *ssa.Function, *ssa.MakeClosure, *ssa.Builtin:
		// Made empty, or code: what is put in them is traced by traceWrites.
	case ssa.Instruction:
		// Phis, operators, conversions, loads, ranges, slicing: made from
		// their operands.
		for _, op := range v.Operands(nil) {
			f.trace(*op)
		}
	}
}

// traceMapValues follows the values (not the keys) a map is given.
func (f *reasonFlow) traceMapValues(m ssa.Value) {
	if f.seenValues[m] {
		return
	}
	f.seenValues[m] = true
	f.traceDef(m)
	f.traceWrites(m, true)
}

// traceWrites follows everything written into the map, slice or pointer v:
// map updates (only their values if valuesOnly), stores through it or an
// element of it, and what a closure or function it is handed stores through
// it.
func (f *reasonFlow) traceWrites(v ssa.Value, valuesOnly bool) {
	refs := v.Referrers()
	if refs == nil {
		return
	}
	for _, ref := range *refs {
		switch r := ref.(type) {
		case *ssa.MapUpdate:
			if r.Map == v {
				if !valuesOnly {
					f.trace(r.Key)
				}
				f.trace(r.Value)
			}
		case *ssa.Store:
			if r.Addr == v {
				f.trace(r.Val)
			}
		case *ssa.IndexAddr:
			if r.X == v {
				f.trace(r)
			}
		case *ssa.Slice:
			if r.X == v {
				f.trace(r)
			}
		case *ssa.MakeClosure:
			for i, b := range r.Bindings {
				if b == v {
					f.trace(r.Fn.(*ssa.Function).FreeVars[i])
				}
			}
		case ssa.CallInstruction:
			f.traceHandedTo(r.Common(), v)
		}
	}
}

// traceHandedTo follows what the package's callees of common store through v,
// which it passes them.
func (f *reasonFlow) traceHandedTo(common *ssa.CallCommon, v ssa.Value) {
	for i, arg := range common.Args {
		if arg != v {
			continue
		}
		for _, callee := range f.callees(common) {
			if j := i + receiverShift(common, callee); j < len(callee.Params) {
				f.trace(callee.Params[j])
			}
		}
	}
}

// traceField follows every value stored to field, anywhere in the package.
func (f *reasonFlow) traceField(field *types.Var) {
	for _, w := range f.fieldWrites[field] {
		f.trace(w)
	}
}

// traceCallResult follows result i of call: what the package's callees
// return there, or, for a call out of the package, its arguments.
func (f *reasonFlow) traceCallResult(call *ssa.Call, i int) {
	callees := f.callees(&call.Call)
	if len(callees) == 0 {
		for _, arg := range call.Call.Args {
			f.trace(arg)
		}
		return
	}
	for _, callee := range callees {
		for _, block := range callee.Blocks {
			if ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return); ok && i < len(ret.Results) {
				f.trace(ret.Results[i])
			}
		}
	}
}

// traceCallers follows parameter p to the matching argument of every call
// that can reach its function.
func (f *reasonFlow) traceCallers(p *ssa.Parameter) {
	fn := p.Parent()
	index := slices.Index(fn.Params, p)
	for _, call := range f.calls {
		common := call.Common()
		if !slices.Contains(f.callees(common), fn) {
			continue
		}
		if i := index - receiverShift(common, fn); i >= 0 && i < len(common.Args) {
			f.trace(common.Args[i])
		}
	}
}

// callees are the package's functions with a body that common can call: its
// static callee; for an interface call, the package's methods of that name
// whose receiver implements the interface; for a call through a func value,
// every function of the package with the same signature.
func (f *reasonFlow) callees(common *ssa.CallCommon) []*ssa.Function {
	if callee := common.StaticCallee(); callee != nil {
		if slices.Contains(f.funcs, callee) && len(callee.Blocks) > 0 {
			return []*ssa.Function{callee}
		}
		return nil
	}
	if _, builtin := common.Value.(*ssa.Builtin); builtin {
		return nil
	}
	var out []*ssa.Function
	for _, fn := range f.funcs {
		recv := fn.Signature.Recv()
		switch {
		case len(fn.Blocks) == 0:
		case common.IsInvoke():
			if recv != nil && fn.Name() == common.Method.Name() &&
				types.Implements(recv.Type(), common.Value.Type().Underlying().(*types.Interface)) {
				out = append(out, fn)
			}
		case recv == nil && types.Identical(fn.Signature, common.Signature()):
			out = append(out, fn)
		}
	}
	return out
}

// receiverShift is how far callee's Params run ahead of common's Args: an
// interface call leaves the receiver out of Args, a static method call does
// not.
func receiverShift(common *ssa.CallCommon, callee *ssa.Function) int {
	if common.IsInvoke() && callee.Signature.Recv() != nil {
		return 1
	}
	return 0
}

// checkConst records a string constant that reaches a sink: the empty zero
// value passes, a named constant's value is noted for checkReasonSpelling,
// and anything else is a problem.
func (f *reasonFlow) checkConst(c *ssa.Const) {
	if c.Value == nil || c.Value.Kind() != constant.String {
		return
	}
	s := constant.StringVal(c.Value)
	switch {
	case s == "":
	case f.named[s]:
		f.reached[s] = true
	default:
		f.problems[fmt.Sprintf("%s: string %s reaches %s", f.fset.Position(f.pos), strconv.Quote(s), f.where)] = true
	}
}

// checkReasonSpelling reports a string literal outside a const declaration
// whose value is one of the reasons in use: a reason's value is spelled once,
// in its constant.
func checkReasonSpelling(fset *token.FileSet, files []*ast.File, reasons, problems map[string]bool) {
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GenDecl:
				return n.Tok != token.CONST && n.Tok != token.IMPORT
			case *ast.BasicLit:
				if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && reasons[s] {
					problems[fmt.Sprintf("%s: string %s spells a reason; use its constant", fset.Position(n.Pos()), n.Value)] = true
				}
			}
			return true
		})
	}
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
	if problems := checkReasons(p.Fset, buildSSA(p.Fset, p.Types, p.Syntax, p.TypesInfo), p.Syntax); len(problems) > 0 {
		t.Fatalf("condition/Event reasons must be named constants (conditions.go, classify.go):\n%s",
			strings.Join(problems, "\n"))
	}
}

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
func setFeedCondition(fs any, condType, status, reason, message string, generation int64) {}

type recorder interface {
	Eventf(regarding, related any, eventtype, reason, action, note string, args ...any)
}

const (
	reasonOther       = "Other"
	reasonReconciled  = "Reconciled"
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
	"ext":  "package ext\nfunc Upper(s string) string { return s }\n",
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
	return checkReasons(fset, buildSSA(fset, pkg, files, info), files)
}

// TestCheckReasons_CatchesEverySink keeps the guard above from passing
// vacuously: each sink and each path it traces is flagged when it carries a
// literal, and the constant forms production uses are not.
func TestCheckReasons_CatchesEverySink(t *testing.T) {
	// planted is the string (quoted) most cases plant at a sink.
	const planted = `"Oops"`
	for _, tc := range []struct {
		name, src string
		// want is the quoted string a problem must name ("" for none).
		want string
	}{
		{name: "Reason field", src: `func f() { _ = metav1.Condition{Reason: "Oops"} }`, want: planted},
		{name: "unkeyed Condition", src: `func f() { _ = metav1.Condition{t, s, "Oops", m, 1} }`, want: planted},
		{name: "conditionReason field", src: `func f() { _ = failureClass{conditionReason: "Oops"} }`, want: planted},
		{name: "unkeyed failureClass", src: `var cs = failureClass{metricOther, "Oops", false}`, want: planted},
		{name: "setFeedCondition", src: `func f() { setFeedCondition(fs, t, s, "Oops", m, 1) }`, want: planted},
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
		{name: "through another package", src: `func f() { a.warn(ext.Upper("Oops"), action, "f") }`, want: planted},

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
		{name: "through two parameters", src: `func h(r string) { k(r) }
func k(r string) { a.warn(r, action, "f") }
func f() { h("Oops") }`, want: planted},
		{name: "through a method parameter", src: `func (a feedAttempt) h(x int, r string) { a.warn(r, action, "f") }
func f() { a.h(1, "Oops") }`, want: planted},
		{name: "an elided failureClass in a slice", src: "var cs = []failureClass{{metricOther, \"Oops\", false}}", want: planted},
		{name: "an elided failureClass in a map", src: "var cs = map[string]failureClass{\"k\": {metricOther, \"Oops\", false}}", want: planted},
		{name: "through a map value", src: `func f() { m := map[string]string{"k": "Oops"}; a.warn(m["k"], action, "f") }`, want: planted},
		{name: "through a slice element", src: `func f() { xs := []string{reasonOther}; xs[0] = "Oops"; a.warn(xs[0], action, "f") }`, want: planted},

		// The paths a syntactic trace missed.
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
		{name: "through a pointer", src: `func set(p *string) { *p = "Oops" }
func f() { x := reasonOther; set(&x); a.warn(x, action, "f") }`, want: planted},

		// What production does, which must pass.
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
		{name: "a helper's unrelated input", src: `func pick(key string) string { return reasonOther }
func f() { a.warn(pick("key"), action, "f") }`},
		{name: "elided failureClass constants", src: "var cs = []failureClass{{metricOther, reasonOther, false}}"},
		{name: "a local constant naming a reason", src: "func f() {\nconst reason = reasonOops\na.warn(reason, action, \"f\")\n}"},
	} {
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
