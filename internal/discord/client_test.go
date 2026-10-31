package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// testWebhookHost is the host of every mock webhook URL: Discord's own, so the
// URL passes the production allowlist unchanged.
const testWebhookHost = "discord.com"

// testWebhookPath is the path of the webhook newWebhookServer serves.
const testWebhookPath = "/api/webhooks/123456789/mock-token"

// newWebhookServer starts h on httptest's in-memory network, shut down when
// the test ends, and returns a real https://discord.com webhook URL plus the
// client that reaches it. The client routes every request to the server
// regardless of host, so tests never register a host in AllowedWebhookHosts.
// h only sees requests for that webhook -- anything else gets a 404, as it
// would from Discord -- so a send to the wrong URL still fails.
func newWebhookServer(t testing.TB, h http.Handler) (webhookURL string, client *http.Client) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != testWebhookHost || r.URL.Path != testWebhookPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	}))
	return "https://" + testWebhookHost + testWebhookPath, srv.Client()
}

// newAbortingServer starts an in-memory server that aborts every response
// mid-request, forcing a transport error (the in-memory network has no refused
// connections to dial into).
func newAbortingServer(t testing.TB) *httptest.Server {
	return httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
}

func TestSendMessage_RejectsNonDiscordHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("attacker-controlled server should never receive a request")
	}))
	defer srv.Close()

	c := NewClient(srv.URL) // httptest URL is http://127.0.0.1:PORT, not discord.com
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for non-Discord webhook host, got nil")
	}
	if !strings.Contains(err.Error(), "discord.com") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewHTTPClientWithTransportWrap_WrapsAndPreservesRedirectRefusal(t *testing.T) {
	var wrapped bool
	wrap := func(rt http.RoundTripper) http.RoundTripper {
		wrapped = true
		return rt
	}

	client := NewHTTPClientWithTransportWrap(wrap)
	if !wrapped {
		t.Fatal("expected wrap to be invoked when building the client")
	}
	if client.CheckRedirect == nil {
		t.Fatal("expected CheckRedirect to still refuse redirects")
	}
}

func TestNewHTTPClientWithTransportWrap_NilWrapMatchesNewHTTPClient(t *testing.T) {
	client := NewHTTPClientWithTransportWrap(nil)
	if client.Transport == nil {
		t.Fatal("expected a non-nil transport")
	}
	if client.CheckRedirect == nil {
		t.Fatal("expected CheckRedirect to still refuse redirects")
	}
}

func TestSendMessage_DoesNotFollowRedirect(t *testing.T) {
	// An allowlisted host redirecting to an arbitrary attacker-controlled
	// host. NewHTTPClient's CheckRedirect must stop http.Client from
	// transparently following it and replaying the message content there.
	//
	// The in-memory client routes every host to this one server, so a
	// followed redirect would land back in the handler with the attacker's
	// Host -- that is how the test sees the attacker being reached.
	const attackerHost = "attacker.example"
	attackerHit := false
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == attackerHost {
			attackerHit = true
			return
		}
		http.Redirect(w, r, "https://"+attackerHost+"/steal", http.StatusFound)
	}))
	client := srv.Client()
	client.CheckRedirect = NewHTTPClient().CheckRedirect

	c := NewClientWithHTTP("https://"+testWebhookHost+testWebhookPath, client)
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected the unfollowed redirect's 3xx status to surface as an error")
	}
	if attackerHit {
		t.Fatal("redirect target must never receive the request")
	}
}

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
	c1 := NewClientWithLimiter(webhookURL, httpClient, limiter)
	c2 := NewClientWithLimiter(webhookURL, httpClient, limiter)

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

	// NewClientWithHTTP (no limiter) must behave exactly as before this
	// change: every send reaches the server, with no cross-Client cooldown.
	c := NewClientWithHTTP(webhookURL, httpClient)
	_, _ = c.SendMessageText(t.Context(), "first"), c.SendMessageText(t.Context(), "second")
	if *requests != 2 {
		t.Fatalf("expected both sends to reach the server without a shared limiter, got %d requests", *requests)
	}
}

func TestSendMessage_RejectsHTTPScheme(t *testing.T) {
	c := NewClient("http://discord.com/api/webhooks/123/abc")
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for non-https scheme, got nil")
	}
}

