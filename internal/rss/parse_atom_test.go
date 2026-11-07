package rss

import (
	"slices"
	"testing"
)

func TestParseFeed_AtomEntry(t *testing.T) {
	e := mustParseOne(t, []byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<entry>
<id>urn:uuid:1</id>
<title>Atom Title</title>
<link rel="alternate" href="http://example.com/a" />
<updated>2015-10-21T07:28:00Z</updated>
<content>Body content</content>
</entry>
</feed>`))
	// Summary is empty, so Content is used as the description.
	assertField(t, "entry", coreOf(e), coreFields{
		ID: "urn:uuid:1", Title: "Atom Title", Link: "http://example.com/a", Description: "Body content",
	})
	// No published element, so the updated timestamp is used.
	assertField(t, "published is zero", e.Published.IsZero(), false)
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
	if want := []string{"golang", "kubernetes"}; !slices.Equal(entries[0].Categories, want) {
		t.Fatalf("unexpected categories: %v, want %v", entries[0].Categories, want)
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
