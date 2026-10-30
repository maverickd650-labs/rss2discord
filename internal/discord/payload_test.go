package discord

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestDecodeErrorDetail(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty body", body: "", want: ""},
		{name: "empty object", body: `{}`, want: ""},
		{name: "message only", body: `{"message":"invalid webhook token"}`, want: ": invalid webhook token"},
		{name: "code and message", body: `{"message":"Unknown Webhook","code":10015}`, want: ": Unknown Webhook (code 10015)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeErrorDetail(strings.NewReader(tc.body)); got != tc.want {
				t.Fatalf("decodeErrorDetail(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// Retry-After header values shared by the parse and cooldown tables.
const (
	retryAfterDay    = "86400"
	retryAfterHuge   = "1e300"
	retryAfterPosInf = "+Inf"
	retryAfterNaN    = "NaN"
	retryAfterNegInf = "-Inf"
	retryAfterJunk   = "soon"
	// retryAfterBillion is far past maxCooldown (about 31 years).
	retryAfterBillion = "1e9"
	// retryAfterCap is exactly maxCooldown in seconds; retryAfterInt64NS is the
	// smallest whole-second value whose nanosecond count overflows an int64.
	retryAfterCap     = "3600"
	retryAfterInt64NS = "9223372036"
)

func TestBoundedSeconds(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        time.Duration
		wantOK      bool
	}{
		{name: "zero is valid", value: "0", want: 0, wantOK: true},
		{name: "fractional with whitespace", value: " 2.5 ", want: 2500 * time.Millisecond, wantOK: true},
		{name: "just under the cap", value: "3599.5", want: 3599*time.Second + 500*time.Millisecond, wantOK: true},
		{name: "exactly the cap", value: retryAfterCap, want: maxCooldown, wantOK: true},
		{name: "just over the cap", value: "3601", want: maxCooldown, wantOK: true},
		{name: "a day", value: retryAfterDay, want: maxCooldown, wantOK: true},
		{name: "int64 nanosecond overflow", value: retryAfterInt64NS, want: maxCooldown, wantOK: true},
		{name: "huge float", value: retryAfterHuge, want: maxCooldown, wantOK: true},
		{name: "bounded +Inf", value: retryAfterPosInf, want: maxCooldown, wantOK: true},
		{name: "NaN", value: retryAfterNaN},
		{name: "negative infinity", value: retryAfterNegInf},
		{name: "negative", value: "-1"},
		{name: "not a number", value: retryAfterJunk},
		{name: "empty", value: ""},
		{name: "blank", value: "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := boundedSeconds(tc.value)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("boundedSeconds(%q) = (%v, %v), want (%v, %v)", tc.value, got, ok, tc.want, tc.wantOK)
			}
			if got < 0 || got > maxCooldown {
				t.Fatalf("boundedSeconds(%q) = %v, outside [0, %v]", tc.value, got, maxCooldown)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "empty defaults to 1s", value: "", want: time.Second},
		{name: "whitespace defaults to 1s", value: "  ", want: time.Second},
		{name: "integer seconds", value: "5", want: 5 * time.Second},
		{name: "fractional seconds", value: "0.5", want: 500 * time.Millisecond},
		{name: "malformed defaults to 1s", value: retryAfterJunk, want: time.Second},
		{name: "negative defaults to 1s", value: "-3", want: time.Second},
		{name: "NaN defaults to 1s", value: retryAfterNaN, want: time.Second},
		{name: "huge is clamped", value: retryAfterDay, want: maxCooldown},
		{name: "infinity is clamped", value: "Inf", want: maxCooldown},
		{name: "exact one hour cap", value: "3600", want: time.Hour},
		{name: "fraction below cap", value: "3599.5", want: time.Hour - 500*time.Millisecond},
		{name: "fraction above cap", value: "3600.5", want: time.Hour},
		{name: "scientific notation at cap", value: "3.6e3", want: time.Hour},
		{name: "huge finite float", value: retryAfterHuge, want: time.Hour},
		{name: "positive infinity", value: retryAfterPosInf, want: time.Hour},
		{name: "negative infinity defaults to 1s", value: retryAfterNegInf, want: time.Second},
		{name: "padded NaN defaults to 1s", value: " \tNaN\r\n", want: time.Second},
		{name: "padded delay is clamped", value: " \t86400\r\n", want: time.Hour},
		{name: "float parse overflow defaults to 1s", value: "1e309", want: time.Second},
		{name: "negative float overflow defaults to 1s", value: "-1e309", want: time.Second},
		{name: "zero remains immediate", value: "0", want: 0},
		{name: "negative zero remains immediate", value: "-0", want: 0},
		{name: "padded fractional delay", value: " \t1.25\r\n", want: 1250 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestSendMessage_BoundedRetryAfterControlsCooldown(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		want         time.Duration
	}{
		{name: "large finite delay", header: retryAfterDay, want: time.Hour},
		{name: "duration conversion overflow", header: retryAfterHuge, want: time.Hour},
		{name: "infinite delay", header: retryAfterPosInf, want: time.Hour},
		{name: "NaN fallback", header: retryAfterNaN, want: time.Second},
		{name: "negative infinity fallback", header: retryAfterNegInf, want: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					status := http.StatusNoContent
					if requests == 1 {
						status = http.StatusTooManyRequests
					}
					return &http.Response{
						StatusCode: status,
						Header:     http.Header{"Retry-After": {tc.header}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				})}
				limiter := NewRateLimiter()
				first := NewClientWithLimiter(testWebhookURL, httpClient, limiter)
				err := first.SendMessageText(t.Context(), "first")
				rle, ok := errors.AsType[*RateLimitError](err)
				if !ok || rle.RetryAfter != tc.want || requests != 1 {
					t.Fatalf("first send: err=%v, requests=%d; want rate-limit error with %v and one request", err, requests, tc.want)
				}

				// Fake time verifies expiry without sleeping for a real hour.
				time.Sleep(tc.want - time.Nanosecond)
				second := NewClientWithLimiter(testWebhookURL, httpClient, limiter)
				err = second.SendMessageText(t.Context(), "held")
				rle, ok = errors.AsType[*RateLimitError](err)
				if !ok || rle.RetryAfter != time.Nanosecond || requests != 1 {
					t.Fatalf("before expiry: err=%v, requests=%d; want 1ns cooldown and no additional request", err, requests)
				}

				time.Sleep(time.Nanosecond)
				if err := second.SendMessageText(t.Context(), "retry"); err != nil || requests != 2 {
					t.Fatalf("after expiry: err=%v, requests=%d; want a successful second request", err, requests)
				}
			})
		})
	}
}

