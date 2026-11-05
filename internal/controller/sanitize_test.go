package controller

import (
	stdhtml "html"
	"strings"
	"testing"
)

const (
	scriptHiddenMarker                  = "rss2discord-script-hidden"
	styleHiddenMarker                   = "rss2discord-style-hidden"
	visibleText                         = "visible"
	decodedScriptComparisonInput        = "<script>if(a<b){x++}</script>visible"
	prefixedScriptMismatchedEndTagInput = `<x:script>hidden</script>still-hidden</x:script>visible`
	prefixedStyleMismatchedEndTagInput  = `<x:style>hidden</style>still-hidden</x:style>visible`
	prefixedScriptNestedRawTextInput    = `<x:script><script>hidden</x:script>visible`
	prefixedStyleNestedRawTextInput     = `<x:style><style>hidden</x:style>visible`
	prefixedScriptStartTag              = "<x:script>"
	prefixedScriptEndTag                = "</x:script>"
	prefixedStyleEndTag                 = "</x:style>"
	scriptStartTag                      = "<script>"
	scriptEndTagHit                     = "x</script>"
	beforeAfterText                     = "beforeafter"
	beforeOuterTailAfterText            = "beforeouter-tailafter"
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
		{name: "script content dropped", input: `before<script>var x = 1 < 2;</script>after`, want: beforeAfterText},
		{name: "decoded script comparison preserves trailing visible text", input: decodedScriptComparisonInput, want: visibleText},
		{name: "style raw text preserves less-than content", input: "<style>a<b{color:red}</style>visible", want: visibleText},
		{name: "escaped script entities remain literal text", input: "&lt;script&gt;if(a&lt;b){x++}&lt;/script&gt;visible", want: decodedScriptComparisonInput},
		{name: "script content with tags dropped", input: `a<script>document.write("<p>x</p>")</script>b`, want: "ab"},
		{name: "script body ignores inner self-closing script", input: `<script><script/>leaked</script>script-visible`, want: "script-visible"},
		{name: "style body ignores inner self-closing style", input: `<style><style/>leaked</style>style-visible`, want: "style-visible"},
		{name: "prefixed script body ignores inner self-closing script", input: `<x:script><x:script/>leaked</x:script>visible`, want: visibleText},
		{name: "prefixed style body ignores inner self-closing style", input: `<x:style><x:style/>leaked</x:style>visible`, want: visibleText},
		{name: "prefixed script body restores tokenization after nested textarea", input: `<x:script><textarea>hidden</textarea></x:script>visible`, want: visibleText},
		{name: "prefixed style body restores tokenization after nested textarea", input: `<x:style><textarea>hidden</textarea></x:style>visible`, want: visibleText},
		{name: "prefixed script keeps a closed nested script boundary", input: `<x:script><script>"</x:script>leaked"</script>hidden</x:script>visible`, want: visibleText},
		{name: "prefixed script preserves outer boundary across double-escaped nested script", input: `<x:script><script><!--<script></script>--!><script></x:script>leaked</script></x:script>visible`, want: visibleText},
		{name: "prefixed style keeps a closed nested style boundary", input: `<x:style><style>"</x:style>leaked"</style>hidden</x:style>visible`, want: visibleText},
		{name: "prefixed script keeps a closed nested textarea boundary", input: `<x:script><textarea>hidden</x:script>leaked</textarea>tail</x:script>visible`, want: visibleText},
		{name: "prefixed style keeps a closed nested textarea boundary", input: `<x:style><textarea>hidden</x:style>leaked</textarea>tail</x:style>visible`, want: visibleText},
		{name: "prefixed script retains outer boundary for unclosed nested textarea", input: `<x:script><textarea>hidden</x:script>visible`, want: visibleText},
		{name: "prefixed style retains outer boundary for unclosed nested textarea", input: `<x:style><textarea>hidden</x:style>visible`, want: visibleText},
		{name: "prefixed script finds a distant nested textarea end tag", input: `<x:script><textarea>hidden</x:script>` + strings.Repeat("x", 64*1024) + `</textarea>tail</x:script>visible`, want: visibleText},
		{name: "prefixed script handles many unclosed nested textareas", input: `<x:script>` + strings.Repeat("<textarea>hidden", 1000) + `</x:script>visible`, want: visibleText},
		{name: "prefixed script body restores tokenization after nested plaintext", input: `<x:script><plaintext>hidden</plaintext></x:script>visible`, want: visibleText},
		{name: "prefixed style body restores tokenization after nested plaintext", input: `<x:style><plaintext>hidden</plaintext></x:style>visible`, want: visibleText},
		{name: "unprefixed script end tag does not close prefixed script", input: prefixedScriptMismatchedEndTagInput, want: visibleText},
		{name: "unprefixed style end tag does not close prefixed style", input: prefixedStyleMismatchedEndTagInput, want: visibleText},
		{name: "nested script raw text does not consume prefixed outer end tag", input: prefixedScriptNestedRawTextInput, want: visibleText},
		{name: "nested style raw text does not consume prefixed outer end tag", input: prefixedStyleNestedRawTextInput, want: visibleText},
		{name: "prefixed script body ignores nested script element", input: `<x:script><x:script>a</x:script>leaked</x:script>visible`, want: visibleText},
		{name: "script raw text ends at first matching end tag", input: `before<script>outer<script>inner</script>outer-tail</script>after`, want: beforeOuterTailAfterText},
		{name: "style raw text ends at first matching end tag", input: `before<style>outer<style>inner</style>outer-tail</style>after`, want: beforeOuterTailAfterText},
		{name: "mixed-prefix script body ignores inner self-closing script", input: `<x:script><script/>leaked</x:script>visible`, want: visibleText},
		{name: "self-closing script outside a body does not hide following text", input: `a<script/>visible`, want: "avisible"},
		{name: "script body keeps marker after closing tag", input: `<script><script/>` + scriptHiddenMarker + `</script>` + scriptHiddenMarker, want: scriptHiddenMarker},
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

func TestRawTextEndTagLookaheadDoesNotCacheScriptMissesAcrossEscapedStates(t *testing.T) {
	input := `<!--<script></script>--!><script></x:script>leaked</script></x:script>`
	secondScript := strings.LastIndex(input, scriptStartTag)
	if secondScript < 0 {
		t.Fatal("test input is missing its second script start tag")
	}
	afterSecondScript := secondScript + len(scriptStartTag)
	lookahead := rawTextEndTagLookahead{input: input}

	if lookahead.hasEndTagAt(0, scriptTagName) {
		t.Fatal("first script suffix unexpectedly has a matching end tag")
	}
	if _, cached := lookahead.noEndTags[scriptTagName]; cached {
		t.Fatal("script miss must not be cached across tokenizer escape states")
	}
	if !lookahead.hasEndTagAt(afterSecondScript, scriptTagName) {
		t.Fatal("later script suffix has a matching end tag, but lookahead missed it")
	}
}

func TestRawTextEndTagLookaheadCachesMissingTags(t *testing.T) {
	lookahead := rawTextEndTagLookahead{input: `<textarea>first<textarea>second`}

	if lookahead.hasEndTagAt(0, textareaTagName) {
		t.Fatal("hasEndTagAt() = true for an input without a matching end tag")
	}
	if _, ok := lookahead.noEndTags[textareaTagName]; !ok {
		t.Fatal("missing end tag was not cached")
	}
	if lookahead.hasEndTagAt(len(lookahead.input), textareaTagName) {
		t.Fatal("hasEndTagAt() = true for a later suffix after a cached miss")
	}
}

// TestRawTextEndTagLookaheadBoundsScriptMisses pins the budget that keeps
// uncacheable script misses from rescanning the input once per nested tag.
func TestRawTextEndTagLookaheadBoundsScriptMisses(t *testing.T) {
	const tags = 1000
	input := strings.Repeat(scriptStartTag, tags)
	lookahead := rawTextEndTagLookahead{input: input}

	for i := range tags {
		if lookahead.hasEndTagAt(i*len(scriptStartTag), scriptTagName) {
			t.Fatalf("hasEndTagAt(%d) = true for an input without an end tag", i)
		}
	}
	if limit := scriptLookaheadBudgetFactor * len(input); lookahead.scriptMissBytes > limit {
		t.Fatalf("scanned %d bytes for script misses, want at most %d", lookahead.scriptMissBytes, limit)
	}
}

// TestRawTextEndTagLookaheadScriptHitsSpendNoBudget checks that script hits
// are never charged: the tokenizer consumes the scanned span anyway, so only
// misses can make the lookahead's total work superlinear.
func TestRawTextEndTagLookaheadScriptHitsSpendNoBudget(t *testing.T) {
	input := strings.Repeat(scriptEndTagHit, 100)
	lookahead := rawTextEndTagLookahead{input: input}

	for i := range 100 {
		if !lookahead.hasEndTagAt(i*len(scriptEndTagHit), scriptTagName) {
			t.Fatalf("hasEndTagAt(%d) = false, want a matching end tag", i)
		}
	}
	if lookahead.scriptMissBytes != 0 {
		t.Fatalf("script hits charged %d bytes, want 0", lookahead.scriptMissBytes)
	}
}

// TestStripHTMLManyUnclosedNestedScripts covers the input shape the script
// budget exists for: it must stay linear and keep the outer boundary.
func TestStripHTMLManyUnclosedNestedScripts(t *testing.T) {
	for _, unit := range []string{scriptStartTag, "<!----!>" + scriptStartTag} {
		input := prefixedScriptStartTag + strings.Repeat(unit, 20000) + prefixedScriptEndTag + visibleText
		if got := stripHTML(input); got != visibleText {
			t.Fatalf("stripHTML(%q...) = %q, want %q", input[:64], got, visibleText)
		}
	}
}

// FuzzStripHTML checks structural invariants of the stripped output for
// arbitrary input: it must be trimmed and never leave a run of 3+ newlines.
// Targeted script/style body suppression is covered by table-driven regression tests.
func FuzzStripHTML(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		"<p>one</p><p>two</p>",
		`<a title="a>b">x</a>`,
		"a<!-- c > d -->b",
		"1 < 2 > 0",
		"<script>alert(1)</script>",
		"<script/>",
		"<style/>",
		decodedScriptComparisonInput,
		"&lt;script&gt;if(a&lt;b){x++}&lt;/script&gt;visible",
		"<script><script/>" + scriptHiddenMarker + "</script>kept",
		"<script><script>" + scriptHiddenMarker + "</script></script>kept",
		"<script><script/>" + scriptHiddenMarker + "</script>" + scriptHiddenMarker,
		"<textarea><x:script><x:script/>" + scriptHiddenMarker + "</x:script></textarea>",
		"<textarea><x:style><x:style/>" + styleHiddenMarker + "</x:style></textarea>",
		"<x:script><x:script/>" + scriptHiddenMarker + "</x:script>kept",
		prefixedScriptMismatchedEndTagInput,
		prefixedStyleMismatchedEndTagInput,
		"<x:script>hidden</script>" + scriptHiddenMarker + "</x:script>kept",
		"<x:style>hidden</style>" + styleHiddenMarker + "</x:style>kept",
		prefixedScriptNestedRawTextInput,
		prefixedStyleNestedRawTextInput,
		"<x:script><script>" + scriptHiddenMarker + "</x:script>kept",
		"<x:style><style>" + styleHiddenMarker + "</x:style>kept",
		"<style>p{}</style>",
		"<style><style/>" + styleHiddenMarker + "</style>kept",
		"<style><style>" + styleHiddenMarker + "</style></style>kept",
		"<x:style><x:style/>" + styleHiddenMarker + "</x:style>kept",
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

		// Escaping keeps the generated body from closing its wrapper, so this
		// property specifically checks that script/style bodies stay hidden
		// even when they contain an inner self-closing tag.
		escaped := stdhtml.EscapeString(input)
		for _, tc := range []struct {
			outerTag string
			innerTag string
		}{
			{outerTag: scriptTagName, innerTag: scriptTagName},
			{outerTag: styleTagName, innerTag: styleTagName},
			{outerTag: "x:" + scriptTagName, innerTag: "x:" + scriptTagName},
			{outerTag: "x:" + styleTagName, innerTag: "x:" + styleTagName},
		} {
			wrapped := "<" + tc.outerTag + "><" + tc.innerTag + "/>" + escaped +
				"</" + tc.outerTag + ">" + visibleText
			if got := stripHTML(wrapped); got != visibleText {
				t.Fatalf("stripHTML(%q) = %q, want %q", wrapped, got, visibleText)
			}
		}
	})
}

