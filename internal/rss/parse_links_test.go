package rss

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
)

func TestPrimaryLink(t *testing.T) {
	cases := []struct {
		name  string
		links []atomLink
		want  string
	}{
		{name: caseNoLinks, links: nil, want: ""},
		{
			name:  "explicit alternate preferred over enclosure",
			links: []atomLink{{Href: encURL, Rel: relEnclosure}, {Href: altURL, Rel: relAlternate}},
			want:  altURL,
		},
		{
			name:  "unmarked rel treated as alternate",
			links: []atomLink{{Href: firstURL}},
			want:  firstURL,
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

	assertLinksAndImages(t, mustParseEntries(t, data), []linkAndImage{
		{"https://example.com/blog/2026/article-1", "https://example.com/blog/img/1.jpg"},
		{"https://other.example.com/news/article-2", ""}, // entry-level base overrides feed-level
		{"https://elsewhere.example.com/x", ""},          // already absolute, unchanged
	})
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

func TestParseFeed_AtomEntryXMLBaseComposesWithFeedBase(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://example.com/blog/">
<entry xml:base="posts/2026/">
<id>1</id>
<title>Relative entry base</title>
<link rel="alternate" href="article-1"/>
<link rel="enclosure" type="image/jpeg" href="img/1.jpg"/>
</entry>
<entry xml:base="/news/">
<id>2</id>
<title>Root-relative entry base</title>
<link rel="alternate" href="article-2"/>
</entry>
<entry xml:base="https://other.example.com/x/">
<id>3</id>
<title>Absolute entry base still overrides</title>
<link rel="alternate" href="article-3"/>
</entry>
</feed>`)

	assertLinksAndImages(t, mustParseEntries(t, data), []linkAndImage{
		{"https://example.com/blog/posts/2026/article-1", "https://example.com/blog/posts/2026/img/1.jpg"},
		{"https://example.com/news/article-2", ""},
		{"https://other.example.com/x/article-3", ""},
	})
}

// TestAtomLinksBoundedSelectionMatchesFull decodes the same <link> list twice,
// once into the bounded atomLinks and once into a plain []atomLink, and asserts
// primaryLink and enclosureImage select identically while the bounded form
// retains only a handful of links.
func TestAtomLinksBoundedSelectionMatchesFull(t *testing.T) {
	const noise = 3000
	related := strings.Repeat(`<link rel="related" href="http://related" type="text/html"/>`, noise)
	cases := []struct {
		name          string
		links         string
		wantPrimary   string
		wantEnclosure string
	}{
		{name: caseNoLinks, links: "", wantPrimary: "", wantEnclosure: ""},
		{
			name:        "unmarked link first",
			links:       `<link href="http://first"/>` + related + `<link rel="alternate" href="http://alt"/>`,
			wantPrimary: firstURL,
		},
		{
			name:        "alternate after many non-alternates",
			links:       related + `<link rel="alternate" href="http://alt"/><link rel="alternate" href="http://alt2"/>`,
			wantPrimary: altURL,
		},
		{
			name:        "no alternate falls back to the first link overall",
			links:       `<link rel="self" href="http://self"/>` + related,
			wantPrimary: "http://self",
		},
		{
			name:        "first alternate wins even with a blank href",
			links:       related + `<link rel="alternate" href=""/><link rel="alternate" href="http://alt"/>`,
			wantPrimary: "",
		},
		{
			name: "image enclosure after many links, blank and non-image ones skipped",
			links: related + `<link rel="enclosure" type="application/pdf" href="http://doc.pdf"/>` +
				`<link rel="enclosure" type="image/png" href="  "/>` +
				`<link rel="enclosure" type="image/png" href=" http://img1.png "/>` + related +
				`<link rel="enclosure" type="image/png" href="http://img2.png"/>` +
				`<link rel="alternate" href="http://alt"/>`,
			wantPrimary:   altURL,
			wantEnclosure: "http://img1.png",
		},
		{
			name:          "image enclosure doubles as the first link",
			links:         `<link rel="enclosure" type="image/jpeg" href="http://img.jpg"/>` + related,
			wantPrimary:   "http://img.jpg",
			wantEnclosure: "http://img.jpg",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := []byte(`<entry>` + tc.links + `</entry>`)

			bounded, full := unmarshalBoundedAndFullLinks(t, doc)

			assertField(t, "primaryLink vs full list", primaryLink(bounded), primaryLink(full))
			assertField(t, "enclosureImage vs full list", enclosureImage(bounded), enclosureImage(full))
			assertField(t, "primaryLink", primaryLink(bounded), tc.wantPrimary)
			assertField(t, "enclosureImage", enclosureImage(bounded), tc.wantEnclosure)
			assertField(t, "at most 3 links retained", len(bounded) <= 3, true)
		})
	}
}

// unmarshalBoundedAndFullLinks decodes doc's <link> children twice: through
// looseAtomEntry, which retains a bounded selection, and into an unbounded
// list.
func unmarshalBoundedAndFullLinks(t *testing.T, doc []byte) (bounded, full []atomLink) {
	t.Helper()
	var b looseAtomEntry
	if err := xml.Unmarshal(doc, &b); err != nil {
		t.Fatalf("unmarshal bounded: %v", err)
	}
	var f struct {
		Links []atomLink `xml:"link"`
	}
	if err := xml.Unmarshal(doc, &f); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}
	return b.Links, f.Links
}

// linkAndImage is an entry's expected Link and Image.
type linkAndImage struct{ link, image string }

// assertLinksAndImages checks entries against want, entry by entry.
func assertLinksAndImages(t *testing.T, entries []Entry, want []linkAndImage) {
	t.Helper()
	assertField(t, "entry count", len(entries), len(want))
	for i, w := range want {
		assertField(t, fmt.Sprintf("entry %d link and image", i), linkAndImage{entries[i].Link, entries[i].Image}, w)
	}
}
