package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newRateLimitedServer starts a webhook server that always answers 429 with
// Retry-After: 60 and returns its webhook URL, a client for it and a pointer
// to the count of requests it has served.
func newRateLimitedServer(t *testing.T) (webhookURL string, httpClient *http.Client, requests *int) {
	t.Helper()
	requests = new(int)
	webhookURL, httpClient = newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	return webhookURL, httpClient, requests
}

func TestSendMessage_SharedRateLimiterCooldownSkipsRequest(t *testing.T) {
	webhookURL, httpClient, requests := newRateLimitedServer(t)

	limiter := NewRateLimiter()
	// Two separate *Client instances for the same webhook, as
	// DiscordClientBuilder constructs one per FeedGroup reconcile -- sharing
	// a limiter is what makes one FeedGroup's 429 also hold off another.
	c1 := NewClient(webhookURL, WithHTTPClient(httpClient), WithRateLimiter(limiter))
	c2 := NewClient(webhookURL, WithHTTPClient(httpClient), WithRateLimiter(limiter))

	// First send actually hits the server and gets 429'd, which records the
	// cooldown.
	if _, ok := errors.AsType[*RateLimitError](c1.SendMessageText(t.Context(), "first")); !ok {
		t.Fatal("expected the first send to reach the server and return *RateLimitError")
	}
	if *requests != 1 {
		t.Fatalf("expected exactly 1 request so far, got %d", *requests)
	}

	// A second Client for the same webhook, sharing the limiter, must be
	// short-circuited by the cooldown rather than issuing another request.
	err := c2.SendMessageText(t.Context(), "second")
	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("expected *RateLimitError from the shared cooldown, got %v", err)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > 60*time.Second {
		t.Fatalf("expected RetryAfter in (0, 60s], got %v", rle.RetryAfter)
	}
	if *requests != 1 {
		t.Fatalf("expected the second send to be short-circuited without a request, got %d requests", *requests)
	}
}

func TestSendMessage_NilRateLimiterDoesNotCooldown(t *testing.T) {
	webhookURL, httpClient, requests := newRateLimitedServer(t)

	// NewClient without a limiter must behave exactly as before this
	// change: every send reaches the server, with no cross-Client cooldown.
	c := NewClient(webhookURL, WithHTTPClient(httpClient))
	_, _ = c.SendMessageText(t.Context(), "first"), c.SendMessageText(t.Context(), "second")
	if *requests != 2 {
		t.Fatalf("expected both sends to reach the server without a shared limiter, got %d requests", *requests)
	}
}

// newBucketServer starts a webhook server that answers 204 with the given
// rate-limit headers and returns its webhook URL, a client for it and the
// arrival time of each request it has served.
func newBucketServer(t *testing.T, remaining, resetAfter string) (webhookURL string, httpClient *http.Client, arrivals *[]time.Time) {
	t.Helper()
	arrivals = new([]time.Time)
	webhookURL, httpClient = newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*arrivals = append(*arrivals, time.Now())
		w.Header().Set("X-RateLimit-Remaining", remaining)
		w.Header().Set("X-RateLimit-Reset-After", resetAfter)
		w.WriteHeader(http.StatusNoContent)
	}))
	return webhookURL, httpClient, arrivals
}

// TestSendMessage_PacesWhenBucketEmpty asserts that a success response
// reporting an empty bucket (X-RateLimit-Remaining: 0) makes the next send
// through the same limiter wait for Reset-After rather than hit the server
// early and get 429'd -- and that it does so without returning an error.
func TestSendMessage_PacesWhenBucketEmpty(t *testing.T) {
	webhookURL, httpClient, arrivals := newBucketServer(t, "0", "0.2")

	c := NewClient(webhookURL, WithHTTPClient(httpClient), WithRateLimiter(NewRateLimiter()))
	for _, text := range []string{"one", "two"} {
		if err := c.SendMessageText(t.Context(), text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
	}
	if len(*arrivals) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(*arrivals))
	}
	if gap := (*arrivals)[1].Sub((*arrivals)[0]); gap < 150*time.Millisecond {
		t.Fatalf("second send arrived %v after the first, want it paced to ~200ms", gap)
	}
}

