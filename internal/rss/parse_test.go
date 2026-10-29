package rss

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shared literals reused across table-driven cases below; pulled into
// constants so repeated values don't trip goconst.
const (
	relAlternate = "alternate"
	relEnclosure = "enclosure"
	imageJPEG    = "image/jpeg"
	picJPEGURL   = "http://pic.jpg"
	thumbJPEGURL = "http://thumb.jpg"
	encURL       = "http://enc"
	mediumImage  = "image"
	mediumVideo  = "video"
	groupJPEGURL = "http://g.jpg"

	titleReal      = "Real"
	titleOnlyMedia = "OnlyMedia"
	linkA1         = "https://a/1"
	descShort      = "short"
	summaryShort   = "Short"
	bodyFull       = "Full body"
)

func TestParseTime(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
		// wantZero is true when a successful parse should yield the zero time
		// (i.e. blank input), distinct from an error.
		wantZero bool
	}{
		{name: "blank", value: "   ", wantZero: true},
		{name: "RFC3339", value: "2015-10-21T07:28:00Z"},
		{name: "RFC1123Z", value: "Wed, 21 Oct 2015 07:28:00 +0000"},
		{name: "RFC1123", value: "Wed, 21 Oct 2015 07:28:00 UTC"},
		{name: "single-digit day", value: "Wed, 2 Jan 2006 15:04:05 MST"},
		{name: "date only", value: "2015-10-21"},
		{name: "datetime no zone", value: "2015-10-21 07:28:00"},
		{name: "unsupported", value: "not a date at all", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTime(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.value, err)
			}
			if tc.wantZero && !got.IsZero() {
				t.Fatalf("expected zero time for %q, got %v", tc.value, got)
			}
			if !tc.wantZero {
				if got.IsZero() {
					t.Fatalf("expected non-zero time for %q", tc.value)
				}
				if got.Location() != time.UTC {
					t.Fatalf("expected parsed time normalized to UTC, got %v", got.Location())
				}
			}
		})
	}
}

