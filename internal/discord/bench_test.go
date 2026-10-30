package discord

import (
	"strings"
	"testing"
)

func BenchmarkEmbedToPayload(b *testing.B) {
	e := Embed{
		Title:        "Benchmark Title",
		Description:  strings.Repeat("Lorem ipsum dolor sit amet. ", 200), // exercises Fit's combined-clamp trim path
		URL:          "https://example.com",
		Color:        0x5865F2,
		Timestamp:    "2015-10-21T07:28:00Z",
		ThumbnailURL: "https://example.com/thumb.png",
		AuthorName:   "Author",
		FooterText:   "Footer",
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = embedToPayload(e)
	}
}