// TestSendMessage_LongResetBecomesCooldown asserts a bucket that won't refill
// within maxPaceWait is surfaced as a RateLimitError instead of blocking.
func TestSendMessage_LongResetBecomesCooldown(t *testing.T) {
	webhookURL, httpClient, arrivals := newBucketServer(t, "0", "30")

	c := NewClient(webhookURL, WithHTTPClient(httpClient), WithRateLimiter(NewRateLimiter()))
	if err := c.SendMessageText(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, ok := errors.AsType[*RateLimitError](c.SendMessageText(t.Context(), "two")); !ok {
		t.Fatal("expected *RateLimitError for a reset beyond maxPaceWait")
	}
	if len(*arrivals) != 1 {
		t.Fatalf("expected the second send to be short-circuited, got %d requests", len(*arrivals))
	}
}

// TestRateLimiterWait_ContextCancel asserts a paced wait ends when the
// caller's context does.
func TestRateLimiterWait_ContextCancel(t *testing.T) {
	l := NewRateLimiter()
	l.pace(testWebhookURL, time.Minute)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := l.wait(ctx, testWebhookURL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait() = %v, want context.DeadlineExceeded", err)
	}
}

// TestNotePacing_IgnoresUnusableHeaders asserts a success response only
// paces the next send when it reports an empty bucket with a usable
// Reset-After; anything else must leave sends unthrottled.
func TestNotePacing_IgnoresUnusableHeaders(t *testing.T) {
	cases := []struct {
		name      string
		remaining string
		reset     string
	}{
		{name: "no headers"},
		{name: "bucket not empty", remaining: "3", reset: "1"},
		{name: "reset missing", remaining: "0"},
		{name: "reset malformed", remaining: "0", reset: retryAfterJunk},
		{name: "reset zero", remaining: "0", reset: "0"},
		{name: "reset negative", remaining: "0", reset: "-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limiter := NewRateLimiter()
			c := NewClient(testWebhookURL, WithRateLimiter(limiter))
			h := http.Header{}
			if tc.remaining != "" {
				h.Set("X-RateLimit-Remaining", tc.remaining)
			}
			if tc.reset != "" {
				h.Set("X-RateLimit-Reset-After", tc.reset)
			}
			c.notePacing(c.webhookURL, h)
			if d := limiter.paceDelay(testWebhookURL); d > 0 {
				t.Errorf("paceDelay = %v, want no pacing", d)
			}
			if _, cooling := limiter.reserve(testWebhookURL); cooling {
				t.Error("unusable headers must not start a hard cooldown either")
			}
		})
	}
}

// TestNotePacing_NilLimiterIsNoop asserts a Client built without a limiter
// ignores rate-limit headers instead of dereferencing nil.
func TestNotePacing_NilLimiterIsNoop(t *testing.T) {
	c := NewClient(testWebhookURL)
	c.notePacing(c.webhookURL, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {"1"}})
}

// TestSendMessage_ContextCanceledWhilePaced asserts a send stuck waiting on a
// pacing delay returns the context's error promptly and never reaches the
// server.
func TestSendMessage_ContextCanceledWhilePaced(t *testing.T) {
	webhookURL, httpClient, arrivals := newBucketServer(t, "5", "1")

	limiter := NewRateLimiter()
	limiter.pace(webhookURL, maxPaceWait)
	c := NewClient(webhookURL, WithHTTPClient(httpClient), WithRateLimiter(limiter))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.SendMessageText(ctx, "hello")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendMessageText() = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned after %v, want it to give up when the context ended", elapsed)
	}
	if len(*arrivals) != 0 {
		t.Errorf("server saw %d requests, want none", len(*arrivals))
	}
}

