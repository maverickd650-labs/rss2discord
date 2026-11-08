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
	neturl "net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// schemeHTTP/schemeHTTPS are the only URL schemes normalizeIdentity and
// httpURLOrEmpty treat as a URL rather than opaque text.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Tag names stripHTML treats specially: script/style bodies are dropped, and
// plaintext is the one raw-text element that never has an end tag.
const (
	scriptTagName    = "script"
	styleTagName     = "style"
	textareaTagName  = "textarea"
	plaintextTagName = "plaintext"
)

// scriptLookaheadBudgetFactor bounds the bytes rawTextEndTagLookahead may scan
// for script end tags it doesn't find, as a multiple of the input length.
// Script misses can't be cached (see rawTextEndTagLookahead), so without a
// bound a prefixed body holding many unclosed <script> tags rescans the rest
// of the input once per tag: quadratic in a feed body that can reach 10MB.
const scriptLookaheadBudgetFactor = 4

// httpURLOrEmpty returns rawURL unchanged if it parses with an http or https
// scheme, or "" otherwise. It does not require a host or check reachability.
// Entry links/images come straight from feed XML, which is
// untrusted external content; Discord embeds (msg.Embed.URL/ThumbnailURL)
// are happy to carry through a javascript:/data: URI verbatim, so this keeps
// those out before they ever reach a user's client.
func httpURLOrEmpty(rawURL string) string {
	parsed, err := neturl.Parse(rawURL)
	if err != nil || !isHTTPScheme(parsed.Scheme) {
		return ""
	}
	return rawURL
}

// isHTTPScheme reports whether scheme is http or https.
func isHTTPScheme(scheme string) bool {
	return scheme == schemeHTTP || scheme == schemeHTTPS
}

// blankLineRegex collapses runs of 3+ newlines (left behind once tags are
// stripped) down to a single blank line.
var blankLineRegex = regexp.MustCompile(`\n{3,}`)

// continueReadingLinkRegex matches a trailing "Continue reading..." anchor,
// the boilerplate several feeds (e.g. the Guardian's) append to truncated
// descriptions. It's redundant in Discord: the embed title/message already
// links to the full article, so left in place it reads as a dead "read
// more" link with nowhere to click.
var continueReadingLinkRegex = regexp.MustCompile(`(?i)<a\b[^>]*>\s*continue reading\s*(\.{2,3}|…)?\s*</a>\s*(</[a-z0-9]+>\s*)*$`)

// continueReadingTextRegex is a fallback for feeds that ship the same
// boilerplate as plain text rather than wrapping it in an <a> tag.
var continueReadingTextRegex = regexp.MustCompile(`(?i)continue reading\s*(\.{2,3}|…)?\s*$`)

// stripHTML converts an RSS/Atom entry's title or description into
// Discord-friendly plain text. Many feeds (e.g. the Guardian's) ship these
// as raw or escaped HTML, which Discord otherwise renders as literal tag
// soup.
//
// Markup is removed with the HTML tokenizer rather than a regex so that a
// ">" inside a quoted attribute or comment, or a bare "<" in prose, can't
// derail tag detection, and so script/style bodies are dropped rather than
// leaked as text. Block-level tags (p, li, br, div, h1-h6) become newlines
// so paragraph and list structure survives.
func stripHTML(input string) string {
	input = continueReadingLinkRegex.ReplaceAllString(input, "")

	st := &htmlStripper{
		lookahead: rawTextEndTagLookahead{input: input},
		z:         html.NewTokenizer(strings.NewReader(input)),
	}
	st.sb.Grow(len(input))
	for {
		tokenType := st.z.Next()
		st.offset += len(st.z.Raw())
		switch tokenType {
		case html.ErrorToken:
			return finishStrippedText(st.sb.String())
		case html.TextToken:
			if !st.skip.active() {
				st.sb.Write(st.z.Text())
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			st.tag(tokenType)
		}
	}
}

// skipState tracks a script/style element whose body stripHTML is dropping.
type skipState struct {
	until    string // qualified name; non-empty while inside a <script>/<style> element
	depth    int    // open until elements; the body ends when it returns to 0
	prefixed bool   // the outer script/style element has a namespace prefix
}

func (s *skipState) active() bool { return s.until != "" }

// begin starts skipping the body of the script/style start tag. It keeps the
// full tag name so only its matching qualified end tag can terminate a
// prefixed Atom XHTML body.
func (s *skipState) begin(tag, localTag string) {
	s.until = tag
	s.depth = 1
	s.prefixed = tag != localTag
}

// observe counts nested starts of the same qualified name, so an inner end tag
// doesn't expose the rest of the outer body, and ends the skip when the outer
// end tag arrives. </script> must not close <x:script>, hence the exact match.
func (s *skipState) observe(tag string, tokenType html.TokenType) {
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
	tag := string(name)
	// Atom XHTML may use prefixes inherited from the feed element.
	_, localTag, ok := strings.Cut(tag, ":")
	if !ok {
		localTag = tag
	}
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
			st.skip.begin(tag, localTag)
		}
	case isBlockTag(localTag):
		st.sb.WriteByte('\n')
	}
}

