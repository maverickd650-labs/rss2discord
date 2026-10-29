package rss

import (
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The same retention rules apply to all three document shapes, including RDF's
// items outside its channel. Metadata follows the items to exercise decoding
// after the entry limit has been reached.
// Literals repeated across the limit tests below; constants keep goconst quiet.
const (
	hrefArticle     = "article"
	blogBaseURL     = "https://example.com/blog/"
	blogPostArticle = "https://example.com/blog/posts/article"
	postsDir        = "posts/"
)

var limitTestFormats = []struct {
	name     string
	document string
	entry    string
	category string
}{
	{
		name:     "rss2",
		document: `<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>%s<title>After entries</title></channel></rss>`,
		entry:    `<item>%s</item>`,
		category: `<category> %s </category>`,
	},
	{
		name: formatRDF,
		document: `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:media="http://search.yahoo.com/mrss/">` +
			`%s<channel><title>After entries</title></channel></rdf:RDF>`,
		entry:    `<item>%s</item>`,
		category: `<category> %s </category>`,
	},
	{
		name:     formatAtom,
		document: `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/">%s<title>After entries</title></feed>`,
		entry:    `<entry>%s</entry>`,
		category: `<category term=" %s "/>`,
	},
}

func TestParseFeed_EntryLimitBoundaries(t *testing.T) {
	for _, format := range limitTestFormats {
		for _, count := range []int{0, maxFeedEntries - 1, maxFeedEntries, maxFeedEntries + 1} {
			t.Run(fmt.Sprintf("%s/%d", format.name, count), func(t *testing.T) {
				var body strings.Builder
				for i := range count {
					fmt.Fprintf(&body, format.entry, fmt.Sprintf("<title>entry-%d</title>", i))
				}
				entries, title, err := parseFeed([]byte(fmt.Sprintf(format.document, body.String())))
				if err != nil {
					t.Fatalf("parseFeed: %v", err)
				}
				if want := min(count, maxFeedEntries); len(entries) != want {
					t.Fatalf("retained %d entries, want %d", len(entries), want)
				}
				for i, entry := range entries {
					if want := fmt.Sprintf("entry-%d", i); entry.Title != want || entry.ID != want || entry.Seq != i {
						t.Fatalf("entry[%d] = %+v, want title/ID %q and Seq %d", i, entry, want, i)
					}
				}
				if title != "After entries" {
					t.Fatalf("feed title after entries = %q", title)
				}
			})
		}
	}
}

func TestParseFeed_EmptyEntriesConsumeLimit(t *testing.T) {
	for _, format := range limitTestFormats {
		t.Run(format.name, func(t *testing.T) {
			body := strings.Repeat(fmt.Sprintf(format.entry, ""), maxFeedEntries) +
				fmt.Sprintf(format.entry, `<title>Must be skipped</title>`)
			entries, _, err := parseFeed([]byte(fmt.Sprintf(format.document, body)))
			if err != nil {
				t.Fatalf("parseFeed: %v", err)
			}
			if len(entries) != maxFeedEntries {
				t.Fatalf("retained %d entries, want %d", len(entries), maxFeedEntries)
			}
			for i, entry := range entries {
				if entry.ID != "" || entry.Title != "" || entry.Seq != i {
					t.Fatalf("entry[%d] = %+v, want empty entry at document position %d", i, entry, i)
				}
			}
		})
	}
}

func TestParseFeed_EntryLimitRejectsMalformedXML(t *testing.T) {
	for _, format := range limitTestFormats {
		for _, preceding := range []int{0, maxFeedEntries - 1, maxFeedEntries, maxFeedEntries + 1} {
			t.Run(fmt.Sprintf("%s/after-%d", format.name, preceding), func(t *testing.T) {
				body := strings.Repeat(fmt.Sprintf(format.entry, `<title>Valid</title>`), preceding) +
					fmt.Sprintf(format.entry, `<extension><title>Unclosed</extension>`)
				entries, title, err := parseFeed([]byte(fmt.Sprintf(format.document, body)))
				if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
					t.Fatalf("error = %v, want XML syntax error", err)
				}
				if entries != nil || title != "" {
					t.Fatalf("malformed feed returned partial result: %d entries, title %q", len(entries), title)
				}
			})
		}
	}
}

