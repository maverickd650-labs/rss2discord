package rss

import (
	"strings"
	"testing"
)

// benchFeedXML builds a synthetic RSS feed sized just under
// maxFeedResponseBytes, the cap FetchEntries enforces on a real response
// before ever calling parseFeed. BenchmarkParseFeed exercises parseFeed at
// that scale to guard against accidental quadratic behavior as item count
// grows.
func benchFeedXML(targetBytes int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss><channel>`)
	const item = `<item><title>Benchmark Item</title><link>https://example.com/item</link>` +
		`<description>Lorem ipsum dolor sit amet, consectetur adipiscing elit.</description>` +
		`<guid>https://example.com/item</guid><pubDate>Wed, 21 Oct 2015 07:28:00 GMT</pubDate></item>`
	for b.Len() < targetBytes {
		b.WriteString(item)
	}
	b.WriteString(`</channel></rss>`)
	return []byte(b.String())
}

func BenchmarkParseFeed(b *testing.B) {
	data := benchFeedXML(maxFeedResponseBytes - 1024)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := parseFeed(data); err != nil {
			b.Fatal(err)
		}
	}
}
