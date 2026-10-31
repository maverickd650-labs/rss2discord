package controller

import (
	"strings"
	"testing"
)

// TestStripHTMLMarkupEdgeCases covers real-world feed markup that a regex
// tag-stripper mishandles: ">" inside quoted attribute values or comments,
// bare "<" in prose, and the text content of script/style elements, which
// must never reach a Discord message.
func TestStripHTMLMarkupEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "gt in double-quoted attribute", input: `<a title="a>b">link</a> text`, want: "link text"},
		{name: "gt in single-quoted attribute", input: `<a title='a>b'>link</a> text`, want: "link text"},
		{name: "gt inside comment", input: `a<!-- x > y -->b`, want: "ab"},
		{name: "comment dropped", input: `a<!-- hidden -->b`, want: "ab"},
		{name: "bare lt in prose", input: "1 < 2 and 3 > 2", want: "1 < 2 and 3 > 2"},
		{name: "script content dropped", input: `before<script>var x = 1 < 2;</script>after`, want: "beforeafter"},
		{name: "script content with tags dropped", input: `a<script>document.write("<p>x</p>")</script>b`, want: "ab"},
		{name: "style content dropped", input: `<style>p{color:red}</style>kept`, want: "kept"},
		{name: "uppercase block tags", input: "<P>one</P><P>two</P>", want: "one\n\ntwo"},
		{name: "prefixed XHTML paragraphs", input: "<x:div><x:p>First</x:p><x:p>Second</x:p></x:div>", want: "First\n\nSecond"},
		{name: "prefixed XHTML script and style", input: "<x:script>hidden</x:script><x:style>hidden</x:style><x:p>kept</x:p>", want: "kept"},
		{name: "self-closing br", input: "x<br/>y", want: "x\ny"},
		{name: "spaced self-closing br", input: "x<br />y", want: "x\ny"},
		{name: "unclosed paragraph", input: "<p>open para", want: "open para"},
		{name: "heading becomes line", input: "<h2>Title</h2>body", want: "Title\nbody"},
		{name: "list items", input: "<ul><li>a</li><li>b</li></ul>", want: "a\n\nb"},
		{name: "numeric entity", input: "it&#8217;s", want: "it’s"},
		{name: "hex entity", input: "it&#x2019;s", want: "it’s"},
		{name: "nbsp trimmed", input: "&nbsp;text&nbsp;", want: "text"},
		{name: "escaped markup stays literal text", input: "&lt;p&gt;not a tag&lt;/p&gt;", want: "<p>not a tag</p>"},
		{name: "empty input", input: "", want: ""},
		{name: "tags only", input: "<p></p><br>", want: ""},
		{name: "unicode preserved", input: "<p>日本語 é́</p>", want: "日本語 é́"},
		{name: "unterminated tag at eof", input: "tail <a href=", want: "tail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.input); got != tc.want {
				t.Fatalf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// FuzzStripHTML checks structural invariants of the stripped output for
// arbitrary input: it must be trimmed, never leave a run of 3+ newlines, and
// never contain script/style element bodies.
func FuzzStripHTML(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		"<p>one</p><p>two</p>",
		`<a title="a>b">x</a>`,
		"a<!-- c > d -->b",
		"1 < 2 > 0",
		"<script>alert(1)</script>",
		"<style>p{}</style>",
		"<p>x</p>\n\n\n\n<p>y</p>",
		"&amp;&lt;&#0;&#x110000;",
		"\x00\xff<p\x00>",
		"<" + strings.Repeat("a", 1000),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		got := stripHTML(input)
		if got != strings.TrimSpace(got) {
			t.Fatalf("stripHTML(%q) = %q, not trimmed", input, got)
		}
		if strings.Contains(got, "\n\n\n") {
			t.Fatalf("stripHTML(%q) = %q, contains 3+ consecutive newlines", input, got)
		}
	})
}

// stripHTMLBenchInputs are representative entry bodies: a short feed
// excerpt, a typical article summary, and a pathological large payload.
func stripHTMLBenchInputs() map[string]string {
	short := `<p>Some excerpt text with a <a href="https://example.com/a">link</a>.</p>` +
		`<a href="https://example.com/full">Continue reading...</a>`
	article := strings.Repeat(
		`<p class="para">Lorem <b>ipsum</b> dolor sit amet, <a href="https://example.com/x?a=1&amp;b=2" title="t">consectetur</a> `+
			`adipiscing elit &amp; sed do eiusmod.</p><ul><li>one</li><li>two</li></ul><br/>`, 20)
	large := strings.Repeat(article, 40)
	return map[string]string{"short": short, "article": article, "large": large}
}

func BenchmarkStripHTML(b *testing.B) {
	for _, name := range []string{"short", "article", "large"} {
		in := stripHTMLBenchInputs()[name]
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for b.Loop() {
				_ = stripHTML(in)
			}
		})
	}
}

func TestStripHTML(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain text unchanged", input: "plain text body", want: "plain text body"},
		{name: "paragraphs become blank lines", input: "<p>one</p><p>two</p>", want: "one\n\ntwo"},
		{name: "br becomes newline", input: "a<br>b", want: "a\nb"},
		{name: "inline tags stripped", input: "<b>bold</b> text", want: "bold text"},
		{name: "entities unescaped", input: "a &amp; b &lt;c&gt;", want: "a & b <c>"},
		{name: "collapses excess blank lines", input: "<p>a</p><br><br><br><p>b</p>", want: "a\n\nb"},
		{
			name:  "trailing continue reading link stripped",
			input: `<p>` + excerptText + `</p><a href="https://example.com/full">Continue reading...</a>`,
			want:  excerptText,
		},
		{
			name:  "trailing continue reading link with ellipsis char stripped",
			input: `<p>` + excerptText + `</p><p><a href="https://example.com/full">Continue reading…</a></p>`,
			want:  excerptText,
		},
		{
			name:  "trailing continue reading plain text stripped",
			input: excerptText + "\nContinue reading...",
			want:  excerptText,
		},
		{
			name:  "continue reading mid-text kept",
			input: "<p>Click <a href=\"https://example.com\">Continue reading</a> below for more, then come back.</p>",
			want:  "Click Continue reading below for more, then come back.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.input); got != tc.want {
				t.Fatalf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
