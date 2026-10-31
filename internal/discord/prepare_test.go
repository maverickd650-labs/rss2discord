package discord

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

const prepareToken = "SUPER-SECRET-TOKEN"

// errNotAllowed is the text of the allowlist rejection.
const errNotAllowed = "must be an https://discord.com"

// prepareURL is a webhook URL that passes the production allowlist. The token
// is distinctive so a leak is easy to spot in any error.
const prepareURL = "https://discord.com/api/webhooks/123456789/" + prepareToken

// TestPrepareRequest_Accepts covers the success path of prepareRequest alone:
// no server, no limiter.
func TestPrepareRequest_Accepts(t *testing.T) {
	tests := []struct {
		name       string
		webhookURL string
		msg        Message
		wantTarget string
	}{
		{"discord.com", prepareURL, Message{Content: "hi"}, prepareURL},
		{"discordapp.com", "https://discordapp.com/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, "https://discordapp.com/api/webhooks/1/" + prepareToken},
		{"host is case-insensitive", "https://DISCORD.com/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, "https://DISCORD.com/api/webhooks/1/" + prepareToken},
		{"embed only", prepareURL, Message{Embed: &Embed{Title: "t"}}, prepareURL},
		{"thread id becomes a query", prepareURL, Message{Content: "hi", ThreadID: "987"}, prepareURL + "?thread_id=987"},
		{"blank thread id adds no query", prepareURL, Message{Content: "hi", ThreadID: "  "}, prepareURL},
		{"thread id replaces an existing one", prepareURL + "?thread_id=1&wait=true", Message{Content: "hi", ThreadID: "2"}, prepareURL + "?thread_id=2&wait=true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := prepareRequest(tt.webhookURL, tt.msg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.target != tt.wantTarget {
				t.Errorf("target = %q, want %q", p.target, tt.wantTarget)
			}
			if p.limiterKey != tt.webhookURL {
				t.Errorf("limiterKey = %q, want the configured webhook URL %q", p.limiterKey, tt.webhookURL)
			}
			var payload discordPayload
			if err := json.Unmarshal(p.body, &payload); err != nil {
				t.Fatalf("body is not a discord payload: %v", err)
			}
			if payload.AllowedMentions.Parse == nil || len(payload.AllowedMentions.Parse) != 0 {
				t.Errorf("allowed_mentions.parse = %v, want an empty list", payload.AllowedMentions.Parse)
			}
		})
	}
}

