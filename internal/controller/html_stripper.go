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
	"strings"

	"golang.org/x/net/html"
)

// skipState tracks a script/style element whose body stripHTML is dropping.
type skipState struct {
	until    tagName // qualified name; non-empty while inside a <script>/<style> element
	depth    int     // open until elements; the body ends when it returns to 0
	prefixed bool    // the outer script/style element has a namespace prefix
}

func (s *skipState) active() bool { return s.until != "" }

// begin starts skipping the body of the script/style start tag. It keeps the
// full tag name so only its matching qualified end tag can terminate a
// prefixed Atom XHTML body.
func (s *skipState) begin(tag tagName) {
	s.until = tag
	s.depth = 1
	s.prefixed = tag != tag.local()
}

// observe counts nested starts of the same qualified name, so an inner end tag
// doesn't expose the rest of the outer body, and ends the skip when the outer
// end tag arrives. </script> must not close <x:script>, hence the exact match.
func (s *skipState) observe(tag tagName, tokenType html.TokenType) {
	if tag != s.until {
		return
	}
	switch tokenType {
	case html.StartTagToken:
		s.depth++
	case html.EndTagToken:
		if s.depth--; s.depth == 0 {
			*s = skipState{}
		}
	}
}

// htmlStripper is the state of one stripHTML pass over the tokenizer.
type htmlStripper struct {
	sb        strings.Builder
	skip      skipState
	lookahead rawTextEndTagLookahead
	z         *html.Tokenizer
	offset    int // bytes of input the tokenizer has consumed
}

// tag handles one start, self-closing or end tag token.
func (st *htmlStripper) tag(tokenType html.TokenType) {
	name, _ := st.z.TagName()
	tag := tagName(name)
	// Atom XHTML may use prefixes inherited from the feed element.
	localTag := tag.local()
	// A self-closing <script/> or <style/> has no body, but the
	// tokenizer still enters raw-text mode after it and would swallow
	// the following markup (including an outer end tag) as text.
	if tokenType == html.SelfClosingTagToken {
		st.z.NextIsNotRawText()
	}
	switch {
	case st.skip.active():
		st.skipTag(tag, tokenType)
	case isSkippedElement(localTag):
		if tokenType == html.StartTagToken {
			st.skip.begin(tag)
		}
	case isBlockTag(localTag):
		st.sb.WriteByte('\n')
	}
}

// skipTag handles a tag seen while dropping a script/style body.
func (st *htmlStripper) skipTag(tag tagName, tokenType html.TokenType) {
	// A nested raw-text/RCDATA element can contain the outer
	// qualified end tag as text. Let the tokenizer consume it
	// normally when it has a matching end tag; for an unclosed
	// element (or plaintext), keep the outer boundary visible.
	if st.hidesOuterBoundary(tag, tokenType) {
		st.z.NextIsNotRawText()
	}
	st.skip.observe(tag, tokenType)
}

// hidesOuterBoundary reports whether the tokenizer would treat a nested
// raw-text start tag as swallowing the rest of a prefixed outer body, because
// it has no matching end tag of its own.
func (st *htmlStripper) hidesOuterBoundary(tag tagName, tokenType html.TokenType) bool {
	if !st.skip.prefixed {
		return false
	}
	if !startsRawTextElement(tag, tokenType) {
		return false
	}
	return !st.lookahead.hasEndTagAt(st.offset, tag)
}

// startsRawTextElement reports whether the token opens a raw-text/RCDATA
// element.
func startsRawTextElement(tag tagName, tokenType html.TokenType) bool {
	return tokenType == html.StartTagToken && isRawTextElement(tag)
}
