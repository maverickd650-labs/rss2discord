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
)

type rssEnvelope struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	// Title is a slice of nsText for the same reason as rssItem's fields: a
	// channel-level <itunes:title> or <media:title> shares the local name and
	// would otherwise override the real <title>.
	Title nsTexts  `xml:"title"`
	Items rssItems `xml:"item"`
}

// rdfEnvelope is RSS 1.0's document shape: root <rdf:RDF>, with <item>
// elements listed as top-level siblings of <channel> rather than nested
// inside it (unlike RSS 2.0's rssChannel.Items). Items reuse the rssItem
// mapping -- RDF's title/link/description/dc:creator/dc:date fields are the
// same local names RSS 2.0 uses (dc:creator and dc:date match rssItem's
// Creator and DCDate regardless of namespace prefix).
type rdfEnvelope struct {
	XMLName xml.Name   `xml:"RDF"`
	Channel rdfChannel `xml:"channel"`
	Items   rssItems   `xml:"item"`
}

// rdfChannel is RSS 1.0's <channel>, which carries feed metadata only; its
// items are siblings (see rdfEnvelope).
type rdfChannel struct {
	Title nsTexts `xml:"title"`
}

// rss10Namespace is RSS 1.0's element namespace. RDF items declare it as
// their default namespace, so their title/link/description carry it rather
// than the empty namespace RSS 2.0 uses.
const rss10Namespace = "http://purl.org/rss/1.0/"

// contentNamespace is the RSS 1.0 Content module's namespace, home of
// <content:encoded>.
const contentNamespace = "http://purl.org/rss/1.0/modules/content/"

// rssItems collects a document's <item> elements but, unlike a plain
// []rssItem, stops decoding after maxFeedEntries: later items are skipped
// rather than allocated. Document order is kept, so Entry.Seq is unaffected.
type rssItems []rssItem

// UnmarshalXML retains the first maxFeedEntries items. XML decoding errors are
// returned; excess items are ignored.
func (c *rssItems) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var err error
	*c, err = appendBounded(d, start, *c, maxFeedEntries)
	return err
}

type rssItem struct {
	// These are nsTexts, not strings, so namespaced extension elements with
	// the same local name can't override the core fields; see nsText and
	// pickText.
	Title       nsTexts `xml:"title"`
	Link        nsTexts `xml:"link"`
	Description nsTexts `xml:"description"`
	GUID        nsTexts `xml:"guid"`
	PubDate     nsTexts `xml:"pubDate"`
	Author      nsTexts `xml:"author"`
	// ContentEncoded is <content:encoded>, the full post body many RSS feeds
	// (WordPress, Ghost, ...) carry alongside a short <description>. Matched
	// by local name, so it needs the same namespace-aware pick as above.
	ContentEncoded nsTexts `xml:"encoded"`
	// Creator matches Dublin Core's <dc:creator>, the de facto author field
	// for feeds (e.g. WordPress) that don't use RSS's own <author>.
	// encoding/xml matches by local name regardless of namespace prefix, so
	// this also covers <creator> without a dc: prefix.
	Creator        string        `xml:"creator"`
	Categories     rssCategories `xml:"category"`
	Enclosure      *rssEnclosure `xml:"enclosure"`
	MediaThumbnail *rssMediaURL  `xml:"thumbnail"`
	MediaContent   mediaContents `xml:"content"`
	// MediaGroup is <media:group>, which some feeds wrap their thumbnail,
	// content and description in; see mediaGroup.
	MediaGroup *mediaGroup `xml:"group"`
	// DCDate matches Dublin Core's <dc:date>, RDF's (RSS 1.0) date field --
	// RDF items carry no <pubDate>. encoding/xml matches by local name
	// regardless of namespace prefix, so this also covers <date> without a
	// dc: prefix. Value is W3CDTF, a profile of RFC3339 that parseTime
	// already handles.
	DCDate string `xml:"date"`
}

// rssEnclosure is RSS's standard <enclosure url=".." type="image/..">,
// commonly used to attach a lead image to an item.
type rssEnclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// rssMediaURL covers the Media RSS namespace's <media:thumbnail> and
// <media:content>, both of which carry a url attribute and (for content)
// optional medium/type attributes identifying it as an image. encoding/xml
// matches by local name regardless of namespace prefix, so this also
// matches Atom's namespaced equivalents if a feed mixes vocabularies.
type rssMediaURL struct {
	URL    string `xml:"url,attr"`
	Medium string `xml:"medium,attr"`
	Type   string `xml:"type,attr"`
}

func (m rssMediaURL) isImage() bool {
	return strings.HasPrefix(m.Medium, "image") || strings.HasPrefix(m.Type, "image/")
}

// usableImage reports whether m is an image with a non-blank url, the only
// kind of <media:content> mediaImage ever selects.
func (m rssMediaURL) usableImage() bool {
	return m.isImage() && strings.TrimSpace(m.URL) != ""
}

// mediaContents collects an item's direct <media:content> elements but keeps
// only the first usable image: mediaImage never looks past it, so retaining one
// element is equivalent to retaining all of them and costs nothing for an item
// padded with thousands of siblings.
type mediaContents []rssMediaURL

// UnmarshalXML retains the first image-typed element with a non-blank url.
// XML decoding errors are returned; other elements are ignored.
func (c *mediaContents) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var m rssMediaURL
	if err := d.DecodeElement(&m, &start); err != nil {
		return err
	}
	if len(*c) == 0 && m.usableImage() {
		*c = append(*c, m)
	}
	return nil
}

// rssCategories collects an item's <category> text, trimmed, keeping the first
// maxCategories non-blank values.
type rssCategories []string

