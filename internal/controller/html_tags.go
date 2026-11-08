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
	"slices"
	"strings"
)

// tagName is an HTML tag name as the tokenizer reports it: lower-cased and,
// in Atom XHTML, possibly namespace-prefixed ("x:script").
type tagName string

// local returns the tag name without any namespace prefix.
func (t tagName) local() tagName {
	_, local, ok := strings.Cut(string(t), ":")
	if !ok {
		return t
	}
	return tagName(local)
}

// Tag names stripHTML treats specially: script/style bodies are dropped, and
// plaintext is the one raw-text element that never has an end tag.
const (
	scriptTagName    = "script"
	styleTagName     = "style"
	textareaTagName  = "textarea"
	plaintextTagName = "plaintext"
)

// isSkippedElement reports whether stripHTML drops the body of the element.
func isSkippedElement(localTag tagName) bool {
	return localTag == scriptTagName || localTag == styleTagName
}

// isRawTextElement reports whether the HTML tokenizer treats the next
// token after a start tag as raw text or RCDATA.
func isRawTextElement(tag tagName) bool {
	return slices.Contains(rawTextElements, tag)
}

var rawTextElements = []tagName{
	"iframe", "noembed", "noframes", "noscript", plaintextTagName,
	scriptTagName, styleTagName, textareaTagName, "title", "xmp",
}

// isBlockTag reports whether tag delimits block-level content (or a line
// break), which stripHTML converts to a newline.
func isBlockTag(tag tagName) bool {
	return slices.Contains(blockTags, tag)
}

var blockTags = []tagName{"p", "li", "br", "div", "h1", "h2", "h3", "h4", "h5", "h6"}
