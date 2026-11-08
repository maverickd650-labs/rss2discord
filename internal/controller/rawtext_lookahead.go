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
	"io"
	"strings"

	"golang.org/x/net/html"
)

// scriptLookaheadBudgetFactor bounds the bytes rawTextEndTagLookahead may scan
// for script end tags it doesn't find, as a multiple of the input length.
// Script misses can't be cached (see rawTextEndTagLookahead), so without a
// bound a prefixed body holding many unclosed <script> tags rescans the rest
// of the input once per tag: quadratic in a feed body that can reach 10MB.
const scriptLookaheadBudgetFactor = 4

// rawTextEndTagLookahead caches suffixes that contain no matching end tag.
// This is safe for the ordinary raw-text/RCDATA scanners, which search for
// their closing tag independently of prior content. Script data has escaped
// and double-escaped states, so a later suffix can have a matching end tag
// even when an earlier scan from the same input returned no match.
//
// Script misses are instead charged to scriptMissBytes, bounded by
// scriptLookaheadBudgetFactor. Hits need no budget: the tokenizer then
// consumes the same span the scan read. Once the budget is spent, script
// lookups report no end tag without scanning. That keeps the outer prefixed
// boundary visible while the nested body stays hidden; at worst, an outer end
// tag quoted inside a nested script exposes the rest of that body.
type rawTextEndTagLookahead struct {
	input           string
	noEndTags       map[tagName]struct{}
	scriptMissBytes int
}

// hasEndTagAt reports whether input after offset contains a matching end tag
// for tag according to the HTML tokenizer. Plaintext has no matching end tag.
func (l *rawTextEndTagLookahead) hasEndTagAt(offset int, tag tagName) bool {
	if tag == plaintextTagName || l.knownMissing(tag) {
		return false
	}
	suffix := l.input[offset:]
	if l.overBudget(tag, len(suffix)) {
		return false
	}
	if hasRawTextEndTag(strings.NewReader(suffix), tag) {
		return true
	}
	l.recordMiss(tag, len(suffix))
	return false
}

// knownMissing reports whether an earlier scan found no end tag for tag.
func (l *rawTextEndTagLookahead) knownMissing(tag tagName) bool {
	_, missing := l.noEndTags[tag]
	return missing
}

// overBudget reports whether scanning a suffix of the given length for a
// script end tag would exceed the script miss budget.
func (l *rawTextEndTagLookahead) overBudget(tag tagName, suffixLen int) bool {
	return tag == scriptTagName &&
		l.scriptMissBytes+suffixLen > scriptLookaheadBudgetFactor*len(l.input)
}

// recordMiss remembers a scan that found no end tag: script misses are charged
// to the budget, any other tag is cached.
func (l *rawTextEndTagLookahead) recordMiss(tag tagName, suffixLen int) {
	if tag == scriptTagName {
		l.scriptMissBytes += suffixLen
		return
	}
	if l.noEndTags == nil {
		l.noEndTags = make(map[tagName]struct{})
	}
	l.noEndTags[tag] = struct{}{}
}

// hasRawTextEndTag uses the HTML tokenizer to preserve its matching end-tag
// semantics. The synthetic opening tag and input suffix are streamed
// separately, avoiding a copy of the remaining input.
func hasRawTextEndTag(input io.Reader, tag tagName) bool {
	prefix := strings.NewReader("<" + string(tag) + ">")
	z := html.NewTokenizer(io.MultiReader(prefix, input))
	z.Next()
	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			return false
		}
		if tokenType == html.EndTagToken {
			name, _ := z.TagName()
			return tagName(name) == tag
		}
	}
}
