package discord

import (
	"reflect"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	const (
		short      = "short"
		truncateMe = "truncate me"
	)
	cases := []struct {
		name         string
		content      string
		max          int
		want         string
		wantOverflow int
	}{
		{name: "under limit unchanged", content: short, max: 10, want: short},
		{name: "exactly at limit unchanged", content: short, max: 5, want: short},
		{name: "truncated with ellipsis", content: truncateMe, max: 5, want: "trun…", wantOverflow: len([]rune(truncateMe)) - 5},
		{name: "multibyte trimmed by rune", content: "日本語テスト", max: 3, want: "日本…", wantOverflow: 3},
		{name: "multibyte under byte length but within rune limit", content: "日本語", max: 3, want: "日本語"},
		{name: "max one keeps single rune without ellipsis", content: truncateMe, max: 1, want: "t", wantOverflow: len([]rune(truncateMe)) - 1},
		{name: "max zero yields empty", content: truncateMe, max: 0, want: "", wantOverflow: len([]rune(truncateMe))},
		{name: "negative max yields empty", content: truncateMe, max: -5, want: "", wantOverflow: len([]rune(truncateMe)) + 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, overflow := Truncate(tc.content, tc.max)
			if got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.content, tc.max, got, tc.want)
			}
			if overflow != tc.wantOverflow {
				t.Fatalf("Truncate(%q, %d) overflow = %d, want %d", tc.content, tc.max, overflow, tc.wantOverflow)
			}
		})
	}
}

// requireSameMessage fails unless got and want agree on content, thread name
// and embed contents (not embed pointer identity).
func requireSameMessage(t *testing.T, got, want Message) {
	t.Helper()
	if got.Content != want.Content {
		t.Fatalf("Content = %q, want %q", got.Content, want.Content)
	}
	if got.ThreadName != want.ThreadName {
		t.Fatalf("ThreadName = %q, want %q", got.ThreadName, want.ThreadName)
	}
	if !reflect.DeepEqual(got.Embed, want.Embed) {
		t.Fatalf("Embed = %+v, want %+v", got.Embed, want.Embed)
	}
}

// embedWithoutDescription returns e with Description cleared, to compare
// everything the combined clamp must leave alone.
func embedWithoutDescription(e Embed) Embed {
	e.Description = ""
	return e
}

// authorFooterLinks is the author/footer URL and icon fields of e.
func authorFooterLinks(e Embed) [3]string {
	return [3]string{e.AuthorURL, e.AuthorIconURL, e.FooterIconURL}
}

func TestFit_UnderLimitsUnchanged(t *testing.T) {
	msg := Message{
		Content:    "c",
		ThreadName: "thread",
		Embed:      &Embed{Title: "t", Description: "d", FooterText: "f", AuthorName: "a"},
	}
	got, trimmed := Fit(msg)
	if trimmed != 0 {
		t.Fatalf("Fit trimmed an in-limit message: trimmed=%d", trimmed)
	}
	requireSameMessage(t, got, msg)
}

func TestFit_PerFieldLimits(t *testing.T) {
	cases := []struct {
		name    string
		msg     Message
		check   func(Message) int // returns the field's rune length
		wantLen int
		trimmed int
	}{
		{"content", Message{Content: strings.Repeat("x", MaxContentLength+7)}, func(m Message) int { return len([]rune(m.Content)) }, MaxContentLength, 7},
		{"title", Message{Embed: &Embed{Title: strings.Repeat("x", MaxEmbedTitleLength+3)}}, func(m Message) int { return len([]rune(m.Embed.Title)) }, MaxEmbedTitleLength, 3},
		{"description", Message{Embed: &Embed{Description: strings.Repeat("x", MaxEmbedDescriptionLength+11)}}, func(m Message) int { return len([]rune(m.Embed.Description)) }, MaxEmbedDescriptionLength, 11},
		{"thread name", Message{ThreadName: strings.Repeat("x", MaxThreadNameLength+5)}, func(m Message) int { return len([]rune(m.ThreadName)) }, MaxThreadNameLength, 5},
		{"username", Message{Username: strings.Repeat("x", MaxUsernameLength+2)}, func(m Message) int { return len([]rune(m.Username)) }, MaxUsernameLength, 2},
		{"author name", Message{Embed: &Embed{AuthorName: strings.Repeat("x", MaxEmbedAuthorNameLength+4)}}, func(m Message) int { return len([]rune(m.Embed.AuthorName)) }, MaxEmbedAuthorNameLength, 4},
		{"footer text", Message{Embed: &Embed{FooterText: strings.Repeat("x", MaxEmbedFooterTextLength+6)}}, func(m Message) int { return len([]rune(m.Embed.FooterText)) }, MaxEmbedFooterTextLength, 6},
		{"username at limit", Message{Username: strings.Repeat("x", MaxUsernameLength)}, func(m Message) int { return len([]rune(m.Username)) }, MaxUsernameLength, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, trimmed := Fit(tc.msg)
			if n := tc.check(got); n != tc.wantLen {
				t.Fatalf("fitted length = %d, want %d", n, tc.wantLen)
			}
			if trimmed != tc.trimmed {
				t.Fatalf("trimmed = %d, want %d", trimmed, tc.trimmed)
			}
		})
	}
}

