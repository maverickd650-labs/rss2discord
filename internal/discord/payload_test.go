package discord

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// fiveRunes is reused across the truncateRunes test cases below.
const fiveRunes = "abcde"

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
		{name: "malformed defaults to 1s", value: "soon", want: time.Second},
		{name: "negative defaults to 1s", value: "-3", want: time.Second},
		{name: "NaN defaults to 1s", value: "NaN", want: time.Second},
		{name: "huge is clamped", value: "86400", want: maxRetryAfter},
		{name: "infinity is clamped", value: "Inf", want: maxRetryAfter},
		{name: "exact one hour cap", value: "3600", want: time.Hour},
		{name: "fraction below cap", value: "3599.5", want: time.Hour - 500*time.Millisecond},
		{name: "fraction above cap", value: "3600.5", want: time.Hour},
		{name: "scientific notation at cap", value: "3.6e3", want: time.Hour},
		{name: "huge finite float", value: "1e300", want: time.Hour},
		{name: "positive infinity", value: "+Inf", want: time.Hour},
		{name: "negative infinity defaults to 1s", value: "-Inf", want: time.Second},
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
		{name: "large finite delay", header: "86400", want: time.Hour},
		{name: "duration conversion overflow", header: "1e300", want: time.Hour},
		{name: "infinite delay", header: "+Inf", want: time.Hour},
		{name: "NaN fallback", header: "NaN", want: time.Second},
		{name: "negative infinity fallback", header: "-Inf", want: time.Second},
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

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name  string
		runes string
		max   int
		want  string
	}{
		{name: "under limit unchanged", runes: fiveRunes, max: 10, want: fiveRunes},
		{name: "exactly at limit unchanged", runes: fiveRunes, max: 5, want: fiveRunes},
		{name: "zero max yields empty", runes: fiveRunes, max: 0, want: ""},
		{name: "negative max yields empty", runes: fiveRunes, max: -1, want: ""},
		{name: "truncates with ellipsis", runes: "hello world", max: 5, want: "hell…"},
		// The ellipsis is multi-byte but a single rune, so a multibyte body is
		// trimmed by rune count, not byte count.
		{name: "multibyte body trimmed by rune", runes: "日本語テスト", max: 3, want: "日本…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes([]rune(tc.runes), tc.max); got != tc.want {
				t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tc.runes, tc.max, got, tc.want)
			}
		})
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
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
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad"}`))
	}))
	defer srv.Close()

	AllowedWebhookHosts["127.0.0.1"] = true
	defer delete(AllowedWebhookHosts, "127.0.0.1")

	c := NewClientWithHTTP(srv.URL, srv.Client())
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

func TestClampEmbedTotalLength_UnderLimitUnchanged(t *testing.T) {
	e := Embed{Title: "t", Description: "d", FooterText: "f", AuthorName: "a"}
	got := clampEmbedTotalLength(e)
	if got.Description != "d" {
		t.Fatalf("expected description unchanged under limit, got %q", got.Description)
	}
}

// TestEmbedTotalLengthOverflow_MatchesClamp asserts the overflow reported by
// the exported EmbedTotalLengthOverflow (which a caller uses purely for
// metrics) is exactly how much clampEmbedTotalLength actually trims, so a
// caller folding this into its own truncation count doesn't over- or
// under-report.
func TestEmbedTotalLengthOverflow_MatchesClamp(t *testing.T) {
	e := Embed{Title: "t", Description: strings.Repeat("d", maxEmbedTotalLength), FooterText: "f", AuthorName: "a"}

	overflow := EmbedTotalLengthOverflow(e)
	if overflow == 0 {
		t.Fatal("expected a non-zero overflow when combined fields exceed maxEmbedTotalLength")
	}

	clamped := clampEmbedTotalLength(e)
	wantDescLen := len([]rune(e.Description)) - overflow
	if got := len([]rune(clamped.Description)); got != wantDescLen {
		t.Fatalf("clamped description length = %d, want %d (overflow %d)", got, wantDescLen, overflow)
	}
}

func TestEmbedTotalLengthOverflow_UnderLimitIsZero(t *testing.T) {
	e := Embed{Title: "t", Description: "d", FooterText: "f", AuthorName: "a"}
	if got := EmbedTotalLengthOverflow(e); got != 0 {
		t.Fatalf("expected 0 overflow under the limit, got %d", got)
	}
}
