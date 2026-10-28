package rss

import "testing"

// FuzzParseFeed exercises parseFeed against arbitrary bytes. Feed XML comes
// straight from whatever rssUrl a namespace user configures, so the parser
// must never panic regardless of how malformed or adversarial the response
// body is; a panic here would crash the controller's reconcile loop and
// take down every other FeedGroup it's processing in the same run.
func FuzzParseFeed(f *testing.F) {
	seeds := []string{
		`<?xml version="1.0"?><rss><channel><item><title>T</title></item></channel></rss>`,
		`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id><title>T</title></entry></feed>`,
		`<rss><channel><item><title>NoGuid</title><link>http://example.com</link></item></channel></rss>`,
		`<feed><entry><id>1</id><author><name>A</name></author><category term="x"/></entry></feed>`,
		`<rss><channel><item><author>a@b.com</author><creator>C</creator><category>x</category></item></channel></rss>`,
		`not xml at all`,
		``,
		`<`,
		`<rss>`,
		`<a xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:creator>X</dc:creator></a>`,
		`<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel><title>C</title></channel><item><title>T</title><link>http://example.com</link><dc:creator>A</dc:creator><dc:date>2015-10-21T07:28:00Z</dc:date></item></rdf:RDF>`,
		`<rss xmlns:atom="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/"><channel><item><title>Real</title><link>https://a/1</link><atom:link href="https://a/self" rel="self"/><media:title>MediaTitle</media:title><description>short</description></item></channel></rss>`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseFeed(data)
		if err != nil {
			return
		}
		for _, e := range entries {
			_ = e.ID
			_ = e.Title
			_ = e.Link
			_ = e.Description
			_ = e.Image
			_ = e.Author
			_ = e.Categories
			_ = e.Published
		}
	})
}