func TestParseFeed_CategoryLimitBoundariesAndPerEntryReset(t *testing.T) {
	for _, format := range limitTestFormats {
		for _, count := range []int{0, maxCategories - 1, maxCategories, maxCategories + 1} {
			t.Run(fmt.Sprintf("%s/%d", format.name, count), func(t *testing.T) {
				var categories strings.Builder
				want := make([]string, 0, min(count, maxCategories))
				for i := range count {
					// Blank categories interspersed with duplicates must neither use
					// slots nor turn the retention limit into a deduplication limit.
					fmt.Fprintf(&categories, format.category, "\t")
					value := fmt.Sprintf("tag-%d", i%7)
					fmt.Fprintf(&categories, format.category, value)
					if i < maxCategories {
						want = append(want, value)
					}
				}
				body := fmt.Sprintf(format.entry, categories.String()) +
					fmt.Sprintf(format.entry, fmt.Sprintf(format.category, "next-entry"))
				entries, _, err := parseFeed([]byte(fmt.Sprintf(format.document, body)))
				if err != nil {
					t.Fatalf("parseFeed: %v", err)
				}
				if len(entries) != 2 {
					t.Fatalf("got %d entries, want 2", len(entries))
				}
				if !slices.Equal(entries[0].Categories, want) {
					t.Fatalf("categories = %v, want %v", entries[0].Categories, want)
				}
				if !slices.Equal(entries[1].Categories, []string{"next-entry"}) {
					t.Fatalf("next entry categories = %v", entries[1].Categories)
				}
			})
		}
	}
}

func TestBoundedFieldsRejectMalformedXML(t *testing.T) {
	// Every custom decoder must propagate errors both before and after it has
	// retained enough values, even for candidates that would be discarded.
	cases := []struct {
		name    string
		valid   string
		invalid string
		decode  func([]byte) error
	}{
		{
			name: "rss categories", valid: strings.Repeat(`<category>kept</category>`, maxCategories),
			invalid: `<category><nested></category>`,
			decode: func(data []byte) error {
				var item rssItem
				return xml.Unmarshal(data, &item)
			},
		},
		{
			name: "atom categories", valid: strings.Repeat(`<category term="kept"/>`, maxCategories),
			invalid: `<category term="discarded"><nested></category>`,
			decode: func(data []byte) error {
				var item atomEntry
				return xml.Unmarshal(data, &item)
			},
		},
		{
			name: "media content", valid: `<content medium="image" url="https://example.com/kept.jpg"/>`,
			invalid: `<content medium="video"><nested></content>`,
			decode: func(data []byte) error {
				var item rssItem
				return xml.Unmarshal(data, &item)
			},
		},
		{
			name: "atom links", valid: `<link rel="self" href="self"/><link href="article"/><link rel="enclosure" type="image/png" href="image"/>`,
			invalid: `<link rel="related"><nested></link>`,
			decode: func(data []byte) error {
				var item atomEntry
				return xml.Unmarshal(data, &item)
			},
		},
	}
	for _, tc := range cases {
		for _, full := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/full=%t", tc.name, full), func(t *testing.T) {
				prefix := ""
				if full {
					prefix = tc.valid
				}
				err := tc.decode([]byte(`<entry>` + prefix + tc.invalid + `</entry>`))
				if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
					t.Fatalf("error = %v, want XML syntax error", err)
				}
			})
		}
	}
}

