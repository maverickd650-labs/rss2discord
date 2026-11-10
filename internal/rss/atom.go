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
	"cmp"
	"encoding/xml"
	"net/url"
	"slices"
	"strings"
)

// atomNamespace is Atom's element namespace, declared as the default
// namespace on a conforming <feed>.
const atomNamespace = "http://www.w3.org/2005/Atom"

// atomEntryCore is everything an Atom entry carries that doesn't decide its
// identity, link or dates. The fields that do are declared per vocabulary (see
// atomEntry and looseAtomEntry).
type atomEntryCore struct {
	// Title, Summary and Content are nsTexts: Content because a direct
	// <media:content> on the entry shares its local name (see atomText), and
	// all three so type="xhtml" values (a child <div>) are captured; see
	// nsTexts.UnmarshalXML.
	Title      nsTexts        `xml:"title"`
	Summary    nsTexts        `xml:"summary"`
	Content    nsTexts        `xml:"content"`
	Author     atomAuthor     `xml:"author"`
	Categories atomCategories `xml:"category"`
	// MediaThumbnail and MediaGroup carry Media RSS images/descriptions
	// (YouTube feeds nest them in <media:group>). Only <thumbnail> and
	// <group> are read directly: Atom has no elements by those names, whereas
	// a direct <media:content>/<media:title>/<media:description> would share
	// a local name with Content/Title and clobber them, so those are read
	// only from inside the group (see mediaGroup).
	MediaThumbnail *rssMediaURL `xml:"thumbnail"`
	MediaGroup     *mediaGroup  `xml:"group"`
	// Base is xml:base, an entry-level override of the feed's Base for
	// resolving this entry's relative link/enclosure hrefs. encoding/xml
	// matches "base,attr" by local name regardless of namespace prefix, so
	// this also covers the attribute written without an "xml:" prefix.
	Base string `xml:"base,attr"`
}

// An Atom document's vocabulary is decided once, from its root element's
// namespace. encoding/xml matches a plain struct tag by local name in any
// namespace, so in a document that mixes vocabularies an extension's <ext:id>
// or <ext:link> would otherwise overwrite Atom's own. In an Atom 1.0 document
// (atomEntry) the identity, link and date elements are therefore matched by
// Atom's namespace, and extension elements never reach them. Any other
// document -- Atom 0.3, or one that omits the declaration -- has no namespace
// to match against and is read by local name alone (looseAtomEntry).

// atomEntry is an entry of an Atom 1.0 document.
type atomEntry struct {
	atomEntryCore
	ID        string    `xml:"http://www.w3.org/2005/Atom id"`
	Links     atomLinks `xml:"http://www.w3.org/2005/Atom link"`
	Updated   string    `xml:"http://www.w3.org/2005/Atom updated"`
	Published string    `xml:"http://www.w3.org/2005/Atom published"`
}