// stripHTMLBenchInputs are representative entry bodies: a short feed
// excerpt, a typical article summary, a pathological large payload, and a
// feed-cap-sized (10MB, rss.maxFeedResponseBytes) prefixed script body full of
// unclosed nested scripts, the worst case for the script lookahead budget.
func stripHTMLBenchInputs() map[string]string {
	short := `<p>Some excerpt text with a <a href="https://example.com/a">link</a>.</p>` +
		`<a href="https://example.com/full">Continue reading...</a>`
	article := strings.Repeat(
		`<p class="para">Lorem <b>ipsum</b> dolor sit amet, <a href="https://example.com/x?a=1&amp;b=2" title="t">consectetur</a> `+
			`adipiscing elit &amp; sed do eiusmod.</p><ul><li>one</li><li>two</li></ul><br/>`, 20)
	large := strings.Repeat(article, 40)
	unit := "<!----!>" + scriptStartTag
	nestedScripts := prefixedScriptStartTag + strings.Repeat(unit, 10<<20/len(unit)) + prefixedScriptEndTag
	return map[string]string{"short": short, "article": article, "large": large, "nested-scripts": nestedScripts}
}

func BenchmarkStripHTML(b *testing.B) {
	for _, name := range []string{"short", "article", "large", "nested-scripts"} {
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

func TestHTTPURLOrEmpty(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "https kept", input: testArticleURL, want: testArticleURL},
		{name: "http kept", input: "http://example.com/a", want: "http://example.com/a"},
		{name: "javascript dropped", input: jsAlertURL, want: ""},
		{name: "data dropped", input: "data:text/html,<script>", want: ""},
		{name: "empty dropped", input: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpURLOrEmpty(tc.input); got != tc.want {
				t.Fatalf("httpURLOrEmpty(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
