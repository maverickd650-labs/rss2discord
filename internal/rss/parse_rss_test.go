package rss

import (
	"slices"
	"testing"
)

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
	if want := []string{"Go", "Kubernetes"}; !slices.Equal(entries[0].Categories, want) {
		t.Fatalf("unexpected categories: %v, want %v", entries[0].Categories, want)
	}

	// No <author>, so <creator> (Dublin Core's de facto author field) is used.
	if entries[1].Author != "Dublin Core Jane" {
		t.Fatalf("expected creator fallback for author, got %q", entries[1].Author)
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