func TestRateLimitError_Error(t *testing.T) {
	err := &RateLimitError{RetryAfter: 3 * time.Second}
	if !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "3s") {
		t.Fatalf("unexpected error string: %q", err.Error())
	}
}

func TestSendMessage_EmptyWebhookURL(t *testing.T) {
	c := NewClient("")
	err := c.SendMessageText(t.Context(), "empty webhook test")
	if err == nil || !strings.Contains(err.Error(), "webhook URL is empty") {
		t.Fatalf("expected empty-URL error, got %v", err)
	}
}

func TestSendMessage_EmptyContentAndEmbed(t *testing.T) {
	c := NewClient("https://discord.com/api/webhooks/1/abc")
	err := c.SendMessage(t.Context(), Message{})
	if err == nil || !strings.Contains(err.Error(), "content is empty") {
		t.Fatalf("expected empty-content error, got %v", err)
	}
}

func TestSendMessage_InvalidWebhookURL(t *testing.T) {
	c := NewClient("://not a url")
	err := c.SendMessageText(t.Context(), "invalid url test")
	if err == nil {
		t.Fatal("expected error for malformed webhook URL, got nil")
	}
}

func TestSendMessage_ReturnsRateLimitError(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessageText(t.Context(), "rate limited test")

	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("expected *RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 2*time.Second {
		t.Fatalf("expected RetryAfter 2s, got %v", rle.RetryAfter)
	}
}

func TestSendMessage_ReturnsErrorOnNon2xx(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad"}`))
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessageText(t.Context(), "non-2xx test")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
}

func TestEmbedToPayload_OmitsUnsetMedia(t *testing.T) {
	// An embed with only a title should produce no thumbnail/author/footer
	// sub-objects, so they stay omitted from the JSON.
	p := embedToPayload(Embed{Title: "only title"})
	if p.Thumbnail != nil || p.Author != nil || p.Footer != nil {
		t.Fatalf("expected unset embed media to be nil, got %+v", p)
	}
	if p.Title != "only title" {
		t.Fatalf("expected title preserved, got %q", p.Title)
	}
}

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

func TestFit_UnderLimitsUnchanged(t *testing.T) {
	msg := Message{
		Content:    "c",
		ThreadName: "thread",
		Embed:      &Embed{Title: "t", Description: "d", FooterText: "f", AuthorName: "a"},
	}
	got, trimmed := Fit(msg)
	if trimmed != 0 || got.Content != "c" || got.ThreadName != "thread" || *got.Embed != *msg.Embed {
		t.Fatalf("Fit changed an in-limit message: trimmed=%d got=%+v", trimmed, got)
	}
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
		Description: strings.Repeat("d", 3000),
		FooterText:  strings.Repeat("f", 2000),
		AuthorName:  strings.Repeat("a", 1000),
	}
	// 6200 combined: 200 over the cap, all taken from Description.
	got, trimmed := Fit(Message{Embed: e})
	if trimmed != 200 {
		t.Fatalf("trimmed = %d, want 200", trimmed)
	}
	if n := len([]rune(got.Embed.Description)); n != 2800 {
		t.Fatalf("description length = %d, want 2800", n)
	}
	if got.Embed.Title != e.Title || got.Embed.FooterText != e.FooterText || got.Embed.AuthorName != e.AuthorName {
		t.Fatal("combined clamp must only trim Description")
	}

	// Per-field overflow (description 4096 -> 5000 runes, 904 over) is counted
	// first; the combined overflow is then measured on what is left.
	e2 := &Embed{
		Title:       "t",
		Description: strings.Repeat("d", 5000),
		FooterText:  strings.Repeat("f", 1500),
		AuthorName:  strings.Repeat("a", 1000),
	}
	// After the per-field trim: 1+4096+1500+1000 = 6597, 597 over the cap.
	if _, trimmed := Fit(Message{Embed: e2}); trimmed != 904+597 {
		t.Fatalf("trimmed = %d, want %d", trimmed, 904+597)
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
	if second != 0 || twice.Content != once.Content || twice.ThreadName != once.ThreadName || *twice.Embed != *once.Embed {
		t.Fatalf("Fit is not idempotent: second trimmed %d", second)
	}
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

func TestFit_OtherFieldsAloneOverCap(t *testing.T) {
	e := &Embed{Description: "desc", FooterText: strings.Repeat("f", 4000), AuthorName: strings.Repeat("a", 4000)}
	got, trimmed := Fit(Message{Embed: e})
	if got.Embed.Description != "" || trimmed != 4 {
		t.Fatalf("description=%q trimmed=%d; want empty description and 4 trimmed", got.Embed.Description, trimmed)
	}
	if _, again := Fit(Message{Embed: got.Embed}); again != 0 {
		t.Fatalf("second Fit trimmed %d, want 0", again)
	}
}