// UnmarshalXML retains the trimmed text of non-blank categories up to
// maxCategories. XML decoding errors are returned; blank or excess categories
// are ignored.
func (c *rssCategories) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var text string
	if err := d.DecodeElement(&text, &start); err != nil {
		return err
	}
	if text = strings.TrimSpace(text); text != "" && len(*c) < maxCategories {
		*c = append(*c, text)
	}
	return nil
}

// mediaGroup is Media RSS's <media:group>, the wrapper YouTube's Atom feeds
// (and some RSS feeds) put an entry's thumbnail and description in. Its
// children are matched by local name, which is safe here because the group's
// own children can't collide with an entry's core fields: only the entry's
// direct children share a struct with <content>, <description> and <title>.
type mediaGroup struct {
	Thumbnail    firstThumbnail    `xml:"thumbnail"`
	ImageContent firstImageContent `xml:"content"`
	Description  string            `xml:"description"`
}

// firstThumbnail and firstImageContent keep only the first usable url among a
// group's repeated <media:thumbnail> / image-typed <media:content> children,
// so a group with many children retains one string rather than a slice sized
// by the response. encoding/xml calls UnmarshalXML once per matching child.
type (
	firstThumbnail    string
	firstImageContent string
)

func (f *firstThumbnail) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var m rssMediaURL
	if err := d.DecodeElement(&m, &start); err != nil {
		return err
	}
	if *f == "" {
		*f = firstThumbnail(strings.TrimSpace(m.URL))
	}
	return nil
}

func (f *firstImageContent) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var m rssMediaURL
	if err := d.DecodeElement(&m, &start); err != nil {
		return err
	}
	if *f == "" && m.isImage() {
		*f = firstImageContent(strings.TrimSpace(m.URL))
	}
	return nil
}

// image returns the group's first thumbnail, else its first image-typed
// <media:content>. It is nil-safe so callers needn't check for a group.
func (g *mediaGroup) image() string {
	if g == nil {
		return ""
	}
	if g.Thumbnail != "" {
		return string(g.Thumbnail)
	}
	return string(g.ImageContent)
}

// description returns the group's trimmed <media:description>; nil-safe.
func (g *mediaGroup) description() string {
	if g == nil {
		return ""
	}
	return strings.TrimSpace(g.Description)
}

// mediaImage picks the first available Media RSS image: a direct thumbnail,
// then direct image content, then whatever the entry's <media:group> holds.
// Candidates are compared trimmed, so a whitespace-only url attribute can't
// shadow a usable image further down the list.
func mediaImage(thumbnail *rssMediaURL, content []rssMediaURL, group *mediaGroup) string {
	if thumbnail != nil {
		if u := strings.TrimSpace(thumbnail.URL); u != "" {
			return u
		}
	}
	for _, c := range content {
		if c.usableImage() {
			return strings.TrimSpace(c.URL)
		}
	}
	return group.image()
}

// entryImage picks the first available image source from an RSS item's
// enclosure, Media RSS thumbnail/content elements, or <media:group>.
func entryImage(enclosure *rssEnclosure, thumbnail *rssMediaURL, content []rssMediaURL, group *mediaGroup) string {
	if enclosure != nil && strings.HasPrefix(enclosure.Type, "image/") {
		if u := strings.TrimSpace(enclosure.URL); u != "" {
			return u
		}
	}
	return mediaImage(thumbnail, content, group)
}

func parseRSS(data []byte) ([]Entry, string, error) {
	var envelope rssEnvelope
	if err := unmarshalXML(data, &envelope); err != nil {
		return nil, "", err
	}

	return rssItemsToEntries(envelope.Channel.Items), pickText(envelope.Channel.Title), nil
}

// parseRDF parses an RSS 1.0 (RDF) document. Unlike RSS 2.0, its <item>
// elements are top-level siblings of <channel> rather than nested inside it,
// so it needs its own envelope, but shares rssItem's field mapping and
// rssItemsToEntries' entry construction.
func parseRDF(data []byte) ([]Entry, string, error) {
	var envelope rdfEnvelope
	if err := unmarshalXML(data, &envelope); err != nil {
		return nil, "", err
	}

	return rssItemsToEntries(envelope.Items), pickText(envelope.Channel.Title), nil
}

// rssItemsToEntries converts RSS 2.0 or RSS 1.0 items in document order, recording
// each item's index in Seq. Identity falls back from GUID to link to title to
// ContentID.
// Invalid or missing dates become zero
// times after trying pubDate and then dc:date.
func rssItemsToEntries(items []rssItem) []Entry {
	entries := make([]Entry, 0, len(items))
	for i, item := range items {
		title := pickText(item.Title)
		link := pickText(item.Link)

		published, _ := parseTime(pickText(item.PubDate))
		if published.IsZero() {
			// RDF (RSS 1.0) items carry no <pubDate>; dc:date is their date
			// field instead.
			published, _ = parseTime(item.DCDate)
		}

		// pickText has already trimmed all three, so pickIdentity's trimming
		// of the GUID and link changes nothing here.
		id, idSource := pickIdentity(pickText(item.GUID), link, title)

		author := pickText(item.Author)
		if author == "" {
			author = strings.TrimSpace(item.Creator)
		}

		description := pickText(item.Description)
		if description == "" {
			description = item.MediaGroup.description()
		}

		entries = append(entries, withContentID(Entry{
			ID:          id,
			IDSource:    idSource,
			Title:       title,
			Link:        link,
			Description: description,
			Content:     pickTextPrefer(item.ContentEncoded, contentNamespace, ""),
			Image:       strings.TrimSpace(entryImage(item.Enclosure, item.MediaThumbnail, item.MediaContent, item.MediaGroup)),
			Author:      author,
			Categories:  trimCategories(item.Categories),
			Published:   published,
			Seq:         i,
		}))
	}

	return entries
}
