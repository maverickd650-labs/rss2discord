package rss

import (
	"testing"
)

// An Atom type="xhtml" construct holds its value in a child <div>, which a
// chardata-only field would drop; type="html"/"text" must stay unchanged.
func TestParseFeed_AtomXHTMLConstructs(t *testing.T) {
	const doc = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="http://www.w3.org/1999/xhtml"><title type="xhtml"><x:div>Feed <b>Name</b></x:div></title>
<entry><id>1</id>
<title type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">Hello <i>world</i></div></title>
<summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Short</p></div></summary>
<content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Full body</p></div></content></entry>
<entry><id>2</id><title type="html">A &amp;lt;b&amp;gt;</title><summary type="text">Plain</summary></entry></feed>`
	res, title, err := parseFeed([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if want := `<x:div>Feed <b>Name</b></x:div>`; title != want {
		t.Errorf("feed title = %q, want %q", title, want)
	}
	e := res[0]
	for name, pair := range map[string][2]string{
		"title":   {e.Title, `<div xmlns="http://www.w3.org/1999/xhtml">Hello <i>world</i></div>`},
		"summary": {e.Description, `<div xmlns="http://www.w3.org/1999/xhtml"><p>Short</p></div>`},
		"content": {e.Content, `<div xmlns="http://www.w3.org/1999/xhtml"><p>Full body</p></div>`},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	if res[1].Title != "A &lt;b&gt;" || res[1].Description != "Plain" {
		t.Errorf("html/text constructs changed: %q / %q", res[1].Title, res[1].Description)
	}
}

func TestParseFeed_AtomXHTMLSummaryFallback(t *testing.T) {
	const content = `<x:div><x:p>Full body</x:p></x:div>`
	for _, tc := range []struct {
		name, summary, want string
	}{
		{name: "empty summary body", summary: "", want: content},
		{name: "empty wrapper", summary: `<div xmlns="http://www.w3.org/1999/xhtml"/>`, want: content},
		{name: "prefixed whitespace", summary: "<x:div><x:p> \n\t </x:p></x:div>", want: content},
		{name: "comments and breaks", summary: `<x:div><!-- hidden --><x:br/></x:div>`, want: content},
		{name: "whitespace entity", summary: `<x:div>&#160;</x:div>`, want: content},
		{name: "script and style only", summary: `<x:div><x:script>hidden</x:script><x:style>hidden</x:style></x:div>`, want: content},
		{name: "visible markup preserved", summary: `<x:div><x:p>Short</x:p><x:p>Next</x:p></x:div>`, want: `<x:div><x:p>Short</x:p><x:p>Next</x:p></x:div>`},
		{name: "visible entity", summary: `<x:div>&amp;</x:div>`, want: `<x:div>&amp;</x:div>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="http://www.w3.org/1999/xhtml"><entry><id>1</id>` +
				`<summary type="xhtml">` + tc.summary + `</summary><content type="xhtml">` + content + `</content></entry></feed>`
			entries, _, err := parseFeed([]byte(doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parse: entries=%v err=%v", entries, err)
			}
			if got := entries[0].Description; got != tc.want {
				t.Errorf("description=%q, want %q", got, tc.want)
			}
			if got := entries[0].Content; got != content {
				t.Errorf("content=%q, want %q", got, content)
			}
		})
	}
}

// Malformed markup inside an xhtml construct is a parse error, not a
// silently empty value.
func TestParseFeed_AtomXHTMLMalformedIsError(t *testing.T) {
	const doc = `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id><title type="xhtml"><div>unclosed</title></entry></feed>`
	if _, _, err := parseFeed([]byte(doc)); err == nil {
		t.Fatal("expected an error for malformed xhtml")
	}
}

func TestParseFeed_AtomXHTMLNamespaceAndFallbacks(t *testing.T) {
	const visible = `<x:div><x:p>Visible &amp; safe</x:p></x:div>`
	for _, tc := range []struct {
		name, body, wantTitle, wantDescription, wantContent, wantID string
	}{
		{
			name:      "mixed case type with surrounding whitespace",
			body:      `<title type=" XhTmL ">  ` + visible + `  </title>`,
			wantTitle: visible, wantID: visible,
		},
		{
			name: "extension titles and summaries after atom fields",
			body: `<title type="xhtml">` + visible + `</title><media:title>Wrong title</media:title>` +
				`<summary type="xhtml">` + visible + `</summary><media:summary>Wrong summary</media:summary>`,
			wantTitle: visible, wantDescription: visible, wantID: visible,
		},
		{
			name: "extension fields before atom fields",
			body: `<media:title>Wrong title</media:title><media:summary>Wrong summary</media:summary>` +
				`<title type="xhtml">` + visible + `</title><summary type="xhtml">` + visible + `</summary>`,
			wantTitle: visible, wantDescription: visible, wantID: visible,
		},
		{
			name: "blank xhtml does not consume same namespace candidate",
			body: `<title type="xhtml"><x:div/></title><title>Usable title</title>` +
				`<summary type="xhtml"><x:div><!-- blank --></x:div></summary><summary>Usable summary</summary>`,
			wantTitle: "Usable title", wantDescription: "Usable summary", wantID: "Usable title",
		},
		{
			name: "empty summary and content fall back to media description",
			body: `<summary type="xhtml"><x:div>&#160;</x:div></summary>` +
				`<content type="xhtml"><x:div><x:style>hidden</x:style></x:div></content>` +
				`<media:group><media:description>Media fallback</media:description></media:group>`,
			wantDescription: "Media fallback",
		},
		{
			name:            "visible text after hidden script is retained",
			body:            `<content type="xhtml"><x:div><x:script>hidden</x:script><x:p>Visible</x:p></x:div></content>`,
			wantDescription: `<x:div><x:script>hidden</x:script><x:p>Visible</x:p></x:div>`,
			wantContent:     `<x:div><x:script>hidden</x:script><x:p>Visible</x:p></x:div>`,
		},
		{
			name:            "self closing hidden elements do not hide following text",
			body:            `<summary type="xhtml"><x:div><x:script/><x:style/>Visible</x:div></summary>`,
			wantDescription: `<x:div><x:script/><x:style/>Visible</x:div>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:x="http://www.w3.org/1999/xhtml" ` +
				`xmlns:media="http://search.yahoo.com/mrss/"><entry>` + tc.body + `</entry></feed>`
			entries, _, err := parseFeed([]byte(doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %d entries, err=%v", len(entries), err)
			}
			e := entries[0]
			wantID := tc.wantID
			if wantID == "" {
				wantID = ContentID(e) // no id, link or title
			}
			if e.Title != tc.wantTitle || e.Description != tc.wantDescription || e.Content != tc.wantContent || e.ID != wantID {
				t.Fatalf("entry = %+v; want title=%q description=%q content=%q id=%q", e, tc.wantTitle, tc.wantDescription, tc.wantContent, wantID)
			}
		})
	}
}

func TestParseFeed_AtomXHTMLMalformedSummaryAndContent(t *testing.T) {
	for _, element := range []string{"summary", "content"} {
		t.Run(element, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom"><entry><` + element +
				` type="xhtml"><div>unclosed</` + element + `></entry></feed>`
			entries, title, err := parseFeed([]byte(doc))
			if err == nil || len(entries) != 0 || title != "" {
				t.Fatalf("malformed %s: entries=%v title=%q err=%v", element, entries, title, err)
			}
		})
	}
}