func TestPrepareRequest_ThreadIDIsQueryEncoded(t *testing.T) {
	p, err := prepareRequest(prepareURL, Message{Content: "hi", ThreadID: "a&b=c"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(p.target)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("thread_id"); got != "a&b=c" {
		t.Fatalf("thread_id = %q, want it round-tripped through the query", got)
	}
}

// TestPrepareRequest_Rejects covers every rejection, and that none of the
// errors contains the webhook URL or its token.
func TestPrepareRequest_Rejects(t *testing.T) {
	tests := []struct {
		name       string
		webhookURL string
		msg        Message
		wantErr    string
	}{
		{"empty webhook URL", "", Message{Content: "hi"}, "webhook URL is empty"},
		{"blank webhook URL", "   ", Message{Content: "hi"}, "webhook URL is empty"},
		{"empty message", prepareURL, Message{Content: "  "}, "content is empty"},
		{"unparseable URL", "https://discord.com/api/webhooks/1/" + prepareToken + "%zz", Message{Content: "hi"}, "invalid discord webhook URL"},
		{"http scheme", "http://discord.com/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, errNotAllowed},
		{"disallowed host", "https://evil.example/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, errNotAllowed},
		{"loopback host", "https://127.0.0.1/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, errNotAllowed},
		{"allowed name as a subdomain suffix", "https://discord.com.evil.example/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, errNotAllowed},
		{"allowed name in userinfo", "https://discord.com@evil.example/api/webhooks/1/" + prepareToken, Message{Content: "hi"}, errNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := prepareRequest(tt.webhookURL, tt.msg)
			if err == nil {
				t.Fatalf("expected an error, got a request to %q", p.target)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			assertNoWebhookSecret(t, err)
			if p.target != "" || p.body != nil {
				t.Errorf("a rejected request must be the zero preparedRequest, got %+v", p)
			}
		})
	}
}

func TestPrepareRequest_AppliesFit(t *testing.T) {
	long := strings.Repeat("x", MaxContentLength+500)
	p, err := prepareRequest(prepareURL, Message{
		Content:    long,
		Username:   strings.Repeat("u", MaxUsernameLength+10),
		ThreadName: strings.Repeat("t", MaxThreadNameLength+10),
		Embed: &Embed{
			Title:       strings.Repeat("a", MaxEmbedTitleLength+10),
			Description: strings.Repeat("d", MaxEmbedDescriptionLength+10),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload discordPayload
	if err := json.Unmarshal(p.body, &payload); err != nil {
		t.Fatal(err)
	}
	check := func(field, got string, limit int) {
		t.Helper()
		if n := utf8.RuneCountInString(got); n == 0 || n > limit {
			t.Errorf("%s has %d runes, want 1..%d", field, n, limit)
		}
	}
	check("content", payload.Content, MaxContentLength)
	check("username", payload.Username, MaxUsernameLength)
	check("thread_name", payload.ThreadName, MaxThreadNameLength)
	if len(payload.Embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(payload.Embeds))
	}
	check("embed title", payload.Embeds[0].Title, MaxEmbedTitleLength)
	check("embed description", payload.Embeds[0].Description, MaxEmbedDescriptionLength)
}

// A message that is only whitespace before Fit stays rejected, and one that
// Fit leaves non-empty is accepted: the emptiness check runs on the fitted
// message.
func TestPrepareRequest_EmptyCheckRunsAfterFit(t *testing.T) {
	if _, err := prepareRequest(prepareURL, Message{Content: "\n\t "}); err == nil {
		t.Fatal("expected a whitespace-only message to be rejected")
	}
}

// assertNoWebhookSecret fails if err's text contains any part of the webhook
// URL that identifies the webhook: its token or its ID path.
func assertNoWebhookSecret(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{prepareToken, "/api/webhooks/", "123456789"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q of the webhook URL: %v", secret, err)
		}
	}
}

// TestSend_NeverLeaksWebhookURL asserts the redaction rule for every error
// path of SendMessage, i.e. of prepareRequest and exchange together: invalid
// URL, disallowed host, non-HTTPS, transport error, 429 and non-2xx.
func TestSend_NeverLeaksWebhookURL(t *testing.T) {
	secretPath := "/api/webhooks/123456789/" + prepareToken

	// Handlers that respond with the webhook URL in the body must not make it
	// into the error either.
	statusServer := func(status int) *Client {
		webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"unknown webhook","code":10015}`))
		}))
		return NewClientWithHTTP(webhookURL, httpClient)
	}
	// newWebhookServer's URL has the fixed "mock-token"; check it as well.
	assertNoMock := func(t *testing.T, err error) {
		t.Helper()
		if strings.Contains(err.Error(), "mock-token") || strings.Contains(err.Error(), testWebhookPath) {
			t.Errorf("error leaks the webhook URL: %v", err)
		}
	}

	abortClient := func() *Client {
		srv := newAbortingServer(t)
		return NewClientWithHTTP("https://"+testWebhookHost+secretPath, srv.Client())
	}

	tests := []struct {
		name   string
		client *Client
	}{
		{"invalid URL", NewClient("https://discord.com" + secretPath + "%zz")},
		{"disallowed host", NewClient("https://evil.example" + secretPath)},
		{"non-HTTPS", NewClient("http://discord.com" + secretPath)},
		{"transport error", abortClient()},
		{"non-2xx", statusServer(http.StatusNotFound)},
		{"server error", statusServer(http.StatusInternalServerError)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.client.SendMessageText(t.Context(), "hello")
			if err == nil {
				t.Fatal("expected an error")
			}
			assertNoWebhookSecret(t, err)
			assertNoMock(t, err)
			// A typed error must be one of ours, not a wrapped *url.Error.
			if _, ok := errors.AsType[*url.Error](err); ok {
				t.Errorf("error still wraps a *url.Error (its text carries the URL): %v", err)
			}
		})
	}

	t.Run("429", func(t *testing.T) {
		webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		err := NewClientWithLimiter(webhookURL, httpClient, NewRateLimiter()).SendMessageText(t.Context(), "hello")
		if _, ok := errors.AsType[*RateLimitError](err); !ok {
			t.Fatalf("expected *RateLimitError, got %v", err)
		}
		assertNoMock(t, err)
	})
	t.Run("cooling limiter", func(t *testing.T) {
		webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		limiter := NewRateLimiter()
		limiter.cooldown(webhookURL, 30e9)
		err := NewClientWithLimiter(webhookURL, httpClient, limiter).SendMessageText(t.Context(), "hello")
		if _, ok := errors.AsType[*RateLimitError](err); !ok {
			t.Fatalf("expected *RateLimitError, got %v", err)
		}
		assertNoMock(t, err)
	})
}

// TestSendMessage_RejectedRequestNeverReachesLimiterOrNetwork pins the order:
// prepareRequest runs first, so a request for a disallowed host (or one that is
// otherwise rejected) neither waits on nor reserves the limiter and sends
// nothing. The webhook is put into cooldown first; if the limiter were
// consulted, the send would fail with *RateLimitError instead of the
// allowlist error.
func TestSendMessage_RejectedRequestNeverReachesLimiterOrNetwork(t *testing.T) {
	for _, webhookURL := range []string{
		"https://evil.example/api/webhooks/1/tok",
		"http://discord.com/api/webhooks/1/tok",
	} {
		t.Run(webhookURL, func(t *testing.T) {
			limiter := NewRateLimiter()
			limiter.cooldown(webhookURL, 30e9)
			var calls int
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("network must not be reached")
			})}

			err := NewClientWithLimiter(webhookURL, client, limiter).SendMessageText(t.Context(), "hello")
			if err == nil || !strings.Contains(err.Error(), errNotAllowed) {
				t.Fatalf("err = %v, want the allowlist rejection", err)
			}
			if _, ok := errors.AsType[*RateLimitError](err); ok {
				t.Fatal("the limiter was consulted for a rejected request")
			}
			if calls != 0 {
				t.Fatalf("transport called %d times, want 0", calls)
			}
		})
	}
}