func TestPrimaryLink(t *testing.T) {
	cases := []struct {
		name  string
		links []atomLink
		want  string
	}{
		{name: "no links", links: nil, want: ""},
		{
			name:  "explicit alternate preferred over enclosure",
			links: []atomLink{{Href: encURL, Rel: relEnclosure}, {Href: "http://alt", Rel: relAlternate}},
			want:  "http://alt",
		},
		{
			name:  "unmarked rel treated as alternate",
			links: []atomLink{{Href: "http://first"}},
			want:  "http://first",
		},
		{
			name:  "falls back to first when no alternate",
			links: []atomLink{{Href: encURL, Rel: relEnclosure}, {Href: "http://self", Rel: "self"}},
			want:  encURL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := primaryLink(tc.links); got != tc.want {
				t.Fatalf("primaryLink() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnclosureImage(t *testing.T) {
	cases := []struct {
		name  string
		links []atomLink
		want  string
	}{
		{name: "no links", links: nil, want: ""},
		{
			name:  "image enclosure matched",
			links: []atomLink{{Href: picJPEGURL, Rel: relEnclosure, Type: imageJPEG}},
			want:  picJPEGURL,
		},
		{
			name:  "non-image enclosure ignored",
			links: []atomLink{{Href: "http://file.pdf", Rel: relEnclosure, Type: "application/pdf"}},
			want:  "",
		},
		{
			name: "whitespace-only href skipped for a later usable one",
			links: []atomLink{
				{Href: "  ", Rel: relEnclosure, Type: imageJPEG},
				{Href: picJPEGURL, Rel: relEnclosure, Type: imageJPEG},
			},
			want: picJPEGURL,
		},
		{
			name:  "alternate link with image type ignored",
			links: []atomLink{{Href: picJPEGURL, Rel: relAlternate, Type: imageJPEG}},
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := enclosureImage(tc.links); got != tc.want {
				t.Fatalf("enclosureImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRSSMediaURL_IsImage(t *testing.T) {
	cases := []struct {
		name string
		m    rssMediaURL
		want bool
	}{
		{name: "medium image", m: rssMediaURL{Medium: mediumImage}, want: true},
		{name: "type image", m: rssMediaURL{Type: "image/png"}, want: true},
		{name: "video medium", m: rssMediaURL{Medium: mediumVideo}, want: false},
		{name: "empty", m: rssMediaURL{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.isImage(); got != tc.want {
				t.Fatalf("isImage() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEntryImage(t *testing.T) {
	cases := []struct {
		name      string
		enclosure *rssEnclosure
		thumbnail *rssMediaURL
		content   []rssMediaURL
		group     *mediaGroup
		want      string
	}{
		{name: "nothing set", want: ""},
		{
			name:      "enclosure preferred",
			enclosure: &rssEnclosure{URL: "http://enc.jpg", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      "http://enc.jpg",
		},
		{
			name:      "non-image enclosure skipped, falls to thumbnail",
			enclosure: &rssEnclosure{URL: "http://file.mp3", Type: "audio/mpeg"},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:    "media content image used when no enclosure/thumbnail",
			content: []rssMediaURL{{URL: "http://c.jpg", Medium: mediumImage}},
			want:    "http://c.jpg",
		},
		{
			name:    "non-image media content ignored",
			content: []rssMediaURL{{URL: "http://c.mp4", Medium: mediumVideo}},
			want:    "",
		},
		{
			name:      "enclosure with empty url skipped",
			enclosure: &rssEnclosure{URL: "", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:    "group thumbnail used after direct sources",
			content: []rssMediaURL{{URL: "http://c.mp4", Medium: mediumVideo}},
			group:   &mediaGroup{Thumbnail: groupJPEGURL},
			want:    groupJPEGURL,
		},
		{
			name:      "direct thumbnail preferred over group",
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			group:     &mediaGroup{Thumbnail: groupJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:  "group image content used when group has no thumbnail",
			group: &mediaGroup{ImageContent: "http://g.png"},
			want:  "http://g.png",
		},
		{
			// Whitespace-only url attributes at every earlier tier must not
			// shadow the usable group image.
			name:      "whitespace-only urls skipped",
			enclosure: &rssEnclosure{URL: "  ", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: "\n"},
			content:   []rssMediaURL{{URL: " ", Medium: mediumImage}},
			group:     &mediaGroup{Thumbnail: groupJPEGURL},
			want:      groupJPEGURL,
		},
		{
			name:  "group without usable image",
			group: &mediaGroup{},
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryImage(tc.enclosure, tc.thumbnail, tc.content, tc.group); got != tc.want {
				t.Fatalf("entryImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMediaGroupDecodeKeepsFirstUsable(t *testing.T) {
	cases := []struct {
		name, xml, want string
	}{
		{
			name: "first non-blank thumbnail wins, later ones dropped",
			xml: `<group><thumbnail url=" "/><thumbnail url=" http://g1.jpg "/>` +
				`<thumbnail url="http://g2.jpg"/><content url="http://c.png" medium="image"/></group>`,
			want: "http://g1.jpg",
		},
		{
			name: "first image content wins when no thumbnail",
			xml: `<group><content url="http://g.mp4" medium="video"/>` +
				`<content url="http://first.png" medium="image"/><content url="http://g2.png" type="image/png"/></group>`,
			want: "http://first.png",
		},
		{
			name: "no usable image",
			xml:  `<group><thumbnail/><content url="http://g.mp4" medium="video"/></group>`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g mediaGroup
			if err := xml.Unmarshal([]byte(tc.xml), &g); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := g.image(); got != tc.want {
				t.Fatalf("image() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseFeed_AtomEntry(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>urn:uuid:1</id>
<title>Atom Title</title>
<link rel="alternate" href="http://example.com/a" />
<updated>2015-10-21T07:28:00Z</updated>
<content>Body content</content>
</entry>
</feed>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.ID != "urn:uuid:1" || e.Title != "Atom Title" || e.Link != "http://example.com/a" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	// Summary is empty, so Content is used as the description.
	if e.Description != "Body content" {
		t.Fatalf("expected description from content, got %q", e.Description)
	}
	// No published element, so the updated timestamp is used.
	if e.Published.IsZero() {
		t.Fatal("expected published derived from updated timestamp")
	}
}

func TestParseFeed_AtomResolvesRelativeLinksAgainstXMLBase(t *testing.T) {
	// Atom feeds commonly give a relative href for <link> (and an enclosure
	// image) alongside an xml:base, rather than a full URL. Without
	// resolving against it, the relative string is unusable as a link and
	// isn't recognized as a URL by anything downstream that expects an
	// absolute one.
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://example.com/blog/">
<entry>
<id>1</id>
<title>Feed-level base</title>
<link rel="alternate" href="2026/article-1"/>
<link rel="enclosure" type="image/jpeg" href="img/1.jpg"/>
</entry>
<entry xml:base="https://other.example.com/news/">
<id>2</id>
<title>Entry-level base overrides feed-level</title>
<link rel="alternate" href="article-2"/>
</entry>
<entry>
<id>3</id>
<title>Already absolute</title>
<link rel="alternate" href="https://elsewhere.example.com/x"/>
</entry>
</feed>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if want := "https://example.com/blog/2026/article-1"; entries[0].Link != want {
		t.Fatalf("expected link %q, got %q", want, entries[0].Link)
	}
	if want := "https://example.com/blog/img/1.jpg"; entries[0].Image != want {
		t.Fatalf("expected image %q, got %q", want, entries[0].Image)
	}
	if want := "https://other.example.com/news/article-2"; entries[1].Link != want {
		t.Fatalf("expected entry-level base to override feed-level, got %q", entries[1].Link)
	}
	if want := "https://elsewhere.example.com/x"; entries[2].Link != want {
		t.Fatalf("expected already-absolute link unchanged, got %q", entries[2].Link)
	}
}

func TestParseFeed_AtomLeavesRelativeLinkUnchangedWithoutBase(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry><id>1</id><title>No base</title><link rel="alternate" href="/2026/article"/></entry>
</feed>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "/2026/article"; entries[0].Link != want {
		t.Fatalf("expected relative link unchanged without xml:base, got %q", entries[0].Link)
	}
}

func TestResolveAtomURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		ref  string
		want string
	}{
		{name: "empty base returns ref unchanged", base: "", ref: "/a", want: "/a"},
		{name: "empty ref returns ref unchanged", base: "http://example.com/", ref: "", want: ""},
		{name: "already absolute ref passes through", base: "http://example.com/", ref: "http://other.com/a", want: "http://other.com/a"},
		{name: "relative ref resolves against base", base: "http://example.com/feed/", ref: "a", want: "http://example.com/feed/a"},
		{name: "malformed base returns ref unchanged", base: "%zz", ref: "/a", want: "/a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAtomURL(tc.base, tc.ref); got != tc.want {
				t.Fatalf("resolveAtomURL(%q, %q) = %q, want %q", tc.base, tc.ref, got, tc.want)
			}
		})
	}
}

func TestParseFeed_RSSFallsBackToLinkAndTitleForID(t *testing.T) {
	// No guid, so the link is used as the ID.
	data := []byte(`<?xml version="1.0"?>
<rss><channel>
<item><title>NoGuid</title><link>http://example.com/link</link></item>
<item><title>OnlyTitle</title></item>
</channel></rss>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "http://example.com/link" {
		t.Fatalf("expected link used as ID, got %q", entries[0].ID)
	}
	if entries[1].ID != "OnlyTitle" {
		t.Fatalf("expected title used as ID, got %q", entries[1].ID)
	}
}

func TestParseFeed_AssignsDocumentOrderSeq(t *testing.T) {
	// Seq must reflect each entry's position in the feed (0 = first listed),
	// for both RSS and Atom, so the consumer can fall back to document order
	// when entries carry no usable publish date.
	rssData := []byte(`<?xml version="1.0"?>
<rss><channel>
<item><title>First</title></item>
<item><title>Second</title></item>
<item><title>Third</title></item>
</channel></rss>`)
	rssEntries, _, err := parseFeed(rssData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, e := range rssEntries {
		if e.Seq != i {
			t.Fatalf("rss entry %q: expected Seq %d, got %d", e.Title, i, e.Seq)
		}
	}

	atomData := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>First</title></entry>
<entry><title>Second</title></entry>
</feed>`)
	atomEntries, _, err := parseFeed(atomData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, e := range atomEntries {
		if e.Seq != i {
			t.Fatalf("atom entry %q: expected Seq %d, got %d", e.Title, i, e.Seq)
		}
	}
}

func TestParseFeed_InvalidXML(t *testing.T) {
	if _, _, err := parseFeed([]byte("<not-closed")); err == nil {
		t.Fatal("expected error for malformed XML, got nil")
	}
}

func TestParseFeed_RSSAuthorAndCategories(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<rss><channel>
<item>
  <title>WithAuthor</title>
  <author>jane@example.com (Jane Doe)</author>
  <category>Go</category>
  <category>  </category>
  <category>Kubernetes</category>
</item>
<item>
  <title>WithCreator</title>
  <creator>Dublin Core Jane</creator>
</item>
</channel></rss>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].Author != "jane@example.com (Jane Doe)" {
		t.Fatalf("unexpected author: %q", entries[0].Author)
	}
	if want := []string{"Go", "Kubernetes"}; !slicesEqual(entries[0].Categories, want) {
		t.Fatalf("unexpected categories: %v, want %v", entries[0].Categories, want)
	}

	// No <author>, so <creator> (Dublin Core's de facto author field) is used.
	if entries[1].Author != "Dublin Core Jane" {
		t.Fatalf("expected creator fallback for author, got %q", entries[1].Author)
	}
}

func TestParseFeed_AtomAuthorAndCategories(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>1</id>
<title>Atom With Author</title>
<author><name>John Smith</name></author>
<category term="golang" />
<category term="" />
<category term="kubernetes" />
</entry>
</feed>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Author != "John Smith" {
		t.Fatalf("unexpected author: %q", entries[0].Author)
	}
	if want := []string{"golang", "kubernetes"}; !slicesEqual(entries[0].Categories, want) {
		t.Fatalf("unexpected categories: %v, want %v", entries[0].Categories, want)
	}
}

func TestTrimCategories(t *testing.T) {
	got := trimCategories([]string{"  a  ", "", "b", "   "})
	if want := []string{"a", "b"}; !slicesEqual(got, want) {
		t.Fatalf("trimCategories() = %v, want %v", got, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseFeed_UnknownRootReturnsUnrecognizedFormatError(t *testing.T) {
	// Root element is none of rss/feed/RDF; this must surface as a distinct,
	// permanent error rather than silently falling through to Atom (which
	// would "succeed" with zero entries and no error).
	data := []byte(`<?xml version="1.0"?>
<feedwrapper><entry><id>1</id><title>X</title></entry></feedwrapper>`)
	_, _, err := parseFeed(data)
	if err == nil {
		t.Fatal("expected an error for an unrecognized root element")
	}
	var unrecognized *UnrecognizedFormatError
	if !errors.As(err, &unrecognized) {
		t.Fatalf("expected *UnrecognizedFormatError, got %T: %v", err, err)
	}
	if unrecognized.Root != "feedwrapper" {
		t.Fatalf("expected Root %q, got %q", "feedwrapper", unrecognized.Root)
	}
	if got, want := err.Error(), "unrecognized feed format: root element <feedwrapper>"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestParseFeed_MalformedBodyAfterValidRoot asserts a document whose root
// element is recognized but whose body isn't well-formed XML surfaces a decode
// error for every supported format, instead of a partial or empty result.
func TestParseFeed_MalformedBodyAfterValidRoot(t *testing.T) {
	for name, doc := range map[string]string{
		"rss":  `<rss><channel><item><title>x</item></channel></rss>`,
		"atom": `<feed><entry><title>x</entry></feed>`,
		"rdf":  `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><item><title>x</item></rdf:RDF>`,
	} {
		t.Run(name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(doc))
			if err == nil {
				t.Fatalf("expected a decode error, got entries %v", entries)
			}
		})
	}
}

func TestParseFeed_RDF(t *testing.T) {
	// RSS 1.0 (RDF): root <rdf:RDF>, <item> elements are top-level siblings
	// of <channel> rather than nested inside it, and items use <dc:date>
	// instead of <pubDate>.
	data := []byte(`<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
         xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>Example</title></channel>
<item>
  <title>RDF Item</title>
  <link>http://example.com/rdf-item</link>
  <description>An RDF item</description>
  <dc:creator>Jane Doe</dc:creator>
  <dc:date>2015-10-21T07:28:00Z</dc:date>
</item>
</rdf:RDF>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Title != "RDF Item" || e.Link != "http://example.com/rdf-item" || e.Description != "An RDF item" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.Author != "Jane Doe" {
		t.Fatalf("expected dc:creator used as author, got %q", e.Author)
	}
	if e.Published.IsZero() {
		t.Fatal("expected published derived from dc:date")
	}
	if e.ID != "http://example.com/rdf-item" {
		t.Fatalf("expected link used as ID (no guid), got %q", e.ID)
	}
}

func TestParseFeed_DecodesNonUTF8Encodings(t *testing.T) {
	// encoding/xml has no charset support of its own and errors out on any
	// non-UTF-8 declared encoding unless a CharsetReader is wired up; older
	// or non-English-language publishers commonly declare ISO-8859-1 or
	// windows-1252, so a feed in either must still parse and decode its
	// Latin-1 bytes (here 0xE9, "é") to the correct UTF-8 rune.
	cases := []struct {
		name     string
		encoding string
	}{
		{name: "ISO-8859-1", encoding: "ISO-8859-1"},
		{name: "windows-1252", encoding: "windows-1252"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("<?xml version=\"1.0\" encoding=\"" + tc.encoding + "\"?>" +
				"<rss><channel><item><title>Caf\xe9</title><guid>1</guid></item></channel></rss>")

			entries, _, err := parseFeed(data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(entries))
			}
			if want := "Café"; entries[0].Title != want {
				t.Fatalf("expected title %q, got %q", want, entries[0].Title)
			}
		})
	}
}

// encoding/xml matches struct tags by local name regardless of namespace, so
// extension elements such as <atom:link> and <media:title> used to overwrite
// the core RSS fields. These cases pin that the unqualified RSS element wins
// regardless of document order.
func TestParseFeed_RSSNamespacedElementsDoNotOverrideCoreFields(t *testing.T) {
	const nsDecls = `xmlns:atom="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"`

	tests := []struct {
		name string
		item string
		want Entry
	}{
		{
			name: "atom:link and media:title after the core elements",
			item: `<title>Real</title><link>https://a/1</link><atom:link href="https://a/self" rel="self"/>` +
				`<media:title>MediaTitle</media:title><description>short</description>`,
			want: Entry{ID: linkA1, Title: titleReal, Link: linkA1, Description: descShort},
		},
		{
			name: "extension elements before the core elements",
			item: `<atom:link href="https://a/self" rel="self"/><media:title>MediaTitle</media:title>` +
				`<media:description>MediaDesc</media:description>` +
				`<title>Real</title><link>https://a/1</link><description>short</description>`,
			want: Entry{ID: linkA1, Title: titleReal, Link: linkA1, Description: descShort},
		},
		{
			name: "itunes:author does not override author",
			item: `<title>T</title><author>core@example.com</author><itunes:author>Podcaster</itunes:author>`,
			want: Entry{ID: "T", Title: "T", Author: "core@example.com"},
		},
		{
			name: "itunes:author before author",
			item: `<title>T</title><itunes:author>Podcaster</itunes:author><author>core@example.com</author>`,
			want: Entry{ID: "T", Title: "T", Author: "core@example.com"},
		},
		{
			name: "itunes:author is used when there is no author",
			item: `<title>T</title><itunes:author>Podcaster</itunes:author>`,
			want: Entry{ID: "T", Title: "T", Author: "Podcaster"},
		},
		{
			name: "namespaced-only title is still used as a lenient fallback",
			item: `<media:title>OnlyMedia</media:title>`,
			want: Entry{ID: titleOnlyMedia, Title: titleOnlyMedia},
		},
		{
			name: "blank core element does not mask a namespaced value",
			item: `<title>  </title><media:title>OnlyMedia</media:title>`,
			want: Entry{ID: titleOnlyMedia, Title: titleOnlyMedia},
		},
		{
			name: "self-closing atom:link alone leaves the link empty",
			item: `<title>T</title><atom:link href="https://a/self" rel="self"/>`,
			want: Entry{ID: "T", Title: "T"},
		},
		{
			name: "guid is not overridden by a namespaced guid",
			item: `<title>T</title><guid>real-guid</guid><media:guid>other</media:guid>`,
			want: Entry{ID: "real-guid", Title: "T"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(`<rss ` + nsDecls + `><channel><item>` + tt.item + `</item></channel></rss>`)

			entries, _, err := parseFeed(data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(entries))
			}
			got := entries[0]
			if got.ID != tt.want.ID || got.Title != tt.want.Title || got.Link != tt.want.Link ||
				got.Description != tt.want.Description || got.Author != tt.want.Author {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// RSS 1.0 items live in the http://purl.org/rss/1.0/ default namespace, so
// that namespace must outrank unrelated extension namespaces.
func TestParseFeed_RDFDefaultNamespacePreferredOverExtensions(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"
         xmlns="http://purl.org/rss/1.0/"
         xmlns:media="http://search.yahoo.com/mrss/">
<channel><title>C</title></channel>
<item><media:title>MediaTitle</media:title><title>Real</title><link>http://example.com/rdf-ns-item</link></item>
</rdf:RDF>`)

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Title != titleReal || entries[0].Link != "http://example.com/rdf-ns-item" || entries[0].ID != "http://example.com/rdf-ns-item" {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
}

func TestPickText(t *testing.T) {
	mk := func(space, text string) nsText {
		return nsText{XMLName: xml.Name{Space: space, Local: "x"}, Text: text}
	}
	tests := []struct {
		name  string
		cands []nsText
		want  string
	}{
		{"empty", nil, ""},
		{"unqualified wins over rss1.0 and others", []nsText{mk("urn:other", "o"), mk(rss10Namespace, "r"), mk("", "u")}, "u"},
		{"rss1.0 wins over others", []nsText{mk("urn:other", "o"), mk(rss10Namespace, "r")}, "r"},
		{"first non-blank of any namespace", []nsText{mk("urn:a", "  "), mk("urn:b", "b"), mk("urn:c", "c")}, "b"},
		{"blank candidates skipped within a tier", []nsText{mk("", " \n"), mk("", "u")}, "u"},
		{"all blank", []nsText{mk("", ""), mk("urn:a", " ")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickText(tt.cands); got != tt.want {
				t.Fatalf("pickText = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNSTextsBoundsRetainedCandidates asserts an item padded with thousands
// of same-named elements keeps at most one candidate per namespace (and at
// most maxNSCandidates overall) instead of one per element, without changing
// which value pickText selects.
func TestNSTextsBoundsRetainedCandidates(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss xmlns:media="http://search.yahoo.com/mrss/"><channel><item>`)
	b.WriteString(strings.Repeat("<title/>", 5000))
	b.WriteString(strings.Repeat("<media:title>M</media:title>", 5000))
	// 20 distinct extension namespaces fill the fallback cap before the real
	// unqualified and RSS 1.0 values arrive; those must still be retained.
	for i := range 20 {
		fmt.Fprintf(&b, `<x:title xmlns:x="urn:ns%d">N%d</x:title>`, i, i)
	}
	b.WriteString(`<r:title xmlns:r="http://purl.org/rss/1.0/">RDF</r:title>`)
	b.WriteString("<title>Real</title><title>Later</title>")
	b.WriteString(`</item></channel></rss>`)

	var env rssEnvelope
	if err := unmarshalXML([]byte(b.String()), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cands := env.Channel.Items[0].Title
	// maxNSCandidates fallbacks plus the unqualified and RSS 1.0 tiers.
	if len(cands) > maxNSCandidates+2 {
		t.Fatalf("retained %d candidates, want at most %d", len(cands), maxNSCandidates+2)
	}
	if got := pickText(cands); got != "Real" {
		t.Fatalf("pickText = %q, want %q", got, "Real")
	}
}

func TestParseFeed_YouTubeAtomFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "youtube.xml"))
	if err != nil {
		t.Fatal(err)
	}

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	first := entries[0]
	if first.ID != "yt:video:abc123" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Title != "First video" {
		t.Errorf("Title = %q, want the entry title (media:title is nested in the group)", first.Title)
	}
	if first.Link != "https://www.youtube.com/watch?v=abc123" {
		t.Errorf("Link = %q, want the alternate link", first.Link)
	}
	if first.Image != "https://i.ytimg.com/vi/abc123/hqdefault.jpg" {
		t.Errorf("Image = %q", first.Image)
	}
	if first.Description != "Line one of the description.\nLine two." {
		t.Errorf("Description = %q", first.Description)
	}
	if first.Author != "Example Channel" {
		t.Errorf("Author = %q", first.Author)
	}

	// An empty media:description leaves the description empty but the
	// thumbnail still comes through.
	second := entries[1]
	if second.Image != "https://i.ytimg.com/vi/def456/hqdefault.jpg" || second.Description != "" {
		t.Errorf("second entry image/description = %q/%q", second.Image, second.Description)
	}
}

func TestParseFeed_AtomMediaGroupDoesNotClobberCoreFields(t *testing.T) {
	// <media:content> and <media:title> share local names with Atom's
	// <content> and <title>; inside <media:group> they must not touch the
	// entry's own values, in either document order.
	const group = `<media:group><media:title>GroupTitle</media:title>` +
		`<media:content url="https://cdn.example/v.mp4" type="video/mp4"/>` +
		`<media:thumbnail url="https://cdn.example/t.jpg"/>` +
		`<media:description>GroupDesc</media:description></media:group>`
	const core = `<id>e1</id><title>Real</title><link href="https://a/1"/>` +
		`<content type="html">Real content</content>`

	for name, body := range map[string]string{
		"group after core":  core + group,
		"group before core": group + core,
	} {
		t.Run(name, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/"><entry>` + body + `</entry></feed>`
			entries, _, err := parseFeed([]byte(doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
			}
			e := entries[0]
			if e.Title != titleReal || e.Description != "Real content" {
				t.Errorf("title/description = %q/%q, want Real/Real content", e.Title, e.Description)
			}
			if e.Image != "https://cdn.example/t.jpg" {
				t.Errorf("Image = %q", e.Image)
			}
		})
	}
}

func TestParseFeed_AtomMediaImagePrecedenceAndDescriptionFallback(t *testing.T) {
	const head = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xml:base="https://example.com/blog/"><entry><id>1</id>`
	const enclosure = `<link rel="enclosure" type="image/png" href="enc.png"/>`
	const direct = `<media:thumbnail url="direct.jpg"/>`
	const grpThumb = `<media:group><media:thumbnail url="group.jpg"/>`
	const grpContent = `<media:content url="groupcontent.png" medium="image"/>`

	tests := []struct {
		name      string
		body      string
		wantImage string
		wantDesc  string
	}{
		{
			name:      "enclosure beats everything",
			body:      grpThumb + grpContent + `</media:group>` + direct + enclosure,
			wantImage: "https://example.com/blog/enc.png",
		},
		{
			name:      "direct thumbnail beats group",
			body:      grpThumb + `</media:group>` + direct,
			wantImage: "https://example.com/blog/direct.jpg",
		},
		{
			name:      "group thumbnail beats group content",
			body:      `<media:group>` + grpContent + `<media:thumbnail url="https://abs.example/g.jpg"/></media:group>`,
			wantImage: "https://abs.example/g.jpg",
		},
		{
			name:      "group image content as last resort",
			body:      `<media:group>` + grpContent + `<media:content url="v.mp4" medium="video"/></media:group>`,
			wantImage: "https://example.com/blog/groupcontent.png",
		},
		{
			name:     "summary beats content beats group description",
			body:     `<summary>S</summary><content>C</content><media:group><media:description>G</media:description></media:group>`,
			wantDesc: "S",
		},
		{
			name:     "content beats group description",
			body:     `<content>C</content><media:group><media:description>G</media:description></media:group>`,
			wantDesc: "C",
		},
		{
			name:     "group description when nothing else",
			body:     `<media:group><media:description>  G  </media:description></media:group>`,
			wantDesc: "G",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(head + tc.body + `</entry></feed>`))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
			}
			if entries[0].Image != tc.wantImage || entries[0].Description != tc.wantDesc {
				t.Errorf("image/description = %q/%q, want %q/%q", entries[0].Image, entries[0].Description, tc.wantImage, tc.wantDesc)
			}
		})
	}
}

func TestParseFeed_RSSMediaGroup(t *testing.T) {
	const doc = `<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>` +
		`<item><title>WithGroup</title><link>https://a/1</link>` +
		`<media:group><media:content url="https://cdn.example/v.mp4" medium="video"/>` +
		`<media:thumbnail url="https://cdn.example/t.jpg"/>` +
		`<media:description>GroupDesc</media:description></media:group></item>` +
		`<item><title>RealDesc</title><link>https://a/2</link><description>Real</description>` +
		`<media:group><media:description>GroupDesc</media:description></media:group>` +
		`<enclosure url="https://cdn.example/e.jpg" type="image/jpeg"/></item>` +
		`</channel></rss>`

	entries, _, err := parseFeed([]byte(doc))
	if err != nil || len(entries) != 2 {
		t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
	}
	if entries[0].Image != "https://cdn.example/t.jpg" || entries[0].Description != "GroupDesc" {
		t.Errorf("item 0 image/description = %q/%q", entries[0].Image, entries[0].Description)
	}
	// A real description and an enclosure image both outrank the group.
	if entries[1].Image != "https://cdn.example/e.jpg" || entries[1].Description != titleReal {
		t.Errorf("item 1 image/description = %q/%q", entries[1].Image, entries[1].Description)
	}
}

func TestParseFeed_RSSContentEncoded(t *testing.T) {
	const cns = `xmlns:content="http://purl.org/rss/1.0/modules/content/"`
	cases := []struct {
		name        string
		doc         string
		wantContent string
		wantDesc    string
	}{
		{
			name: "prefixed content:encoded",
			doc: `<rss ` + cns + `><channel><item><title>T</title><description>short</description>` +
				`<content:encoded><![CDATA[<p>Full <b>body</b></p>]]></content:encoded></item></channel></rss>`,
			wantContent: "<p>Full <b>body</b></p>",
			wantDesc:    descShort,
		},
		{
			// No namespace declaration at all: the local name still matches.
			name: "unprefixed encoded",
			doc: `<rss><channel><item><title>T</title><description>short</description>` +
				`<encoded>Plain body</encoded></item></channel></rss>`,
			wantContent: "Plain body",
			wantDesc:    descShort,
		},
		{
			name: "content namespace beats lookalike in another namespace",
			doc: `<rss ` + cns + ` xmlns:x="urn:other"><channel><item><title>T</title>` +
				`<content:encoded>Real</content:encoded><x:encoded>Impostor</x:encoded></item></channel></rss>`,
			wantContent: titleReal,
		},
		{
			name: "lookalike before real element still loses",
			doc: `<rss ` + cns + ` xmlns:x="urn:other"><channel><item><title>T</title>` +
				`<x:encoded>Impostor</x:encoded><content:encoded>Real</content:encoded></item></channel></rss>`,
			wantContent: titleReal,
		},
		{
			name: "blank content:encoded does not mask a lower tier",
			doc: `<rss ` + cns + ` xmlns:x="urn:other"><channel><item><title>T</title>` +
				`<content:encoded>  </content:encoded><x:encoded>Fallback</x:encoded></item></channel></rss>`,
			wantContent: "Fallback",
		},
		{
			name:        "absent",
			doc:         `<rss><channel><item><title>T</title><description>d</description></item></channel></rss>`,
			wantContent: "",
			wantDesc:    "d",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(tc.doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
			}
			if entries[0].Content != tc.wantContent {
				t.Errorf("Content = %q, want %q", entries[0].Content, tc.wantContent)
			}
			if entries[0].Description != tc.wantDesc {
				t.Errorf("Description = %q, want %q", entries[0].Description, tc.wantDesc)
			}
		})
	}
}

func TestParseFeed_AtomContent(t *testing.T) {
	const head = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/"><entry><id>1</id>`
	cases := []struct {
		name        string
		body        string
		wantContent string
		wantDesc    string
	}{
		{
			name:        "content alongside summary",
			body:        `<summary>Short</summary><content type="html">Full body</content>`,
			wantContent: bodyFull,
			wantDesc:    summaryShort,
		},
		{
			// With no summary the content is already the description; it is
			// still exposed as Content too.
			name:        "content without summary",
			body:        `<content>Only body</content>`,
			wantContent: "Only body",
			wantDesc:    "Only body",
		},
		{
			name:        "summary only",
			body:        `<summary>Short</summary>`,
			wantContent: "",
			wantDesc:    summaryShort,
		},
		{
			name:        "direct media:content does not clobber content",
			body:        `<summary>Short</summary><content>Full body</content><media:content url="https://cdn.example/v.mp4"/>`,
			wantContent: bodyFull,
			wantDesc:    summaryShort,
		},
		{
			name:        "direct media:content before content",
			body:        `<media:content url="https://cdn.example/v.mp4"/><content>Full body</content>`,
			wantContent: bodyFull,
			wantDesc:    bodyFull,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(head + tc.body + `</entry></feed>`))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
			}
			if entries[0].Content != tc.wantContent || entries[0].Description != tc.wantDesc {
				t.Errorf("content/description = %q/%q, want %q/%q",
					entries[0].Content, entries[0].Description, tc.wantContent, tc.wantDesc)
			}
		})
	}
}

func TestParseFeed_FeedTitle(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "rss",
			doc:  `<rss><channel><title> My Blog </title><item><title>Item</title></item></channel></rss>`,
			want: "My Blog",
		},
		{
			name: "rss without title",
			doc:  `<rss><channel><item><title>Item</title></item></channel></rss>`,
			want: "",
		},
		{
			// Item titles are nested one level down and must not be taken
			// for the channel's.
			name: "rss title after items",
			doc:  `<rss><channel><item><title>Item</title></item><title>Late Title</title></channel></rss>`,
			want: "Late Title",
		},
		{
			name: "rss channel-level extension titles do not override",
			doc: `<rss xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:media="http://search.yahoo.com/mrss/">` +
				`<channel><title>Real Show</title><itunes:title>Itunes Title</itunes:title>` +
				`<media:title>Media Title</media:title><item><title>Ep</title></item></channel></rss>`,
			want: "Real Show",
		},
		{
			name: "rss extension title first still loses",
			doc: `<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>` +
				`<media:title>Media Title</media:title><title>Real Show</title></channel></rss>`,
			want: "Real Show",
		},
		{
			name: "rss falls back to a lone extension title",
			doc: `<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>` +
				`<media:title>Media Title</media:title></channel></rss>`,
			want: "Media Title",
		},
		{
			name: "atom",
			doc: `<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom Feed</title>` +
				`<entry><id>1</id><title>Entry</title></entry></feed>`,
			want: "Atom Feed",
		},
		{
			name: "atom without xmlns",
			doc:  `<feed><title>Bare Atom</title><entry><id>1</id></entry></feed>`,
			want: "Bare Atom",
		},
		{
			name: "atom feed-level extension titles do not override",
			doc: `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">` +
				`<media:title>Media Title</media:title><title>Atom Feed</title><itunes:title>Itunes Title</itunes:title>` +
				`<entry><id>1</id></entry></feed>`,
			want: "Atom Feed",
		},
		{
			name: "rdf",
			doc: `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">` +
				`<channel><title>RDF Feed</title><link>http://example.com</link></channel>` +
				`<item><title>Item</title><link>http://example.com/1</link></item></rdf:RDF>`,
			want: "RDF Feed",
		},
		{
			name: "rdf channel-level extension title does not override",
			doc: `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:media="http://search.yahoo.com/mrss/">` +
				`<channel><title>RDF Feed</title><media:title>Media Title</media:title></channel>` +
				`<item><title>Item</title><link>http://example.com/1</link></item></rdf:RDF>`,
			want: "RDF Feed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := parseFeed([]byte(tc.doc))
			if err != nil {
				t.Fatalf("parseFeed: %v", err)
			}
			if got != tc.want {
				t.Errorf("feed title = %q, want %q", got, tc.want)
			}
		})
	}
}

// An Atom type="xhtml" construct holds its value in a child <div>, which a
// chardata-only field would drop; type="html"/"text" must stay unchanged.
func TestParseFeed_AtomXHTMLConstructs(t *testing.T) {
	const doc = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="http://www.w3.org/1999/xhtml"><title type="xhtml"><x:div>Feed <b>Name</b></x:div></title>
<entry><id>1</id>
<title type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">Hello <i>world</i></div></title>
<summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Short</p></div></summary>
<content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Full body</p></div></content></entry>
<entry><id>2</id><title type="html">A &amp;lt;b&amp;gt;</title><summary type="text">Plain</summary></entry></feed>`
	res, title, err := parseFeed([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if want := `<x:div>Feed <b>Name</b></x:div>`; title != want {
		t.Errorf("feed title = %q, want %q", title, want)
	}
	e := res[0]
	for name, pair := range map[string][2]string{
		"title":   {e.Title, `<div xmlns="http://www.w3.org/1999/xhtml">Hello <i>world</i></div>`},
		"summary": {e.Description, `<div xmlns="http://www.w3.org/1999/xhtml"><p>Short</p></div>`},
		"content": {e.Content, `<div xmlns="http://www.w3.org/1999/xhtml"><p>Full body</p></div>`},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	if res[1].Title != "A &lt;b&gt;" || res[1].Description != "Plain" {
		t.Errorf("html/text constructs changed: %q / %q", res[1].Title, res[1].Description)
	}
}

func TestParseFeed_AtomXHTMLSummaryFallback(t *testing.T) {
	const content = `<x:div><x:p>Full body</x:p></x:div>`
	for _, tc := range []struct {
		name, summary, want string
	}{
		{name: "empty summary body", summary: "", want: content},
		{name: "empty wrapper", summary: `<div xmlns="http://www.w3.org/1999/xhtml"/>`, want: content},
		{name: "prefixed whitespace", summary: "<x:div><x:p> \n\t </x:p></x:div>", want: content},
		{name: "comments and breaks", summary: `<x:div><!-- hidden --><x:br/></x:div>`, want: content},
		{name: "whitespace entity", summary: `<x:div>&#160;</x:div>`, want: content},
		{name: "script and style only", summary: `<x:div><x:script>hidden</x:script><x:style>hidden</x:style></x:div>`, want: content},
		{name: "visible markup preserved", summary: `<x:div><x:p>Short</x:p><x:p>Next</x:p></x:div>`, want: `<x:div><x:p>Short</x:p><x:p>Next</x:p></x:div>`},
		{name: "visible entity", summary: `<x:div>&amp;</x:div>`, want: `<x:div>&amp;</x:div>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="http://www.w3.org/1999/xhtml"><entry><id>1</id>` +
				`<summary type="xhtml">` + tc.summary + `</summary><content type="xhtml">` + content + `</content></entry></feed>`
			entries, _, err := parseFeed([]byte(doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parse: entries=%v err=%v", entries, err)
			}
			if got := entries[0].Description; got != tc.want {
				t.Errorf("description=%q, want %q", got, tc.want)
			}
			if got := entries[0].Content; got != content {
				t.Errorf("content=%q, want %q", got, content)
			}
		})
	}
}

// Malformed markup inside an xhtml construct is a parse error, not a
// silently empty value.
func TestParseFeed_AtomXHTMLMalformedIsError(t *testing.T) {
	const doc = `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id><title type="xhtml"><div>unclosed</title></entry></feed>`
	if _, _, err := parseFeed([]byte(doc)); err == nil {
		t.Fatal("expected an error for malformed xhtml")
	}
}

func TestParseFeed_AtomXHTMLNamespaceAndFallbacks(t *testing.T) {
	const visible = `<x:div><x:p>Visible &amp; safe</x:p></x:div>`
	for _, tc := range []struct {
		name, body, wantTitle, wantDescription, wantContent, wantID string
	}{
		{
			name:      "mixed case type with surrounding whitespace",
			body:      `<title type=" XhTmL ">  ` + visible + `  </title>`,
			wantTitle: visible, wantID: visible,
		},
		{
			name: "extension titles and summaries after atom fields",
			body: `<title type="xhtml">` + visible + `</title><media:title>Wrong title</media:title>` +
				`<summary type="xhtml">` + visible + `</summary><media:summary>Wrong summary</media:summary>`,
			wantTitle: visible, wantDescription: visible, wantID: visible,
		},
		{
			name: "extension fields before atom fields",
			body: `<media:title>Wrong title</media:title><media:summary>Wrong summary</media:summary>` +
				`<title type="xhtml">` + visible + `</title><summary type="xhtml">` + visible + `</summary>`,
			wantTitle: visible, wantDescription: visible, wantID: visible,
		},
		{
			name: "blank xhtml does not consume same namespace candidate",
			body: `<title type="xhtml"><x:div/></title><title>Usable title</title>` +
				`<summary type="xhtml"><x:div><!-- blank --></x:div></summary><summary>Usable summary</summary>`,
			wantTitle: "Usable title", wantDescription: "Usable summary", wantID: "Usable title",
		},
		{
			name: "empty summary and content fall back to media description",
			body: `<summary type="xhtml"><x:div>&#160;</x:div></summary>` +
				`<content type="xhtml"><x:div><x:style>hidden</x:style></x:div></content>` +
				`<media:group><media:description>Media fallback</media:description></media:group>`,
			wantDescription: "Media fallback",
		},
		{
			name:            "visible text after hidden script is retained",
			body:            `<content type="xhtml"><x:div><x:script>hidden</x:script><x:p>Visible</x:p></x:div></content>`,
			wantDescription: `<x:div><x:script>hidden</x:script><x:p>Visible</x:p></x:div>`,
			wantContent:     `<x:div><x:script>hidden</x:script><x:p>Visible</x:p></x:div>`,
		},
		{
			name:            "self closing hidden elements do not hide following text",
			body:            `<summary type="xhtml"><x:div><x:script/><x:style/>Visible</x:div></summary>`,
			wantDescription: `<x:div><x:script/><x:style/>Visible</x:div>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="http://www.w3.org/1999/xhtml" ` +
				`xmlns:media="http://search.yahoo.com/mrss/"><entry>` + tc.body + `</entry></feed>`
			entries, _, err := parseFeed([]byte(doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %d entries, err=%v", len(entries), err)
			}
			e := entries[0]
			if e.Title != tc.wantTitle || e.Description != tc.wantDescription || e.Content != tc.wantContent || e.ID != tc.wantID {
				t.Fatalf("entry = %+v; want title=%q description=%q content=%q id=%q", e, tc.wantTitle, tc.wantDescription, tc.wantContent, tc.wantID)
			}
		})
	}
}

func TestParseFeed_AtomXHTMLMalformedSummaryAndContent(t *testing.T) {
	for _, element := range []string{"summary", "content"} {
		t.Run(element, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom"><entry><` + element +
				` type="xhtml"><div>unclosed</` + element + `></entry></feed>`
			entries, title, err := parseFeed([]byte(doc))
			if err == nil || len(entries) != 0 || title != "" {
				t.Fatalf("malformed %s: entries=%v title=%q err=%v", element, entries, title, err)
			}
		})
	}
}
