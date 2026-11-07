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

// Package msgtemplate is the sandbox for user-supplied Discord message
// templates (a FeedGroup's format, embed descriptionFormat and
// forumThreadName). Templates are untrusted input, so they only reach the rest
// of the operator as an opaque *Template that Compile has checked and whose
// Execute is size-capped; there is no way to parse a template without the
// construct checks or to run one without the output cap.
//
// Compile rejects the constructs that let a template consume unbounded CPU or
// memory: range, define/template/block, with, variable declaration and
// assignment, and the string-building text/template builtins (printf, print,
// println, html, js, urlquery, call), which combined with value reuse can grow
// memory exponentially. if/else, field access, pipelines and the helpers in
// funcs.go remain available. Execute writes into a buffer that fails past
// maxTemplateOutputBytes, and the replace helper bounds its own result, so even
// intermediate pipeline values stay small.
//
// This is the only production package allowed to import text/template (or
// text/template/parse, html/template); TestOnlyTheSandboxImportsTextTemplate
// fails the build of any other.
package msgtemplate

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"text/template/parse"
)

// Template is a compiled, sandbox-checked message template. Build one with
// Compile; the zero value is not usable.
type Template struct {
	tmpl *template.Template
}

// Compile parses text as a message template, falling back to fallback when
// text is blank. Leading and trailing whitespace is removed from text. Parse
// errors, including unknown helper names, and use of an unsupported construct
// are returned. The fallback is checked like any other text.
func Compile(name, text, fallback string) (*Template, error) {
	text = cmp.Or(strings.TrimSpace(text), fallback)
	tmpl, err := template.New(name).Funcs(templateFuncs).Parse(text)
	if err != nil {
		return nil, err
	}
	if err := rejectUnboundedConstructs(tmpl); err != nil {
		return nil, err
	}
	return &Template{tmpl: tmpl}, nil
}

// Execute runs the template against data and returns the raw output. It is not
// clamped to Discord's limits (that is the caller's job), but its size is
// bounded by limitedBuffer (maxTemplateOutputBytes). Execution errors,
// including helper errors and output overflow, are returned with empty text.
func (t *Template) Execute(data any) (string, error) {
	var buf limitedBuffer
	if err := t.tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// maxTemplateOutputBytes bounds what one template execution may write, well
// above Discord's limits (2000 message / 4096 embed runes).
const maxTemplateOutputBytes = 1024 * 1024

// growthBuiltins are text/template builtins that build a new string from
// their arguments. Combined with repeated reuse of a value they can grow
// memory exponentially before anything is written to the output buffer, so
// they are not available; the helpers in templateFuncs cover real needs.
var growthBuiltins = map[string]bool{
	"printf": true, "print": true, "println": true,
	"html": true, "js": true, "urlquery": true, "call": true,
}

// rejectUnboundedConstructs refuses the template features that let a
// user-supplied template consume unbounded CPU or memory:
//   - range (including over an integer, e.g. {{range 100000000000}}{{end}},
//     which spins the reconcile worker forever) and define/template
//     (recursion);
//   - variable declarations/assignments and {{with}} (which rebinds dot), the
//     two ways to reuse a value so that a chain of string-building calls
//     doubles it each step (e.g. {{$x = printf "%s%s" $x $x}} repeated);
//   - the string-building builtins in growthBuiltins.
//
// Templates only interpolate a handful of strings; {{if}}/{{else}}, field
// access, pipelines and the helpers in templateFuncs remain available.
func rejectUnboundedConstructs(tmpl *template.Template) error {
	if len(tmpl.Templates()) > 1 {
		return errors.New("template: {{define}} is not supported")
	}
	if tmpl.Tree == nil {
		return nil
	}
	return checkNode(tmpl.Root)
}

// checkNode walks a parsed template and returns the first unsupported
// construct. It only dispatches; each node kind with real logic has its own
// helper. Nodes that need no check (text, fields, literals and variables,
// where only $ -- the root value -- can appear without a declaration) fall
// through to nil.
func checkNode(node parse.Node) error {
	switch n := node.(type) {
	case *parse.ListNode:
		return checkList(n)
	case *parse.ActionNode:
		return checkNode(n.Pipe)
	case *parse.IfNode:
		return checkBranch(&n.BranchNode)
	case *parse.PipeNode:
		return checkPipe(n)
	case *parse.ChainNode:
		return checkNode(n.Node)
	case *parse.IdentifierNode:
		return checkIdentifier(n)
	}
	// Reached only by non-nil nodes, so Type is safe to call here.
	return unsupportedNodes[node.Type()]
}

// unsupportedNodes maps the node types that are refused outright to the error
// reported for them; node types absent from it are allowed.
var unsupportedNodes = map[parse.NodeType]error{
	parse.NodeRange:    errors.New("template: {{range}} is not supported"),
	parse.NodeWith:     errors.New("template: {{with}} is not supported"),
	parse.NodeTemplate: errors.New("template: {{template}} is not supported"),
}

var errDeclUnsupported = errors.New("template: variable declaration and assignment are not supported")

// checkList checks each node of a (possibly nil) list in order.
func checkList(l *parse.ListNode) error {
	if l == nil {
		return nil
	}
	for _, c := range l.Nodes {
		if err := checkNode(c); err != nil {
			return err
		}
	}
	return nil
}

// checkPipe refuses variable declarations and checks every command argument of
// a (possibly nil) pipeline.
func checkPipe(p *parse.PipeNode) error {
	if p == nil {
		return nil
	}
	if len(p.Decl) > 0 {
		return errDeclUnsupported
	}
	for _, cmd := range p.Cmds {
		for _, arg := range cmd.Args {
			if err := checkNode(arg); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkIdentifier refuses the string-building builtins.
func checkIdentifier(id *parse.IdentifierNode) error {
	if growthBuiltins[id.Ident] {
		return fmt.Errorf("template: %s is not supported", id.Ident)
	}
	return nil
}

func checkBranch(b *parse.BranchNode) error {
	if err := checkNode(b.Pipe); err != nil {
		return err
	}
	if err := checkNode(b.List); err != nil {
		return err
	}
	return checkNode(b.ElseList)
}

// limitedBuffer is a bytes.Buffer that fails once it would exceed
// maxTemplateOutputBytes, as defense in depth behind rejectUnboundedConstructs.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxTemplateOutputBytes {
		return 0, fmt.Errorf("template output exceeds maximum size of %d bytes", maxTemplateOutputBytes)
	}
	return b.Buffer.Write(p)
}