// skipTag handles a tag seen while dropping a script/style body.
func (st *htmlStripper) skipTag(tag string, tokenType html.TokenType) {
	// A nested raw-text/RCDATA element can contain the outer
	// qualified end tag as text. Let the tokenizer consume it
	// normally when it has a matching end tag; for an unclosed
	// element (or plaintext), keep the outer boundary visible.
	if st.skip.prefixed && tokenType == html.StartTagToken &&
		isRawTextElement(tag) && !st.lookahead.hasEndTagAt(st.offset, tag) {
		st.z.NextIsNotRawText()
	}
	st.skip.observe(tag, tokenType)
}

// isSkippedElement reports whether stripHTML drops the body of the element.
func isSkippedElement(localTag string) bool {
	return localTag == scriptTagName || localTag == styleTagName
}

// isRawTextElement reports whether the HTML tokenizer treats the next
// token after a start tag as raw text or RCDATA.
func isRawTextElement(tag string) bool {
	return slices.Contains(rawTextElements, tag)
}

var rawTextElements = []string{
	"iframe", "noembed", "noframes", "noscript", plaintextTagName,
	scriptTagName, styleTagName, textareaTagName, "title", "xmp",
}

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
	noEndTags       map[string]struct{}
	scriptMissBytes int
}

// hasEndTagAt reports whether input after offset contains a matching end tag
// for tag according to the HTML tokenizer. Plaintext has no matching end tag.
func (l *rawTextEndTagLookahead) hasEndTagAt(offset int, tag string) bool {
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
func (l *rawTextEndTagLookahead) knownMissing(tag string) bool {
	_, missing := l.noEndTags[tag]
	return missing
}

// overBudget reports whether scanning a suffix of the given length for a
// script end tag would exceed the script miss budget.
func (l *rawTextEndTagLookahead) overBudget(tag string, suffixLen int) bool {
	return tag == scriptTagName &&
		l.scriptMissBytes+suffixLen > scriptLookaheadBudgetFactor*len(l.input)
}

// recordMiss remembers a scan that found no end tag: script misses are charged
// to the budget, any other tag is cached.
func (l *rawTextEndTagLookahead) recordMiss(tag string, suffixLen int) {
	if tag == scriptTagName {
		l.scriptMissBytes += suffixLen
		return
	}
	if l.noEndTags == nil {
		l.noEndTags = make(map[string]struct{})
	}
	l.noEndTags[tag] = struct{}{}
}

// hasRawTextEndTag uses the HTML tokenizer to preserve its matching end-tag
// semantics. The synthetic opening tag and input suffix are streamed
// separately, avoiding a copy of the remaining input.
func hasRawTextEndTag(input io.Reader, tag string) bool {
	prefix := strings.NewReader("<" + tag + ">")
	z := html.NewTokenizer(io.MultiReader(prefix, input))
	z.Next()
	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			return false
		}
		if tokenType == html.EndTagToken {
			name, _ := z.TagName()
			return string(name) == tag
		}
	}
}

// isBlockTag reports whether tag delimits block-level content (or a line
// break), which stripHTML converts to a newline.
func isBlockTag(tag string) bool {
	return slices.Contains(blockTags, tag)
}

var blockTags = []string{"p", "li", "br", "div", "h1", "h2", "h3", "h4", "h5", "h6"}

// finishStrippedText normalizes whitespace in text left after tag removal:
// trims each line, collapses runs of blank lines, and drops a trailing
// plain-text "Continue reading" stub.
func finishStrippedText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	text = blankLineRegex.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	text = continueReadingTextRegex.ReplaceAllString(strings.TrimSpace(text), "")

	return strings.TrimSpace(text)
}
