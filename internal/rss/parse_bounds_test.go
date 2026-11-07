package rss

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// assertSequentialEntries checks that entries[i] is the i-th entry in
// document order: ID "id-<i>" and Seq i.
func assertSequentialEntries(t *testing.T, entries []Entry) {
	t.Helper()
	for i, e := range entries {
		if want := fmt.Sprintf("id-%d", i); e.ID != want || e.Seq != i {
			t.Fatalf("entries[%d] = {ID: %q, Seq: %d}, want {ID: %q, Seq: %d}", i, e.ID, e.Seq, want, i)
		}
	}
}

// TestParseFeed_BoundsEntryCount asserts a document with more than
// maxFeedEntries items/entries decodes exactly maxFeedEntries of them, that
// they are the first ones in document order, and that Seq still reflects the
// document position.
func TestParseFeed_BoundsEntryCount(t *testing.T) {
	const total = maxFeedEntries + 50
	build := func(open, el, closing string) []byte {
		var b strings.Builder
		b.WriteString(open)
		for i := range total {
			fmt.Fprintf(&b, el, i, i)
		}
		b.WriteString(closing)
		return []byte(b.String())
	}
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "rss 2.0",
			data: build(`<rss><channel><title>F</title>`, `<item><guid>id-%d</guid><title>T%d</title></item>`, `</channel></rss>`),
		},
		{
			name: "rss 1.0",
			data: build(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/"><channel><title>F</title></channel>`,
				`<item><link>id-%d</link><title>T%d</title></item>`, `</rdf:RDF>`),
		},
		{
			name: formatAtom,
			data: build(`<feed xmlns="http://www.w3.org/2005/Atom"><title>F</title>`, `<entry><id>id-%d</id><title>T%d</title></entry>`, `</feed>`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, feedTitle, err := parseFeed(tt.data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(entries) != maxFeedEntries {
				t.Fatalf("got %d entries, want %d", len(entries), maxFeedEntries)
			}
			assertSequentialEntries(t, entries)
			if feedTitle != "F" {
				t.Fatalf("feed title = %q, want %q", feedTitle, "F")
			}
		})
	}
}

// TestParseFeed_EntryCountBoundDoesNotMaskLaterErrors asserts elements past the
// cap are skipped rather than ignored: malformed XML after them is still an
// error, as it is for a document under the cap.
func TestParseFeed_EntryCountBoundDoesNotMaskLaterErrors(t *testing.T) {
	data := `<rss><channel>` + strings.Repeat(`<item><title>T</title></item>`, maxFeedEntries+1) +
		`<item><title>broken</item></channel></rss>`
	if _, _, err := parseFeed([]byte(data)); err == nil {
		t.Fatal("expected a decoding error for malformed XML past the entry cap")
	}
}

// TestParseFeed_RSSBoundsPerItemSlices asserts an item padded with thousands of
// <media:content> and <category> elements retains a bounded number of each,
// without changing the image or categories that are selected.
func TestParseFeed_RSSBoundsPerItemSlices(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss xmlns:media="http://search.yahoo.com/mrss/"><channel><item><title>T</title>`)
	// Unusable media:content (wrong medium, blank url) ahead of the first
	// usable image, then more images that must be ignored.
	b.WriteString(strings.Repeat(`<media:content url="http://v.mp4" medium="video"/>`, 3000))
	b.WriteString(`<media:content url="  " medium="image"/>`)
	b.WriteString(`<media:content url=" http://first.png " type="image/png"/>`)
	b.WriteString(strings.Repeat(`<media:content url="http://later.png" medium="image"/>`, 3000))
	b.WriteString(strings.Repeat(`<category>  </category>`, 3000))
	for i := range 3 * maxCategories {
		fmt.Fprintf(&b, `<category> c%d </category>`, i)
	}
	b.WriteString(`</item></channel></rss>`)
	data := []byte(b.String())

	var env rssEnvelope
	if err := unmarshalXML(data, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item := env.Channel.Items[0]
	if len(item.MediaContent) != 1 {
		t.Fatalf("retained %d media:content elements, want 1", len(item.MediaContent))
	}
	if len(item.Categories) != maxCategories {
		t.Fatalf("retained %d categories, want %d", len(item.Categories), maxCategories)
	}

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := entries[0].Image; got != "http://first.png" {
		t.Fatalf("image = %q, want %q", got, "http://first.png")
	}
	want := make([]string, maxCategories)
	for i := range want {
		want[i] = fmt.Sprintf("c%d", i)
	}
	if !slices.Equal(entries[0].Categories, want) {
		t.Fatalf("categories = %v, want the first %d non-blank ones in order", entries[0].Categories, maxCategories)
	}
}

func TestParseFeed_AtomBoundsCategories(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id>`)
	b.WriteString(strings.Repeat(`<category term=" "/>`, 3000))
	for i := range 3 * maxCategories {
		fmt.Fprintf(&b, `<category term=" c%d "/>`, i)
	}
	b.WriteString(`</entry></feed>`)

	entries, _, err := parseFeed([]byte(b.String()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := make([]string, maxCategories)
	for i := range want {
		want[i] = fmt.Sprintf("c%d", i)
	}
	if !slices.Equal(entries[0].Categories, want) {
		t.Fatalf("categories = %v, want the first %d non-blank ones in order", entries[0].Categories, maxCategories)
	}
}
