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
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// maxFeedEntries bounds how many <item>/<entry> elements a feed document
// decodes. The response-size cap bounds bytes, not elements: a 10MB body of
// empty <item/> elements would otherwise decode into well over a million
// ~280-byte structs, hundreds of MiB against a manager that runs several
// fetches at once. Real feeds publish a few dozen entries (archive and
// podcast feeds a few hundred); 1000 leaves ample headroom while keeping the
// decoded form to a few hundred KiB. The first entries in document order are
// the ones kept -- feeds list newest-first, and the controller only needs the
// newest entries -- so a longer feed is truncated to its most recent
// maxFeedEntries rather than rejected.
const maxFeedEntries = 1000

// maxCategories bounds how many categories one entry retains. Categories are
// only joined into a short display string; real entries carry a handful of
// tags, so the cap only matters for a hostile document.
const maxCategories = 50

type Entry struct {
	ID string
	// IDSource is the element ID was taken from: GUID, link, title, or
	// (for an entry with none of those) ContentID.
	IDSource IDSource
	// PreviousID and PreviousIDSource are the ID and source earlier releases
	// would have picked for an Atom entry whose choice has since changed (an
	// extension's <ext:id> or <ext:link> no longer stands in for Atom's own);
	// PreviousID is empty when nothing changed. Consumers use them to carry
	// stored identities over to ID.
	PreviousID       string
	PreviousIDSource IDSource
	Title            string
	Link             string
	Description      string
	// Content is the entry's full body when the feed carries one separately
	// from its summary: RSS <content:encoded>, or Atom <content> (which, for
	// Atom entries with no <summary>, is also used as Description). It is raw
	// markup like Description; consumers strip it before display.
	Content    string
	Image      string
	Author     string
	Categories []string
	Published  time.Time
	// FeedTitle is the title of the feed the entry came from. The parser
	// leaves it empty; the caller stamps it from FetchResult.FeedTitle so the
	// entry can be rendered without threading the feed's metadata alongside it.
	FeedTitle string
	// Seq is the entry's 0-based position in the feed document (0 = first
	// listed). RSS/Atom feeds list newest-first by convention, so a lower Seq
	// means more recent. It's the recency tiebreak used when Published is
	// missing or tied: many feeds omit per-entry dates entirely, and without a
	// fallback every such entry shares a zero timestamp and ordering collapses
	// to document order, which the consumer would otherwise read tail-first
	// (oldest) when picking the "most recent" entries.
	Seq int
}

// UnrecognizedFormatError is returned by parseFeed when the document's root
// element is none of the feed formats it understands (rss, feed, RDF). It's
// a distinct, permanent classification rather than a silent fall-through, so
// a feed in a format this parser doesn't support surfaces as a status
// condition instead of quietly posting zero entries forever.
type UnrecognizedFormatError struct {
	Root string
}

func (e *UnrecognizedFormatError) Error() string {
	return fmt.Sprintf("unrecognized feed format: root element <%s>", e.Root)
}

// ParseError is returned by parseFeed (and so by FetchEntries) for every
// failure to turn a 2xx response body into entries: malformed or truncated
// XML (Err is then an *xml.SyntaxError), an empty body (io.EOF), a declared
// charset that cannot be decoded, or a root element that is no known feed
// format (Err is then an *UnrecognizedFormatError, which errors.As still
// finds). The body is what the server sent, so retrying the same request
// won't fix it. Typing the whole family here keeps callers from matching
// encoding/xml's error types themselves.
type ParseError struct {
	Err error
}

func (e *ParseError) Error() string {
	return "parse feed: " + e.Err.Error()
}

func (e *ParseError) Unwrap() error { return e.Err }

// appendBounded decodes the element at start into a new T and appends it to s
// unless s already holds limit elements, in which case the element is skipped
// without being decoded. It is the shared body of the bounded slice types
// below, whose UnmarshalXML encoding/xml calls once per matching element.
func appendBounded[T any](d *xml.Decoder, start xml.StartElement, s []T, limit int) ([]T, error) {
	if len(s) >= limit {
		return s, d.Skip()
	}
	var v T
	if err := d.DecodeElement(&v, &start); err != nil {
		return s, err
	}
	return append(s, v), nil
}

// trimCategories trims each category and drops any that are blank, so
// whitespace-only <category> elements don't surface as empty entries.
func trimCategories(raw []string) []string {
	categories := make([]string, 0, len(raw))
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c != "" {
			categories = append(categories, c)
		}
	}
	return categories
}

// unmarshalXML decodes data into v using encoding/xml, with a CharsetReader
// so feeds that declare a non-UTF-8 encoding (ISO-8859-1/windows-1252 are
// common among older or non-English-language publishers) decode instead of
// failing outright; encoding/xml has no charset support of its own.
func unmarshalXML(data []byte, v any) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charset.NewReaderLabel
	return decoder.Decode(v)
}

// rootElementName returns the local name of data's outermost XML element
// ("rss" or "feed", typically), reading only as far as that first start tag
// rather than decoding the whole document. parseFeed uses this to pick
// parseRSS vs. parseAtom without paying for a full Unmarshal twice -- once
// here and again in whichever of those it then calls.
func rootElementName(data []byte) (string, error) {
	root, err := rootElement(data)
	return root.Local, err
}

// rootElement is rootElementName with the root's namespace too.
func rootElement(data []byte) (xml.Name, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charset.NewReaderLabel
	for {
		tok, err := decoder.Token()
		if err != nil {
			return xml.Name{}, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name, nil
		}
	}
}

// parseFeed parses an RSS 2.0, Atom or RSS 1.0 (RDF) document into its
// entries and the feed's own title ("" if it has none). Every failure is a
// *ParseError.
func parseFeed(data []byte) ([]Entry, string, error) {
	entries, title, err := parseFeedDocument(data)
	if err != nil {
		return nil, "", &ParseError{Err: err}
	}
	return entries, title, nil
}

func parseFeedDocument(data []byte) ([]Entry, string, error) {
	root, err := rootElementName(data)
	if err != nil {
		return nil, "", err
	}

	switch strings.ToLower(root) {
	case "rss":
		return parseRSS(data)
	case "feed":
		return parseAtom(data)
	case "rdf":
		return parseRDF(data)
	default:
		return nil, "", &UnrecognizedFormatError{Root: root}
	}
}