// TestFit_CombinedEmbedClamp checks the combined clamp is measured after the
// per-field trims, trims only Description, and sums with the per-field counts.
func TestFit_CombinedEmbedClamp(t *testing.T) {
	e := &Embed{
		Title:       strings.Repeat("t", 200),
		Description: strings.Repeat("d", 3700),
		FooterText:  strings.Repeat("f", MaxEmbedFooterTextLength),
		AuthorName:  strings.Repeat("a", 252),
	}
	// Every field is within its own limit; 6200 combined: 200 over the cap,
	// all taken from Description.
	got, trimmed := Fit(Message{Embed: e})
	if trimmed != 200 {
		t.Fatalf("trimmed = %d, want 200", trimmed)
	}
	if n := len([]rune(got.Embed.Description)); n != 3500 {
		t.Fatalf("description length = %d, want 3500", n)
	}
	if embedWithoutDescription(*got.Embed) != embedWithoutDescription(*e) {
		t.Fatal("combined clamp must only trim Description")
	}

	// Per-field overflow (description 4096 -> 5000 runes, 904 over) is counted
	// first; the combined overflow is then measured on what is left.
	e2 := &Embed{
		Title:       "t",
		Description: strings.Repeat("d", 5000),
		FooterText:  strings.Repeat("f", MaxEmbedFooterTextLength),
		AuthorName:  strings.Repeat("a", MaxEmbedAuthorNameLength),
	}
	// After the per-field trim: 1+4096+2048+256 = 6401, 401 over the cap.
	if _, trimmed := Fit(Message{Embed: e2}); trimmed != 904+401 {
		t.Fatalf("trimmed = %d, want %d", trimmed, 904+401)
	}
}

// The author and footer URL and icon fields aren't text Discord counts
// toward the 6000-character total, so Fit must pass them through unchanged
// and leave them out of the combined clamp, even with author name and
// footer text at their limits.
func TestFit_AuthorAndFooterURLsAreKeptAndNotCounted(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("u", 3000)
	e := &Embed{
		Description:   strings.Repeat("d", 3000),
		AuthorName:    strings.Repeat("a", MaxEmbedAuthorNameLength),
		AuthorURL:     longURL,
		AuthorIconURL: longURL + ".png",
		FooterText:    strings.Repeat("f", MaxEmbedFooterTextLength),
		FooterIconURL: longURL + ".ico",
	}
	// 3000+256+2048 = 5304 counted runes: under the cap, so nothing is
	// trimmed, however long the URLs are.
	got, trimmed := Fit(Message{Embed: e})
	if trimmed != 0 {
		t.Fatalf("trimmed = %d, want 0: URLs must not count toward the combined limit", trimmed)
	}
	if !reflect.DeepEqual(got.Embed, e) {
		t.Fatalf("Fit changed an embed within every limit: %+v", got.Embed)
	}

	// Over the per-field limits, only the author name and footer text are
	// cut; their URLs and icons survive untouched.
	e.AuthorName = strings.Repeat("a", MaxEmbedAuthorNameLength+5)
	e.FooterText = strings.Repeat("f", MaxEmbedFooterTextLength+7)
	got, trimmed = Fit(Message{Embed: e})
	if trimmed != 5+7 {
		t.Fatalf("trimmed = %d, want 12", trimmed)
	}
	if authorFooterLinks(*got.Embed) != authorFooterLinks(*e) {
		t.Fatal("Fit changed an author/footer URL")
	}
}

func TestFit_SumsEveryField(t *testing.T) {
	msg := Message{
		Content:    strings.Repeat("c", MaxContentLength+1),
		ThreadName: strings.Repeat("n", MaxThreadNameLength+2),
		Embed: &Embed{
			Title:       strings.Repeat("t", MaxEmbedTitleLength+3),
			Description: strings.Repeat("d", MaxEmbedDescriptionLength+4),
		},
	}
	if _, trimmed := Fit(msg); trimmed != 1+2+3+4 {
		t.Fatalf("trimmed = %d, want 10", trimmed)
	}
}

func TestFit_Idempotent(t *testing.T) {
	msg := Message{
		Content:    strings.Repeat("c", MaxContentLength*2),
		ThreadName: strings.Repeat("n", MaxThreadNameLength*2),
		Embed: &Embed{
			Title:       strings.Repeat("t", MaxEmbedTitleLength*2),
			Description: strings.Repeat("d", MaxEmbedDescriptionLength*2),
			FooterText:  strings.Repeat("f", 2000),
			AuthorName:  strings.Repeat("a", 2000),
		},
	}
	once, first := Fit(msg)
	if first == 0 {
		t.Fatal("expected the first Fit to trim")
	}
	twice, second := Fit(once)
	if second != 0 {
		t.Fatalf("Fit is not idempotent: second trimmed %d", second)
	}
	requireSameMessage(t, twice, once)
}

func TestFit_DoesNotMutateCallerEmbed(t *testing.T) {
	e := &Embed{Title: "t", Description: strings.Repeat("d", MaxEmbedDescriptionLength+100)}
	got, trimmed := Fit(Message{Embed: e})
	if trimmed != 100 {
		t.Fatalf("trimmed = %d, want 100", trimmed)
	}
	if got.Embed == e {
		t.Fatal("Fit must return a copy of the embed")
	}
	if len([]rune(e.Description)) != MaxEmbedDescriptionLength+100 {
		t.Fatal("Fit mutated the caller's embed")
	}
}

// TestClampEmbedTotalLength_OtherFieldsAloneOverCap covers the clamp's total
// fallback directly: Fit's per-field limits keep title+footer+author under the
// combined cap, so this case can't arise through Fit.
func TestClampEmbedTotalLength_OtherFieldsAloneOverCap(t *testing.T) {
	e := Embed{Description: "desc", FooterText: strings.Repeat("f", 4000), AuthorName: strings.Repeat("a", 4000)}
	got, trimmed := clampEmbedTotalLength(e)
	if got.Description != "" || trimmed != 4 {
		t.Fatalf("description=%q trimmed=%d; want empty description and 4 trimmed", got.Description, trimmed)
	}
	if _, again := clampEmbedTotalLength(got); again != 0 {
		t.Fatalf("second clamp trimmed %d, want 0", again)
	}
}
