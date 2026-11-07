package rss

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"testing"
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

	caseNoLinks = "no links"
	altURL      = "http://alt"
	firstURL    = "http://first"
	formatAtom  = "atom"
	formatRDF   = "rdf"
	formatRSS   = "rss"
)

func TestParseFeed_InvalidXML(t *testing.T) {
	if _, _, err := parseFeed([]byte("<not-closed")); err == nil {
		t.Fatal("expected error for malformed XML, got nil")
	}
}

func TestParseFeed_UnknownRootReturnsUnrecognizedFormatError(t *testing.T) {
	// Root element is none of rss/feed/RDF; this must surface as a distinct,
	// permanent error rather than silently falling through to Atom (which
	// would "succeed" with zero entries and no error).
	data := []byte(`<?xml version="1.0"?>
<feedwrapper><entry><id>1</id><title>X</title></entry></feedwrapper>`)
	_, _, err := parseFeed(data)
	unrecognized, ok := errors.AsType[*UnrecognizedFormatError](err)
	if !ok {
		t.Fatalf("expected *UnrecognizedFormatError, got %T: %v", err, err)
	}
	if unrecognized.Root != "feedwrapper" {
		t.Fatalf("expected Root %q, got %q", "feedwrapper", unrecognized.Root)
	}
	if _, ok := errors.AsType[*ParseError](err); !ok {
		t.Fatalf("expected *ParseError, got %T: %v", err, err)
	}
	if got, want := unrecognized.Error(), "unrecognized feed format: root element <feedwrapper>"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestParseFeed_MalformedBodyAfterValidRoot asserts a document whose root
// element is recognized but whose body isn't well-formed XML surfaces a decode
// error for every supported format, instead of a partial or empty result.
func TestParseFeed_MalformedBodyAfterValidRoot(t *testing.T) {
	for name, doc := range map[string]string{
		formatRSS:  `<rss><channel><item><title>x</item></channel></rss>`,
		formatAtom: `<feed><entry><title>x</entry></feed>`,
		formatRDF:  `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><item><title>x</item></rdf:RDF>`,
	} {
		t.Run(name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(doc))
			if err == nil {
				t.Fatalf("expected a decode error, got entries %v", entries)
			}
		})
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

func TestParseFeed_AssignsDocumentOrderSeq(t *testing.T) {
	// Seq must reflect each entry's position in the feed (0 = first listed),
	// for both RSS and Atom, so the consumer can fall back to document order
	// when entries carry no usable publish date.
	tests := []struct {
		name string
		data string
	}{
		{formatRSS, `<?xml version="1.0"?>
<rss><channel>
<item><title>First</title></item>
<item><title>Second</title></item>
<item><title>Third</title></item>
</channel></rss>`},
		{formatAtom, `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry><title>First</title></entry>
<entry><title>Second</title></entry>
</feed>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(tt.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for i, e := range entries {
				if e.Seq != i {
					t.Fatalf("entry %q: expected Seq %d, got %d", e.Title, i, e.Seq)
				}
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
			name: formatAtom,
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
			name: formatRDF,
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
	// The RSS 1.0 candidate arrived after the fallback cap filled, but the
	// cap only applies to non-preferred namespaces, so it must still be kept.
	var retainedRDF bool
	for _, c := range cands {
		if c.XMLName.Space == rss10Namespace && c.Text == "RDF" {
			retainedRDF = true
		}
	}
	if !retainedRDF {
		t.Fatalf("RSS 1.0 candidate was dropped although the fallback cap is full: %+v", cands)
	}
}
