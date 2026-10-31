package discord

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	canonicalWebhookHost = "discord.com"
	ptbWebhookHost       = "ptb.discord.com"
	canaryWebhookHost    = "canary.discord.com"
)

func TestNormalizeWebhookURL_PreservesURLComponents(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{
			name: "escaped path query and fragment",
			raw:  "https://canary.discord.com/api/v10/webhooks/1/a%2Fb?wait=true&thread_id=42#fragment",
			want: "https://discord.com/api/v10/webhooks/1/a%2Fb?wait=true&thread_id=42#fragment",
		},
		{name: "explicit https port", raw: "https://ptb.discord.com:443/api/webhooks/1/token"},
		{name: "invalid port", raw: "https://ptb.discord.com:bad/api/webhooks/1/token"},
		{name: "malformed escape", raw: "https://ptb.discord.com/api/webhooks/1/%zz"},
		{name: "trailing dot", raw: "https://ptb.discord.com./api/webhooks/1/token"},
		{name: "lookalike", raw: "https://ptb.discord.com.evil.example/api/webhooks/1/token"},
		{name: "legacy discord domain", raw: "https://discordapp.com/api/webhooks/1/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.raw
			}
			got := normalizeWebhookURL(tc.raw)
			if got != want || normalizeWebhookURL(got) != got {
				t.Fatalf("normalizeWebhookURL(%q) = %q, want %q and idempotent", tc.raw, got, want)
			}
		})
	}
}

func TestSendMessage_NormalizedURLPreservesThreadTarget(t *testing.T) {
	var requests []*http.Request
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r)
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	c := NewClient(" \nhttps://PTB.Discord.com/api/v10/webhooks/1/a%2Fb?wait=true&thread_id=old\t ", WithHTTPClient(httpClient))
	if err := c.SendMessage(t.Context(), Message{Content: "thread-target", ThreadID: "12345"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests))
	}
	r := requests[0]
	if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != canonicalWebhookHost ||
		r.URL.EscapedPath() != "/api/v10/webhooks/1/a%2Fb" || r.URL.Query().Get("wait") != "true" ||
		r.URL.Query().Get("thread_id") != "12345" || len(r.URL.Query()["thread_id"]) != 1 {
		t.Fatalf("normalization changed request target: %s %s", r.Method, r.URL)
	}
}

