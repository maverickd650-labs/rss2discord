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
	"go/constant"
	"go/token"
	"go/types"
	"iter"
	"slices"
	"strconv"
	"strings"

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
// the reason ones); and a map, slice or pointer to everything written into it
// -- a map's values, and its keys only where a range over it yields them.
// A call into another package is traced through its arguments, since its
// result can only be made from them.
//
// SSA folds named constants into their values, so a constant that arrives is
// accepted only if its value is that of a string constant declared in one of
// the reasonFiles, and checkReasonSpelling then requires that value to be spelled
// nowhere but in its const declaration. Together: every reason is one of the
// declarations in conditions.go and classify.go, which are user-visible API.
type reasonFlow struct {
	fset         *token.FileSet
	named        map[string]bool // values of the reasonFiles' string constants
	funcs        []*ssa.Function // the package's, closures included
	calls        []ssa.CallInstruction
	globalWrites map[*ssa.Global][]ssa.Value
	fieldWrites  map[*types.Var][]ssa.Value
	bindings     map[*ssa.Function][]*ssa.MakeClosure
	seen         map[ssa.Value]bool
	seenKeys     map[ssa.Value]bool // maps whose keys traceMapKeys followed
	reached      map[string]bool    // named values found at a sink
	problems     map[string]bool
	where        string    // the sink being traced
	pos          token.Pos // and its position
}

