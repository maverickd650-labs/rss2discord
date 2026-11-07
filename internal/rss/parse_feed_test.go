package rss

import (
	"encoding/xml"
	"errors"
	"io"
	"testing"
)

func TestParseFeed_WrapsEveryFailureAndKeepsCause(t *testing.T) {
	_, _, err := parseFeed([]byte(`<rss><channel><item>`))
	if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
		t.Fatalf("truncated body should keep *xml.SyntaxError reachable, got %T: %v", err, err)
	}
	_, _, err = parseFeed(nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty body should keep io.EOF reachable, got %T: %v", err, err)
	}
	if _, ok := errors.AsType[*ParseError](err); !ok {
		t.Fatalf("expected *ParseError, got %T", err)
	}
}

func TestParseFeed_ExtractsImage(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		wantImage string
	}{
		{
			name: "RSS enclosure",
			data: `<?xml version="1.0"?>
<rss><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid>
<enclosure url="http://example.com/pic.jpg" type="image/jpeg" /></item>
</channel></rss>`,
			wantImage: "http://example.com/pic.jpg",
		},
		{
			name: "media thumbnail",
			data: `<?xml version="1.0"?>
<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>
<item><title>Hello</title><link>http://example.com/1</link><description>World</description><guid>1</guid>
<media:thumbnail url="http://example.com/thumb.jpg" /></item>
</channel></rss>`,
			wantImage: "http://example.com/thumb.jpg",
		},
		{
			name: "Atom enclosure",
			data: `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>1</id>
<title>Hello</title>
<link rel="alternate" href="http://example.com/1" />
<link rel="enclosure" href="http://example.com/pic.jpg" type="image/jpeg" />
<summary>World</summary>
</entry>
</feed>`,
			wantImage: "http://example.com/pic.jpg",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(tc.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("unexpected entries: %+v", entries)
			}
			if entries[0].Link != "http://example.com/1" {
				t.Fatalf("unexpected link: %q", entries[0].Link)
			}
			if entries[0].Image != tc.wantImage {
				t.Fatalf("unexpected image: %q", entries[0].Image)
			}
		})
	}
}

func TestParseFeed_DefaultBranchFallsBackToAtomAndFails(t *testing.T) {
	// The root element "RDF" is neither "rss" nor "feed", so parseFeed takes
	// the default branch; the document is otherwise malformed, so both the
	// RSS and the Atom decode attempts fail.
	data := []byte(`<rdf:RDF><channel><unclosed`)

	_, _, err := parseFeed(data)
	if err == nil {
		t.Fatal("expected error for malformed non-rss/feed document, got nil")
	}
}