func TestMediaContentsRetainsFirstUsableImage(t *testing.T) {
	cases := []struct {
		name  string
		media string
		want  string
	}{
		{name: "none usable", media: `<content medium="video" url="video"/><content medium="image" url=" "/>`},
		{name: "medium identifies image", media: `<content medium="image" url=" medium.jpg "/>`, want: "medium.jpg"},
		{name: "type identifies image despite video medium", media: `<content medium="video" type="image/png" url="type.png"/>`, want: "type.png"},
		{name: "medium identifies image despite video type", media: `<content medium="image" type="video/mp4" url="medium.png"/>`, want: "medium.png"},
		{name: "blank candidate does not mask later image", media: `<content type="image/png" url=" "/><content type="image/png" url="later.png"/>`, want: "later.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var item rssItem
			if err := xml.Unmarshal([]byte(`<item>`+tc.media+`<content medium="video" url="ignored"/></item>`), &item); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			wantCount := 0
			if tc.want != "" {
				wantCount = 1
			}
			if len(item.MediaContent) != wantCount {
				t.Fatalf("retained %d media elements, want %d", len(item.MediaContent), wantCount)
			}
			if got := mediaImage(nil, item.MediaContent, nil); got != tc.want {
				t.Fatalf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAtomLinksRetentionAcrossCandidateOrders(t *testing.T) {
	links := []string{
		`<link rel="self" href="self"/>`,
		`<link href="article"/>`, // An omitted rel is an alternate, even late in the list.
		`<link rel="enclosure" type="image/png" href=" first.png "/>`,
	}
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			body := links[order[0]] + links[order[1]] + links[order[2]] +
				strings.Repeat(`<link rel="alternate" href="later"/><link rel="enclosure" type="image/png" href="later.png"/>`, 10)
			var item atomEntry
			if err := xml.Unmarshal([]byte(`<entry>`+body+`</entry>`), &item); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			wantCount := 2
			if order[0] == 0 {
				wantCount = 3 // The fallback link is distinct from both useful candidates.
			}
			if len(item.Links) != wantCount {
				t.Fatalf("retained %d links, want %d", len(item.Links), wantCount)
			}
			if primary, image := primaryLink(item.Links), enclosureImage(item.Links); primary != hrefArticle || image != "first.png" {
				t.Fatalf("primary = %q, image = %q, want article and first.png", primary, image)
			}
		})
	}
}

func TestParseFeed_MediaAndLinkLimitsResetPerEntry(t *testing.T) {
	for _, format := range limitTestFormats {
		t.Run(format.name, func(t *testing.T) {
			var body strings.Builder
			for _, name := range []string{"first", "second"} {
				image := fmt.Sprintf(`<media:content medium="image" url="https://example.com/%s.png"/>`, name)
				links := ""
				if format.name == formatAtom {
					// Fill all three retained link roles on each Atom entry.
					links = fmt.Sprintf(`<link rel="self" href="self"/><link href="https://example.com/%s"/>`, name)
					image = fmt.Sprintf(`<link rel="enclosure" type="image/png" href="https://example.com/%s.png"/>`, name)
				}
				fmt.Fprintf(&body, format.entry, links+strings.Repeat(image, 4))
			}
			entries, _, err := parseFeed([]byte(fmt.Sprintf(format.document, body.String())))
			if err != nil {
				t.Fatalf("parseFeed: %v", err)
			}
			if len(entries) != 2 {
				t.Fatalf("got %d entries, want 2", len(entries))
			}
			for i, name := range []string{"first", "second"} {
				if want := "https://example.com/" + name + ".png"; entries[i].Image != want {
					t.Fatalf("entry[%d] image = %q, want %q", i, entries[i].Image, want)
				}
				if want := "https://example.com/" + name; format.name == formatAtom && entries[i].Link != want {
					t.Fatalf("entry[%d] link = %q, want %q", i, entries[i].Link, want)
				}
			}
		})
	}
}

func TestParseFeed_AtomNestedBaseURLBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		feedBase  string
		entryBase string
		href      string
		want      string
	}{
		{name: "parent segments", feedBase: "https://example.com/blog/2026/", entryBase: "../posts/./", href: "../article?view=1#part", want: "https://example.com/blog/article?view=1#part"},
		{name: "feed base names a file", feedBase: "https://example.com/blog/feed.xml", entryBase: postsDir, href: hrefArticle, want: blogPostArticle},
		{name: "entry base names a file", feedBase: blogBaseURL, entryBase: "posts/index.xml", href: hrefArticle, want: blogPostArticle},
		{name: "network relative entry base", feedBase: blogBaseURL, entryBase: "//cdn.example.com/posts/", href: hrefArticle, want: "https://cdn.example.com/posts/article"},
		{name: "trim bases", feedBase: " https://example.com/blog/ ", entryBase: " posts/ ", href: hrefArticle, want: blogPostArticle},
		{name: "blank entry base inherits", feedBase: blogBaseURL, entryBase: " ", href: hrefArticle, want: "https://example.com/blog/article"},
		{name: "absolute reference", feedBase: blogBaseURL, entryBase: postsDir, href: "https://other.example.com/article", want: "https://other.example.com/article"},
		{name: "relative entry base without feed base", entryBase: postsDir, href: hrefArticle, want: "/posts/article"},
		{name: "missing reference stays empty", feedBase: blogBaseURL, entryBase: postsDir},
	}
	for _, tc := range cases {
		for _, imageSource := range []struct {
			name   string
			markup string
		}{
			{name: "enclosure", markup: `<link rel="enclosure" type="image/png" href="%s"/>`},
			{name: "thumbnail", markup: `<media:thumbnail url="%s"/>`},
			{name: "group", markup: `<media:group><media:content medium="image" url="%s"/></media:group>`},
		} {
			t.Run(tc.name+"/"+imageSource.name, func(t *testing.T) {
				data := fmt.Sprintf(`<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xml:base="%s"><entry xml:base="%s"><title>Fallback title</title><link href="%s"/>%s</entry></feed>`,
					tc.feedBase, tc.entryBase, tc.href, fmt.Sprintf(imageSource.markup, tc.href))
				entries, _, err := parseFeed([]byte(data))
				if err != nil {
					t.Fatalf("parseFeed: %v", err)
				}
				if len(entries) != 1 {
					t.Fatalf("got %d entries, want 1", len(entries))
				}
				entry := entries[0]
				if entry.Link != tc.want || entry.Image != tc.want {
					t.Fatalf("link = %q, image = %q, want %q", entry.Link, entry.Image, tc.want)
				}
				wantID := tc.want
				if wantID == "" {
					wantID = "Fallback title"
				}
				if entry.ID != wantID {
					t.Fatalf("fallback ID = %q, want %q", entry.ID, wantID)
				}
			})
		}
	}
}
