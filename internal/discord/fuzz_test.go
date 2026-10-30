package discord

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzEmbedPayload exercises Fit/embedToPayload/Truncate -- and the json.Marshal of the resulting payload -- against
// arbitrary embed field strings. Feed entry titles/descriptions/authors are
// untrusted external text (the same threat model as FuzzParseFeed in
// internal/rss), so these fields must never panic or defeat Discord's
// length/mention-suppression guarantees regardless of how adversarial the
// input is. Go fuzz-generated strings, unlike string literals, aren't
// required to be valid UTF-8, so seeds/invariants below account for that.
func FuzzEmbedPayload(f *testing.F) {
	seeds := []string{
		"",
		"plain title",
		"@everyone",
		"@here",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"\x00\x00\x00",
		"\xff\xfe\x80", // invalid UTF-8 byte sequence
		strings.Repeat("x", MaxEmbedTotalLength*2),
		strings.Repeat("\U0001F600", MaxEmbedTotalLength), // 4-byte emoji straddling the clamp boundary
		"é́́",    // base char with combining acute accents
		"日本語テスト", // multibyte (Japanese) text
	}
	for _, title := range seeds {
		for _, description := range seeds {
			f.Add(title, description, "author", "footer", "https://example.com")
		}
	}

	f.Fuzz(func(t *testing.T, title, description, author, footer, url string) {
		e := Embed{Title: title, Description: description, AuthorName: author, FooterText: footer, URL: url}

		fitted, trimmed := Fit(Message{Content: description, ThreadName: title, Embed: &e})
		if trimmed < 0 {
			t.Fatalf("Fit reported negative trimmed count %d", trimmed)
		}
		if n := len([]rune(fitted.Content)); n > MaxContentLength {
			t.Fatalf("fitted content has %d runes, want <= %d", n, MaxContentLength)
		}
		if n := len([]rune(fitted.ThreadName)); n > MaxThreadNameLength {
			t.Fatalf("fitted thread name has %d runes, want <= %d", n, MaxThreadNameLength)
		}
		fe := fitted.Embed
		if n := len([]rune(fe.Title)); n > MaxEmbedTitleLength {
			t.Fatalf("fitted title has %d runes, want <= %d", n, MaxEmbedTitleLength)
		}
		if n := len([]rune(fe.Description)); n > MaxEmbedDescriptionLength {
			t.Fatalf("fitted description has %d runes, want <= %d", n, MaxEmbedDescriptionLength)
		}
		// Fit only ever trims Description for the combined cap, so the total
		// can exceed it only when the other fields alone do, with Description
		// already empty.
		total := len([]rune(fe.Title)) + len([]rune(fe.Description)) + len([]rune(fe.FooterText)) + len([]rune(fe.AuthorName))
		if total > MaxEmbedTotalLength && fe.Description != "" {
			t.Fatalf("Fit left a combined length of %d without emptying Description", total)
		}
		if _, again := Fit(fitted); again != 0 {
			t.Fatalf("Fit is not idempotent: second pass trimmed %d runes", again)
		}

		// Truncate re-encodes through []rune, so whenever it cuts, its output
		// is valid UTF-8 even for invalid input bytes. The pass-through branch
		// returns the input untouched and carries no such guarantee.
		truncated, overflow := Truncate(description, MaxEmbedTotalLength)
		if got := len([]rune(truncated)); got > MaxEmbedTotalLength {
			t.Fatalf("Truncate(%d runes, %d) returned %d runes", len([]rune(description)), MaxEmbedTotalLength, got)
		}
		if want := max(len([]rune(description))-MaxEmbedTotalLength, 0); overflow != want {
			t.Fatalf("Truncate overflow = %d, want %d", overflow, want)
		}
		if overflow > 0 && !utf8.ValidString(truncated) {
			t.Fatalf("Truncate output is not valid UTF-8: %q", truncated)
		}

		payload := embedToPayload(e)
		full := discordPayload{
			Embeds:          []discordEmbed{payload},
			AllowedMentions: discordAllowedMentions{Parse: []string{}},
		}
		body, err := json.Marshal(full)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		// The @everyone/@here suppression invariant: allowed_mentions must
		// never be omitted or nil, regardless of what ends up in the embed.
		if !strings.Contains(string(body), `"allowed_mentions":{"parse":[]}`) {
			t.Fatalf("marshaled payload missing empty allowed_mentions.parse: %s", body)
		}
		// json.Marshal replaces invalid UTF-8 with U+FFFD rather than
		// erroring, so the wire payload is always well-formed JSON
		// regardless of input filth -- confirm it actually round-trips.
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("marshaled payload does not round-trip through json.Unmarshal: %v\n%s", err, body)
		}
	})
}