// TestPaceDelay_PrunesExpiredEntries asserts expired pacing entries are
// dropped across the whole map, like reserve does for cooldowns, so a webhook
// that is never sent to again doesn't linger.
func TestPaceDelay_PrunesExpiredEntries(t *testing.T) {
	l := NewRateLimiter()
	l.pace("https://discord.com/api/webhooks/1/a", -time.Second)
	l.pace("https://discord.com/api/webhooks/2/b", time.Minute)

	if d := l.paceDelay("https://discord.com/api/webhooks/2/b"); d <= 0 {
		t.Fatalf("live entry reported delay %v", d)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if got := len(l.pacing); got != 1 {
		t.Errorf("pacing map has %d entries after pruning, want 1", got)
	}
}

// TestNotePacing_ResetAfterIsBounded asserts every X-RateLimit-Reset-After
// value is validated before it can create a wait: usable values become pacing
// (up to maxPaceWait) or a hard cooldown no longer than maxCooldown, and
// unusable ones (NaN, -Inf) start nothing.
func TestNotePacing_ResetAfterIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name, reset  string
		wantPace     bool
		wantCooldown bool
		maxRemaining time.Duration
	}{
		{name: "at the pacing limit", reset: "5", wantPace: true},
		{name: "just over the pacing limit", reset: "6", wantCooldown: true, maxRemaining: 6 * time.Second},
		{name: "at the cap", reset: retryAfterCap, wantCooldown: true, maxRemaining: maxCooldown},
		{name: "a billion seconds", reset: retryAfterBillion, wantCooldown: true, maxRemaining: maxCooldown},
		{name: "int64 nanosecond overflow", reset: retryAfterInt64NS, wantCooldown: true, maxRemaining: maxCooldown},
		{name: "huge float", reset: retryAfterHuge, wantCooldown: true, maxRemaining: maxCooldown},
		{name: "reset-after +Inf", reset: retryAfterPosInf, wantCooldown: true, maxRemaining: maxCooldown},
		{name: "NaN", reset: retryAfterNaN},
		{name: "negative infinity", reset: retryAfterNegInf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := NewRateLimiter()
			c := NewClient(testWebhookURL, WithRateLimiter(limiter))
			h := http.Header{}
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset-After", tc.reset)
			c.notePacing(c.webhookURL, h)

			if pace := limiter.paceDelay(testWebhookURL); (pace > 0) != tc.wantPace || pace > maxPaceWait {
				t.Errorf("paceDelay = %v, want pacing=%v and at most %v", pace, tc.wantPace, maxPaceWait)
			}
			remaining, cooling := limiter.reserve(testWebhookURL)
			if cooling != tc.wantCooldown || remaining > tc.maxRemaining {
				t.Errorf("cooldown remaining=%v cooling=%v, want cooling=%v and at most %v", remaining, cooling, tc.wantCooldown, tc.maxRemaining)
			}
		})
	}
}

// TestSendMessage_ExcessiveResetAfterGivesABoundedRateLimit asserts the whole
// path: a success response with an absurd Reset-After makes the next send fail
// fast with a RateLimitError whose wait is finite, not decades.
func TestSendMessage_ExcessiveResetAfterGivesABoundedRateLimit(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {retryAfterBillion}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	c := NewClient(testWebhookURL, WithHTTPClient(&http.Client{Transport: transport}), WithRateLimiter(NewRateLimiter()))

	if err := c.SendMessageText(t.Context(), "first"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	err := c.SendMessageText(t.Context(), "second")
	rl, ok := errors.AsType[*RateLimitError](err)
	if !ok {
		t.Fatalf("second send error = %v, want a *RateLimitError", err)
	}
	if rl.RetryAfter <= maxPaceWait || rl.RetryAfter > maxCooldown {
		t.Fatalf("RetryAfter = %v, want in (%v, %v]", rl.RetryAfter, maxPaceWait, maxCooldown)
	}
}