// Exercise a real 429 response through the client: equal normalized strings
// alone would not catch a constructor using the original URL as its limiter key.
func TestSendMessage_NormalizedHostsShareCooldown(t *testing.T) {
	for _, host := range []string{canonicalWebhookHost, ptbWebhookHost, canaryWebhookHost} {
		t.Run(host, func(t *testing.T) {
			requests := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.URL.Host != canonicalWebhookHost {
					t.Errorf("request host = %q", r.URL.Host)
				}
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": {"3600"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			})}
			limiter := NewRateLimiter()
			const path = "/api/webhooks/1/token"
			first := NewClient("https://"+host+path, WithHTTPClient(httpClient), WithRateLimiter(limiter))
			if err := first.SendMessageText(t.Context(), "first"); err == nil {
				t.Fatal("expected first request to be rate limited")
			}
			for _, alias := range []string{canonicalWebhookHost, ptbWebhookHost, canaryWebhookHost} {
				other := NewClient(" \nhttps://"+alias+path+"\t", WithHTTPClient(httpClient), WithRateLimiter(limiter))
				err := other.SendMessageText(t.Context(), "held")
				rle, ok := errors.AsType[*RateLimitError](err)
				if !ok || rle.RetryAfter <= 0 || rle.RetryAfter > time.Hour {
					t.Fatalf("%s did not share cooldown: %v", alias, err)
				}
			}
			if requests != 1 {
				t.Fatalf("cooldown allowed %d network requests, want 1", requests)
			}
			// A different token must retain its own rate-limit bucket.
			independent := NewClient("https://discord.com/api/webhooks/1/other-token", WithHTTPClient(httpClient), WithRateLimiter(limiter))
			if err := independent.SendMessageText(t.Context(), "independent"); err == nil || requests != 2 {
				t.Fatalf("different webhook did not reach transport: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestSendMessage_EmbedMediaJSON(t *testing.T) {
	for _, tc := range []struct {
		name, thumbnail, image string
	}{
		{name: "neither"},
		{name: "thumbnail", thumbnail: imageFixtureURL},
		{name: "large image", image: imageFixtureURL},
		{name: "independent media", thumbnail: imageFixtureURL, image: "https://x/large.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload discordPayload
			var media map[string]json.RawMessage
			calls := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Embeds) != 1 {
					t.Fatalf("got %d embeds, want 1", len(payload.Embeds))
				}
				var rawPayload struct {
					Embeds []json.RawMessage `json:"embeds"`
				}
				if err := json.Unmarshal(body, &rawPayload); err != nil {
					t.Fatal(err)
				}
				if len(rawPayload.Embeds) != 1 {
					t.Fatalf("got %d raw embeds, want 1", len(rawPayload.Embeds))
				}
				if err := json.Unmarshal(rawPayload.Embeds[0], &media); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			c := NewClient("https://discord.com/api/webhooks/1/token", WithHTTPClient(httpClient))
			if err := c.SendMessage(t.Context(), Message{Embed: &Embed{Title: "Title", ThumbnailURL: tc.thumbnail, ImageURL: tc.image}}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("got %d requests, want 1", calls)
			}
			for field, want := range map[string]string{"thumbnail": tc.thumbnail, "image": tc.image} {
				raw, present := media[field]
				if present != (want != "") {
					t.Fatalf("%s presence=%t, want URL %q", field, present, want)
				}
				if present {
					var got discordEmbedMedia
					if err := json.Unmarshal(raw, &got); err != nil || got.URL != want {
						t.Fatalf("%s = %s, err=%v; want %q", field, raw, err, want)
					}
				}
			}
			if payload.AllowedMentions.Parse == nil || len(payload.AllowedMentions.Parse) != 0 {
				t.Fatalf("embed delivery must disable mentions: %+v", payload.AllowedMentions)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// recordingClient returns an http.Client that never touches the network and
// records the host of every request it is asked to make.
func recordingClient(hosts *[]string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*hosts = append(*hosts, r.URL.Host)
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})}
}

func TestSendMessage_PTBAndCanaryHostsAreSentToDiscordCom(t *testing.T) {
	for _, host := range []string{"ptb.discord.com", "canary.discord.com", "PTB.Discord.com"} {
		t.Run(host, func(t *testing.T) {
			var hosts []string
			c := NewClient("https://"+host+"/api/webhooks/1/token", WithHTTPClient(recordingClient(&hosts)))
			if err := c.SendMessageText(t.Context(), "hi"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(hosts) != 1 || hosts[0] != "discord.com" {
				t.Fatalf("request hosts = %v, want [discord.com]", hosts)
			}
		})
	}
}

// The rewrite must not widen what can be reached: lookalikes, plain HTTP and
// explicit ports are still refused and never produce a request.
func TestSendMessage_PTBLookalikesStillRejected(t *testing.T) {
	for _, raw := range []string{
		"https://evil.ptb.discord.com/api/webhooks/1/token",
		"https://ptb.discord.com.evil.com/api/webhooks/1/token",
		"http://ptb.discord.com/api/webhooks/1/token",
		"https://ptb.discord.com:8443/api/webhooks/1/token",
		"https://canary.discordapp.com/api/webhooks/1/token",
	} {
		t.Run(raw, func(t *testing.T) {
			var hosts []string
			c := NewClient(raw, WithHTTPClient(recordingClient(&hosts)))
			if err := c.SendMessageText(t.Context(), "hi"); err == nil {
				t.Fatal("expected the URL to be rejected")
			}
			if len(hosts) != 0 {
				t.Fatalf("a request was made to %v", hosts)
			}
		})
	}
}

// A PTB URL and the regular URL for the same webhook must share a rate-limit
// key, so a 429 seen through one cools down the other.
func TestNormalizeWebhookURL_SharesRateLimitKey(t *testing.T) {
	const regular = "https://discord.com/api/webhooks/1/token"
	for _, ptb := range []string{
		"https://ptb.discord.com/api/webhooks/1/token",
		"https://canary.discord.com/api/webhooks/1/token",
	} {
		if got := normalizeWebhookURL(ptb); got != regular {
			t.Errorf("normalizeWebhookURL(%q) = %q, want %q", ptb, got, regular)
		}
	}
	if got := normalizeWebhookURL(regular); got != regular {
		t.Errorf("regular URL changed: %q", got)
	}
}

const imageFixtureURL = "https://x/a.png"

func TestEmbedToPayload_ImageStyles(t *testing.T) {
	thumb := embedToPayload(Embed{Title: "t", ThumbnailURL: imageFixtureURL})
	if thumb.Thumbnail == nil || thumb.Image != nil {
		t.Fatalf("thumbnail style: %+v", thumb)
	}
	img := embedToPayload(Embed{Title: "t", ImageURL: imageFixtureURL})
	if img.Image == nil || img.Image.URL != imageFixtureURL || img.Thumbnail != nil {
		t.Fatalf("image style: %+v", img)
	}
}