// index records pkg's functions (closures included), call sites, and every
// value stored to a package variable or a struct field, or bound into a
// closure.
func (f *reasonFlow) index(pkg *ssa.Package) {
	for _, member := range pkg.Members {
		switch m := member.(type) {
		case *ssa.Function:
			f.addFunc(pkg, m)
			if m.Name() == "init" {
				// The package initializer calls the init funcs declared.
				for callee := range staticCallees(m) {
					f.addFunc(pkg, callee)
				}
			}
		case *ssa.Type:
			for fn := range declaredMethods(pkg.Prog, m.Type()) {
				f.addFunc(pkg, fn)
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

// addFunc records fn, if it is pkg's and new, and the closures inside it.
func (f *reasonFlow) addFunc(pkg *ssa.Package, fn *ssa.Function) {
	if fn == nil || fn.Pkg != pkg || slices.Contains(f.funcs, fn) {
		return
	}
	f.funcs = append(f.funcs, fn)
	for _, anon := range fn.AnonFuncs {
		f.addFunc(pkg, anon)
	}
}

// staticCallees are the functions fn calls directly, nil results included.
func staticCallees(fn *ssa.Function) iter.Seq[*ssa.Function] {
	return func(yield func(*ssa.Function) bool) {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				if call, ok := instr.(*ssa.Call); ok && !yield(call.Call.StaticCallee()) {
					return
				}
			}
		}
	}
}

// declaredMethods are the methods typ and *typ declare, not the wrappers *T has
// for T's methods.
func declaredMethods(prog *ssa.Program, typ types.Type) iter.Seq[*ssa.Function] {
	return func(yield func(*ssa.Function) bool) {
		for _, t := range []types.Type{typ, types.NewPointer(typ)} {
			for method := range prog.MethodSets.MethodSet(t).Methods() {
				if fn := prog.MethodValue(method); fn != nil && fn.Synthetic == "" && !yield(fn) {
					return
				}
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
		f.checkCallSink(call)
	}
	for _, fn := range f.funcs {
		reasonNamed := strings.Contains(strings.ToLower(fn.Name()), "reason")
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				f.checkInstrSink(fn, reasonNamed, instr)
			}
		}
	}
}

// checkCallSink traces the reason argument of call, if it is one of reasonArgs.
func (f *reasonFlow) checkCallSink(call ssa.CallInstruction) {
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

// checkInstrSink traces a store to a reasonFields field, and the results a
// function named for a reason returns (reasonNamed).
func (f *reasonFlow) checkInstrSink(fn *ssa.Function, reasonNamed bool, instr ssa.Instruction) {
	switch in := instr.(type) {
	case *ssa.Store:
		fa, ok := in.Addr.(*ssa.FieldAddr)
		if !ok {
			return
		}
		if name := fieldOf(fa.X.Type(), fa.Field).Name(); slices.Contains(reasonFields, name) {
			f.sink(in.Val, in.Pos(), "the "+name+" field")
		}
	case *ssa.Return:
		if !reasonNamed {
			return
		}
		for _, result := range in.Results {
			f.sink(result, in.Pos(), "a result of "+fn.Name())
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
		f.traceWrites(v)
	}
}

// traceDef follows v to the values its definition is made from.
func (f *reasonFlow) traceDef(v ssa.Value) {
	switch v := v.(type) {
	case *ssa.Const:
		f.checkConst(v)
	case *ssa.Call:
		f.traceCallResult(v, 0, f.trace)
	case *ssa.Extract:
		f.traceExtract(v)
	case *ssa.Parameter:
		f.traceCallers(v, f.trace)
	case *ssa.FreeVar:
		for _, bound := range f.boundTo(v) {
			f.trace(bound)
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
		f.trace(v.X) // a map's values; the key looked up is not the value
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

// traceExtract follows one result of a tuple: a call's result by position, a
// range over a map's key to the map's keys, anything else to the tuple.
func (f *reasonFlow) traceExtract(v *ssa.Extract) {
	switch t := v.Tuple.(type) {
	case *ssa.Call:
		f.traceCallResult(t, v.Index, f.trace)
	case *ssa.Next:
		if m := rangedMap(t); m != nil && v.Index == 1 {
			f.traceMapKeys(m)
			return
		}
		f.trace(t)
	default:
		f.trace(t)
	}
}

// rangedMap is the map next ranges over, or nil.
func rangedMap(next *ssa.Next) ssa.Value {
	rng, ok := next.Iter.(*ssa.Range)
	if !ok {
		return nil
	}
	if _, isMap := rng.X.Type().Underlying().(*types.Map); !isMap {
		return nil
	}
	return rng.X
}

// boundTo are the values every closure binds to its free variable v.
func (f *reasonFlow) boundTo(v *ssa.FreeVar) []ssa.Value {
	fn := v.Parent()
	i := slices.Index(fn.FreeVars, v)
	if i < 0 {
		return nil
	}
	var bound []ssa.Value
	for _, closure := range f.bindings[fn] {
		if i < len(closure.Bindings) {
			bound = append(bound, closure.Bindings[i])
		}
	}
	return bound
}

// traceMapKeys follows the keys put into map m, and into the maps it is made
// from: phis, parameters, calls, globals, fields, closures and allocations.
func (f *reasonFlow) traceMapKeys(m ssa.Value) {
	if f.seenKeys[m] {
		return
	}
	f.seenKeys[m] = true
	for _, ref := range referrers(m) {
		if r, ok := ref.(*ssa.MapUpdate); ok && r.Map == m {
			f.trace(r.Key)
		}
	}
	f.traceMapOrigin(m)
}

// traceMapOrigin follows the keys of the maps m is made from.
func (f *reasonFlow) traceMapOrigin(m ssa.Value) {
	switch m := m.(type) {
	case *ssa.Phi:
		for _, edge := range m.Edges {
			f.traceMapKeys(edge)
		}
	case *ssa.Parameter:
		f.traceCallers(m, f.traceMapKeys)
	case *ssa.Call:
		f.traceCallResult(m, 0, f.traceMapKeys)
	case *ssa.UnOp:
		// A package-level map is loaded from its global pointer. Follow the
		// load back to the global so its initialisers' MapUpdates are visited.
		if m.Op == token.MUL {
			f.traceMapKeys(m.X)
		}
	case *ssa.FieldAddr:
		f.traceFieldMapKeys(m.X.Type(), m.Field)
	case *ssa.Field:
		f.traceFieldMapKeys(m.X.Type(), m.Field)
	case *ssa.FreeVar:
		for _, bound := range f.boundTo(m) {
			f.traceMapKeys(bound)
		}
	case *ssa.Alloc:
		// Follow the values assigned to a local variable whose address is
		// taken, including a map captured by a closure.
		for _, ref := range referrers(m) {
			if store, ok := ref.(*ssa.Store); ok && store.Addr == m {
				f.traceMapKeys(store.Val)
			}
		}
	case *ssa.Global:
		for _, stored := range f.globalWrites[m] {
			f.traceMapKeys(stored)
		}
	}
}

// referrers are the instructions that use v.
func referrers(v ssa.Value) []ssa.Instruction {
	if refs := v.Referrers(); refs != nil {
		return *refs
	}
	return nil
}

// traceFieldMapKeys follows maps stored in a struct field.
func (f *reasonFlow) traceFieldMapKeys(typ types.Type, field int) {
	for _, stored := range f.fieldWrites[fieldOf(typ, field)] {
		f.traceMapKeys(stored)
	}
}

// traceWrites follows everything written into the map, slice or pointer v:
// a map's values (traceMapKeys follows keys), stores through it or an element
// of it, and what a closure or function it is handed stores through it.
func (f *reasonFlow) traceWrites(v ssa.Value) {
	for _, ref := range referrers(v) {
		f.traceWriteVia(v, ref)
	}
}

// traceWriteVia follows what the instruction ref, which uses v, writes into it.
func (f *reasonFlow) traceWriteVia(v ssa.Value, ref ssa.Instruction) {
	switch r := ref.(type) {
	case *ssa.MapUpdate:
		if r.Map == v {
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

// traceCallResult hands follow result i of call: what the package's callees
// return there, or, for a call out of the package, its arguments.
func (f *reasonFlow) traceCallResult(call *ssa.Call, i int, follow func(ssa.Value)) {
	callees := f.callees(&call.Call)
	if len(callees) == 0 {
		for _, arg := range call.Call.Args {
			follow(arg)
		}
		// Whatever a call out of the package does to its arguments, a string it
		// returns is not a named constant, even when every argument is.
		if isStringResult(call.Call.Signature().Results(), i) {
			f.problems[fmt.Sprintf("%s: the string result of an unresolved call reaches %s", f.fset.Position(call.Pos()), f.where)] = true
		}
		return
	}
	for _, callee := range callees {
		for _, block := range callee.Blocks {
			if ret, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return); ok && i < len(ret.Results) {
				follow(ret.Results[i])
			}
		}
	}
}

// isStringResult reports whether result i of results is a string.
func isStringResult(results *types.Tuple, i int) bool {
	if i < 0 || i >= results.Len() {
		return false
	}
	basic, ok := results.At(i).Type().Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// traceCallers hands follow the argument for parameter p of every call that
// can reach its function.
func (f *reasonFlow) traceCallers(p *ssa.Parameter, follow func(ssa.Value)) {
	fn := p.Parent()
	index := slices.Index(fn.Params, p)
	for _, call := range f.calls {
		common := call.Common()
		if !slices.Contains(f.callees(common), fn) {
			continue
		}
		if i := index - receiverShift(common, fn); i >= 0 && i < len(common.Args) {
			follow(common.Args[i])
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
		if len(fn.Blocks) > 0 && canCall(common, fn) {
			out = append(out, fn)
		}
	}
	return out
}

// canCall reports whether the dynamic call common can reach fn, a function with
// a body: an interface call reaches the methods of its name that implement the
// interface, a call through a func value the functions of its signature.
func canCall(common *ssa.CallCommon, fn *ssa.Function) bool {
	recv := fn.Signature.Recv()
	if common.IsInvoke() {
		return recv != nil && fn.Name() == common.Method.Name() &&
			types.Implements(recv.Type(), common.Value.Type().Underlying().(*types.Interface))
	}
	return recv == nil && types.Identical(fn.Signature, common.Signature())
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
// and anything else is a problem. SSA folds concatenations such as
// "Recon" + "ciled" into the same constant value, so the syntax-level
// spelling check must verify that a canonical value came from its named const.
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
