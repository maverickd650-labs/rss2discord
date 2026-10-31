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

package rss

import (
	"encoding/xml"
	"strings"

	"golang.org/x/net/html"
)

// nsText is an element's text plus the namespace it was declared in.
// encoding/xml matches a struct tag by local name regardless of namespace, so
// a plain string field for "link" would also be written by <atom:link> and
// "title" by <media:title> (last match wins), clobbering the real value.
// Collecting every match lets pickText choose by namespace instead.
type nsText struct {
	XMLName xml.Name
	Text    string `xml:",chardata"`
}

// maxNSCandidates caps how many namespaces an nsTexts field remembers. Real
// feeds carry one or two same-named elements per field; the cap only matters
// for a hostile document.
const maxNSCandidates = 8

// nsTexts collects the candidates for one field, but -- unlike a plain
// []nsText, which encoding/xml grows by one entry per matching element --
// keeps only the first non-blank value per namespace, up to maxNSCandidates.
// The 10MB response cap bounds bytes, not elements, so a single item with a
// million <title/> elements would otherwise retain tens of MB of candidates
// that pickText never looks at. pickText's result is unchanged: it only ever
// returns the first non-blank candidate of a namespace tier.
type nsTexts []nsText

// UnmarshalXML retains the first non-blank value per namespace, subject to
// nsTexts' retention limits. XHTML constructs retain raw inner markup only when
// they contain visible text; other constructs retain decoded character data.
// XML decoding errors are returned; blank or excess candidates are ignored.
func (c *nsTexts) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var el nsText
	if isXHTMLConstruct(start) {
		// An Atom type="xhtml" text construct wraps its value in a child
		// <div>, which chardata alone would drop. Keep the raw inner markup;
		// the controller's stripHTML turns it into plain text. Only xhtml is
		// read this way: innerxml of type="html" is still-escaped markup.
		var x struct {
			Inner string `xml:",innerxml"`
		}
		if err := d.DecodeElement(&x, &start); err != nil {
			return err
		}
		if !hasVisibleXHTMLText(x.Inner) {
			return nil
		}
		el = nsText{XMLName: start.Name, Text: x.Inner}
	} else if err := d.DecodeElement(&el, &start); err != nil {
		return err
	}
	el.Text = strings.TrimSpace(el.Text)
	if el.Text == "" {
		return nil
	}
	others := 0
	for _, existing := range *c {
		if existing.XMLName.Space == el.XMLName.Space {
			return nil
		}
		if !preferredNamespace(existing.XMLName.Space) {
			others++
		}
	}
	// Only the fallback tier is capped: a preferred namespace is always
	// admitted, so a run of extension namespaces ahead of it can't push out
	// the value pickText would choose.
	if !preferredNamespace(el.XMLName.Space) && others >= maxNSCandidates {
		return nil
	}
	*c = append(*c, el)
	return nil
}

// hasVisibleXHTMLText ignores empty wrappers, comments, and script/style
// bodies so a markup-only Atom summary doesn't mask usable content.
func hasVisibleXHTMLText(markup string) bool {
	z := html.NewTokenizer(strings.NewReader(markup))
	var skipUntil string
	for {
		switch tokenType := z.Next(); tokenType {
		case html.ErrorToken:
			return false
		case html.TextToken:
			if skipUntil == "" && strings.TrimSpace(string(z.Text())) != "" {
				return true
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if _, local, ok := strings.Cut(tag, ":"); ok {
				tag = local
			}
			if skipUntil != "" {
				if tokenType == html.EndTagToken && tag == skipUntil {
					skipUntil = ""
				}
			} else if tokenType == html.StartTagToken && (tag == "script" || tag == "style") {
				skipUntil = tag
			}
		}
	}
}

// isXHTMLConstruct reports whether start declares type="xhtml", trimming
// attribute whitespace and ignoring case. Attribute namespaces and the element's
// name are not checked.
func isXHTMLConstruct(start xml.StartElement) bool {
	for _, a := range start.Attr {
		if a.Name.Local == "type" && strings.EqualFold(strings.TrimSpace(a.Value), "xhtml") {
			return true
		}
	}
	return false
}

// preferredNamespace reports whether space is one pickText (or a caller's
// pickTextPrefer) ranks ahead of the any-namespace fallback.
func preferredNamespace(space string) bool {
	switch space {
	case "", rss10Namespace, atomNamespace, contentNamespace:
		return true
	}
	return false
}

// pickText returns the trimmed text of the most authoritative non-blank
// candidate: the unqualified RSS 2.0 element, then the RSS 1.0 namespace, then
// any other namespace (the lenient fallback for feeds that only carry e.g.
// <media:title>). Blank candidates, such as a self-closing <atom:link/>, are
// skipped so they can't mask a usable value in a lower tier.
func pickText(cands []nsText) string {
	return pickTextPrefer(cands, "", rss10Namespace)
}

// pickTextPrefer is pickText with the namespace tiers spelled out, for fields
// whose authoritative namespace isn't RSS 2.0's (Atom's own elements, the
// Content module's <content:encoded>). Candidates in a namespace outside
// spaces are still a last resort, so a feed that omits the declaration (or
// uses a lookalike) keeps working.
func pickTextPrefer(cands []nsText, spaces ...string) string {
	for _, space := range spaces {
		for _, c := range cands {
			if c.XMLName.Space != space {
				continue
			}
			if v := strings.TrimSpace(c.Text); v != "" {
				return v
			}
		}
	}

	for _, c := range cands {
		if v := strings.TrimSpace(c.Text); v != "" {
			return v
		}
	}

	return ""
}