func TestSendMessage_AcceptsDiscordHost(t *testing.T) {
	requestReceived := false
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusNoContent)
	}))

	// NewClientWithHTTP lets the test supply a client trusting the
	// httptest TLS cert. The httptest server resolves to 127.0.0.1, so we
	// temporarily add it to the allow-list to exercise the "happy path"
	// without depending on a real discord.com endpoint.
	c := NewClientWithHTTP(webhookURL, httpClient)
	if err := c.SendMessageText(t.Context(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !requestReceived {
		t.Fatal("expected request to reach the test server")
	}
}

func TestSendMessage_SuppressesMentionsByDefault(t *testing.T) {
	var receivedBody string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessageText(t.Context(), "@everyone breaking news")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedBody, `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("expected allowed_mentions to suppress all mentions, got %q", receivedBody)
	}
}

func TestSendMessage_RedactsWebhookURLOnTransportError(t *testing.T) {
	// Aborting the response mid-request forces a transport error (the
	// in-memory network has no refused connections to dial into).
	reached := false
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		panic(http.ErrAbortHandler)
	}))
	httpClient := srv.Client()
	webhookURL := "https://" + testWebhookHost + "/api/webhooks/123456789/SUPER-SECRET-TOKEN"

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if !reached {
		t.Fatal("expected the request to reach the server before the connection was aborted")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-TOKEN") {
		t.Fatalf("error leaked webhook token: %v", err)
	}
}

func TestSendMessage_RedactsWebhookURLOnParseError(t *testing.T) {
	c := NewClient("https://discord.com/api/webhooks/123456789/SUPER-SECRET-TOKEN%zz")
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-TOKEN") {
		t.Fatalf("error leaked webhook token: %v", err)
	}
}

func TestSendMessage_ClampsCombinedEmbedLength(t *testing.T) {
	var receivedBody string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessage(t.Context(), Message{
		Embed: &Embed{
			Title:       strings.Repeat("t", 256),
			Description: strings.Repeat("d", 4096),
			FooterText:  strings.Repeat("f", 2048),
			AuthorName:  strings.Repeat("a", 256),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var payload struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Footer      struct {
				Text string `json:"text"`
			} `json:"footer"`
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal([]byte(receivedBody), &payload); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(payload.Embeds))
	}
	embed := payload.Embeds[0]
	total := len([]rune(embed.Title)) + len([]rune(embed.Description)) + len([]rune(embed.Footer.Text)) + len([]rune(embed.Author.Name))
	if total > MaxEmbedTotalLength {
		t.Fatalf("expected combined embed length <= %d, got %d", MaxEmbedTotalLength, total)
	}
}

func TestSendMessage_SendsEmbedWithColorAndThumbnail(t *testing.T) {
	var receivedBody string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessage(t.Context(), Message{
		Embed: &Embed{
			Title:        "Breaking News",
			Description:  "Something happened",
			URL:          "https://example.com/article",
			Color:        0x00FF00,
			ThumbnailURL: "https://example.com/image.png",
			AuthorName:   "Example Feed",
			FooterText:   "example.com",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		`"title":"Breaking News"`,
		`"color":65280`,
		`"thumbnail":{"url":"https://example.com/image.png"}`,
		`"author":{"name":"Example Feed"}`,
		`"footer":{"text":"example.com"}`,
	} {
		if !strings.Contains(receivedBody, want) {
			t.Fatalf("expected request body to contain %q, got %q", want, receivedBody)
		}
	}
}

func TestSendMessage_SetsThreadNameForForumChannel(t *testing.T) {
	var receivedBody string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessage(t.Context(), Message{Content: "hello", ThreadName: "New Post Title"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedBody, `"thread_name":"New Post Title"`) {
		t.Fatalf("expected request body to contain thread_name, got %q", receivedBody)
	}
}

func TestSendMessage_SetsThreadIDQueryParamForExistingThread(t *testing.T) {
	var receivedQuery string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessage(t.Context(), Message{Content: "hello", ThreadID: "123456789"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedQuery != "thread_id=123456789" {
		t.Fatalf("expected thread_id query param, got %q", receivedQuery)
	}
}

func TestSendMessage_SurfacesDiscordErrorDetail(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Invalid Webhook Token","code":50027}`))
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
	for _, want := range []string{"400", "Invalid Webhook Token", "50027"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
}

func TestSendMessage_ErrorWithoutBodyStillReturnsStatus(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	c := NewClientWithHTTP(webhookURL, httpClient)
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to contain status 500, got %q", err.Error())
	}
}

func TestSendMessage_RespectsTimeout(t *testing.T) {
	// The handler's sleep and the client's timeout both run on the bubble's
	// fake clock, so this is instant and deterministic. The in-memory server
	// has to be created inside the bubble to be part of it.
	synctest.Test(t, func(t *testing.T) {
		webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
		}))

		client := httpClient
		client.Timeout = 10 * time.Millisecond
		c := NewClientWithHTTP(webhookURL, client)
		err := c.SendMessageText(t.Context(), "hello")
		if err == nil {
			t.Fatal("expected timeout-related error, got nil")
		}
	})
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

	c := NewClientWithLimiter(webhookURL, httpClient, NewRateLimiter())
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

	c := NewClientWithLimiter(webhookURL, httpClient, NewRateLimiter())
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
			c := NewClientWithLimiter(testWebhookURL, nil, limiter)
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
	c := NewClientWithHTTP(testWebhookURL, nil)
	c.notePacing(c.webhookURL, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset-After": {"1"}})
}

// TestSendMessage_ContextCanceledWhilePaced asserts a send stuck waiting on a
// pacing delay returns the context's error promptly and never reaches the
// server.
func TestSendMessage_ContextCanceledWhilePaced(t *testing.T) {
	webhookURL, httpClient, arrivals := newBucketServer(t, "5", "1")

	limiter := NewRateLimiter()
	limiter.pace(webhookURL, maxPaceWait)
	c := NewClientWithLimiter(webhookURL, httpClient, limiter)

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
			c := NewClientWithLimiter(testWebhookURL, nil, limiter)
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
	c := NewClientWithLimiter(testWebhookURL, &http.Client{Transport: transport}, NewRateLimiter())

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
