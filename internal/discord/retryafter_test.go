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

// requireRateLimited fails unless err is a *RateLimitError carrying wantRetry.
func requireRateLimited(t *testing.T, stage string, err error, wantRetry time.Duration) {
	t.Helper()
	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("%s: err=%v, want *RateLimitError", stage, err)
	}
	if rle.RetryAfter != wantRetry {
		t.Fatalf("%s: RetryAfter=%v, want %v", stage, rle.RetryAfter, wantRetry)
	}
}

// requireRequests fails unless the HTTP client has seen exactly want requests.
func requireRequests(t *testing.T, stage string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: requests=%d, want %d", stage, got, want)
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
				first := NewClient(testWebhookURL, WithHTTPClient(httpClient), WithRateLimiter(limiter))
				err := first.SendMessageText(t.Context(), "first")
				requireRateLimited(t, "first send", err, tc.want)
				requireRequests(t, "first send", requests, 1)

				// Fake time verifies expiry without sleeping for a real hour.
				time.Sleep(tc.want - time.Nanosecond)
				second := NewClient(testWebhookURL, WithHTTPClient(httpClient), WithRateLimiter(limiter))
				err = second.SendMessageText(t.Context(), "held")
				requireRateLimited(t, "before expiry", err, time.Nanosecond)
				requireRequests(t, "before expiry", requests, 1)

				time.Sleep(time.Nanosecond)
				err = second.SendMessageText(t.Context(), "retry")
				if err != nil {
					t.Fatalf("after expiry: err=%v; want a successful second request", err)
				}
				requireRequests(t, "after expiry", requests, 2)
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

func TestSendMessage_ReturnsRateLimitError(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
	err := c.SendMessageText(t.Context(), "rate limited test")

	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("expected *RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 2*time.Second {
		t.Fatalf("expected RetryAfter 2s, got %v", rle.RetryAfter)
	}
}