// looseAtomEntry is an entry of any other Atom document; see atomEntry.
type looseAtomEntry struct {
	atomEntryCore
	ID        string    `xml:"id"`
	Links     atomLinks `xml:"link"`
	Updated   string    `xml:"updated"`
	Published string    `xml:"published"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

// atomCategories collects an entry's <category term="..."/> elements, keeping
// the trimmed text of the first maxCategories non-blank terms.
type atomCategories []string

// UnmarshalXML retains the trimmed attribute value of non-blank category terms up
// to maxCategories. XML decoding errors are returned; blank or excess categories
// are ignored.
func (c *atomCategories) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var cat struct {
		Term string `xml:"term,attr"`
	}
	if err := d.DecodeElement(&cat, &start); err != nil {
		return err
	}
	if term := strings.TrimSpace(cat.Term); term != "" && len(*c) < maxCategories {
		*c = append(*c, term)
	}
	return nil
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

// isAlternate reports whether l is an article link: Atom defaults an unmarked
// link's rel to "alternate".
func (l atomLink) isAlternate() bool {
	return l.Rel == "" || l.Rel == "alternate"
}

// isImageEnclosure reports whether l is an image enclosure with a non-blank
// href.
func (l atomLink) isImageEnclosure() bool {
	return l.Rel == "enclosure" && strings.HasPrefix(l.Type, "image/") && strings.TrimSpace(l.Href) != ""
}

// atomLinks collects an entry's <link> elements but retains only the three
// that primaryLink and enclosureImage can select: the first link overall, the
// first alternate link, and the first image enclosure. Entries rarely carry
// more than a handful of links, but nothing else bounds them, and the retained
// subset yields exactly the same selection as the full list.
type atomLinks []atomLink

// UnmarshalXML retains a link only if it is the first, the first alternate, or
// the first image enclosure. XML decoding errors are returned; other links are
// ignored.
func (c *atomLinks) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var l atomLink
	if err := d.DecodeElement(&l, &start); err != nil {
		return err
	}
	if c.retains(l) {
		*c = append(*c, l)
	}
	return nil
}

// retains reports whether l is worth keeping beside the links already kept: it
// is the first link, or the first alternate, or the first image enclosure.
func (c atomLinks) retains(l atomLink) bool {
	if len(c) == 0 {
		return true
	}
	hasAlternate := slices.ContainsFunc(c, atomLink.isAlternate)
	hasEnclosure := slices.ContainsFunc(c, atomLink.isImageEnclosure)
	return (!hasAlternate && l.isAlternate()) || (!hasEnclosure && l.isImageEnclosure())
}

// primaryLink returns the entry's main article link: the "alternate" rel,
// or the first link if none is explicitly marked alternate (Atom defaults
// an unmarked link's rel to "alternate").
func primaryLink(links []atomLink) string {
	for _, l := range links {
		if l.isAlternate() {
			return l.Href
		}
	}
	if len(links) > 0 {
		return links[0].Href
	}
	return ""
}

// enclosureImage returns an Atom entry's image enclosure link, if any
// (<link rel="enclosure" type="image/..">).
func enclosureImage(links []atomLink) string {
	for _, l := range links {
		if l.isImageEnclosure() {
			return strings.TrimSpace(l.Href)
		}
	}
	return ""
}

// atomFeed is an Atom 1.0 document; see atomEntry.
type atomFeed struct {
	// Title is a slice of nsText so a feed-level <media:title> or
	// <itunes:title> can't override Atom's own <title>; see atomText.
	Title   nsTexts              `xml:"title"`
	Entries entryList[atomEntry] `xml:"http://www.w3.org/2005/Atom entry"`
	// Base is the feed-level xml:base, used to resolve an entry's relative
	// link/enclosure hrefs when the entry doesn't set its own.
	Base string `xml:"base,attr"`
}

// looseAtomFeed is any other Atom document; see looseAtomEntry.
type looseAtomFeed struct {
	Title   nsTexts                   `xml:"title"`
	Entries entryList[looseAtomEntry] `xml:"entry"`
	Base    string                    `xml:"base,attr"`
}

// resolveAtomURL resolves ref against base per RFC 3986. Atom commonly
// supplies relative link hrefs (a bare path/slug) alongside an xml:base
// rather than a full URL; without resolving against it, the relative
// string is neither a usable link nor recognized as a URL by anything
// downstream (e.g. httpURLOrEmpty, or entry-identity normalization) that
// expects an absolute one. If ref is already absolute, or base is
// empty/unparseable, ref is returned unchanged.
func resolveAtomURL(base, ref string) string {
	if ref == "" || base == "" {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil || refURL.IsAbs() {
		return ref
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

// atomText picks the Atom-namespaced value among same-named candidates,
// falling back to un-namespaced (feeds that omit xmlns) and then any other
// namespace.
func atomText(cands []nsText) string {
	return pickTextPrefer(cands, atomNamespace, "")
}

// atomItem is an entry of either Atom vocabulary: the identity, link and date
// elements as the document's vocabulary found them, plus everything else.
type atomItem struct {
	atomEntryCore
	ID, Updated, Published string
	Links                  atomLinks
}

// effectiveBase is the xml:base this entry's relative hrefs resolve against.
// An entry's xml:base is itself resolved against the feed's (RFC 3986 section
// 5.1.2), so a relative entry base such as "posts/" extends the feed base
// instead of replacing it and leaving relative links relative.
func (item atomItem) effectiveBase(feedBase string) string {
	if entryBase := strings.TrimSpace(item.Base); entryBase != "" {
		return resolveAtomURL(feedBase, entryBase)
	}
	return feedBase
}

// item is the vocabulary-neutral form of an Atom 1.0 entry.
func (e atomEntry) item() atomItem {
	return atomItem{atomEntryCore: e.atomEntryCore, ID: e.ID, Links: e.Links, Updated: e.Updated, Published: e.Published}
}

// item is the vocabulary-neutral form of a loosely read Atom entry.
func (e looseAtomEntry) item() atomItem {
	return atomItem{atomEntryCore: e.atomEntryCore, ID: e.ID, Links: e.Links, Updated: e.Updated, Published: e.Published}
}

// atomItemsOf converts one vocabulary's entries into atomItems, in order.
func atomItemsOf[E interface{ item() atomItem }](entries []E) []atomItem {
	items := make([]atomItem, len(entries))
	for i, e := range entries {
		items[i] = e.item()
	}
	return items
}

// decodeAtomItems decodes data into its atomItems, feed title, and feedBase
// attribute, selecting strict Atom 1.0 or loose Atom based on whether rootSpace
// matches atomNamespace.
func decodeAtomItems(data []byte, rootSpace string) ([]atomItem, nsTexts, string, error) {
	if rootSpace == atomNamespace {
		var feed atomFeed
		if err := unmarshalXML(data, &feed); err != nil {
			return nil, nil, "", err
		}
		return atomItemsOf(feed.Entries), feed.Title, strings.TrimSpace(feed.Base), nil
	}
	var feed looseAtomFeed
	if err := unmarshalXML(data, &feed); err != nil {
		return nil, nil, "", err
	}
	return atomItemsOf(feed.Entries), feed.Title, strings.TrimSpace(feed.Base), nil
}

// atomItemsToEntries converts Atom items in document order into Entries.
func atomItemsToEntries(items []atomItem, feedBase string) []Entry {
	entries := make([]Entry, 0, len(items))
	for i, item := range items {
		base := item.effectiveBase(feedBase)

		link := resolveAtomURL(base, primaryLink(item.Links))
		id, idSource := pickIdentity(item.ID, link, atomText(item.Title))

		// Published falls back to Updated when it is missing or invalid.
		published, _ := parseTime(item.Published)
		if published.IsZero() {
			published, _ = parseTime(item.Updated)
		}

		// The image falls back from an enclosure link to Media RSS (direct
		// thumbnail, then the group), resolved against base.
		image := cmp.Or(enclosureImage(item.Links), mediaImage(item.MediaThumbnail, nil, item.MediaGroup))
		image = strings.TrimSpace(resolveAtomURL(base, image))

		// Content is exposed even when it doubles as Description (no
		// summary), so templates can use {{.Content}} regardless of which
		// elements a given feed fills in.
		content := atomText(item.Content)
		description := cmp.Or(atomText(item.Summary), content, item.MediaGroup.description())

		entry := Entry{
			ID:          id,
			IDSource:    idSource,
			Title:       atomText(item.Title),
			Link:        strings.TrimSpace(link),
			Description: description,
			Content:     content,
			Image:       image,
			Author:      strings.TrimSpace(item.Author.Name),
			Categories:  item.Categories,
			Published:   published,
			Seq:         i,
		}
		entries = append(entries, withContentID(entry))
	}
	return entries
}

// parseAtom returns entries in document order and the feed title, or nil entries
// and an empty title on an XML decoding error. Identity falls back from ID to
// resolved link to title to ContentID. XHTML
// text constructs retain their inner markup. Description falls back from summary
// to content to the Media RSS group description. Invalid or missing dates become
// zero times after trying published and then updated.
func parseAtom(data []byte, rootSpace string) ([]Entry, string, error) {
	items, title, feedBase, err := decodeAtomItems(data, rootSpace)
	if err != nil {
		return nil, "", err
	}

	return atomItemsToEntries(items, feedBase), atomText(title), nil
}
