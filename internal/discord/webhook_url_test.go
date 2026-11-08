package discord

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
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

// sentRequest is what a stubbed transport saw: enough to assert on the
// request target and payload after the call has returned.
type sentRequest struct {
	Method string
	URL    *url.URL
	Body   []byte
}

// stubClient answers every request with status and header, and records each
// request it was asked to make. It never touches the network.
func stubClient(t *testing.T, status int, header http.Header) (*http.Client, *[]sentRequest) {
	t.Helper()
	var sent []sentRequest
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		sent = append(sent, sentRequest{Method: r.Method, URL: r.URL, Body: body})
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}, &sent
}

// requestTarget is the part of a request that normalization must get right.
type requestTarget struct {
	Method, Scheme, Host, EscapedPath, Wait string
	ThreadIDs                               string // every thread_id value, comma-joined
}

func targetOf(r sentRequest) requestTarget {
	q := r.URL.Query()
	return requestTarget{
		Method:      r.Method,
		Scheme:      r.URL.Scheme,
		Host:        r.URL.Host,
		EscapedPath: r.URL.EscapedPath(),
		Wait:        q.Get("wait"),
		ThreadIDs:   strings.Join(q["thread_id"], ","),
	}
}

func TestSendMessage_NormalizedURLPreservesThreadTarget(t *testing.T) {
	httpClient, requests := stubClient(t, http.StatusNoContent, nil)
	c := NewClient(" \nhttps://PTB.Discord.com/api/v10/webhooks/1/a%2Fb?wait=true&thread_id=old\t ", WithHTTPClient(httpClient))
	if err := c.SendMessage(t.Context(), Message{Content: "thread-target", ThreadID: "12345"}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(*requests))
	}
	want := requestTarget{
		Method:      http.MethodPost,
		Scheme:      "https",
		Host:        canonicalWebhookHost,
		EscapedPath: "/api/v10/webhooks/1/a%2Fb",
		Wait:        "true",
		ThreadIDs:   "12345",
	}
	if got := targetOf((*requests)[0]); got != want {
		t.Fatalf("normalization changed request target: got %+v, want %+v", got, want)
	}
}

var webhookHostAliases = []string{canonicalWebhookHost, ptbWebhookHost, canaryWebhookHost}

// cooldownFixture is a webhook whose first send drew a 429 with a long
// Retry-After, plus the transport and limiter shared by every client built
// from it.
type cooldownFixture struct {
	httpClient *http.Client
	requests   *[]sentRequest
	limiter    *RateLimiter
	path       string
}

func newCooldownFixture(t *testing.T, host string) cooldownFixture {
	t.Helper()
	httpClient, requests := stubClient(t, http.StatusTooManyRequests, http.Header{"Retry-After": {"3600"}})
	f := cooldownFixture{httpClient: httpClient, requests: requests, limiter: NewRateLimiter(), path: "/api/webhooks/1/token"}
	first := NewClient("https://"+host+f.path, WithHTTPClient(httpClient), WithRateLimiter(f.limiter))
	if err := first.SendMessageText(t.Context(), "first"); err == nil {
		t.Fatal("expected first request to be rate limited")
	}
	return f
}

// assertHeld requires a client for alias to be refused by the shared limiter
// with a bounded RateLimitError, without reaching the transport.
func (f cooldownFixture) assertHeld(t *testing.T, alias string) {
	t.Helper()
	other := NewClient(" \nhttps://"+alias+f.path+"\t", WithHTTPClient(f.httpClient), WithRateLimiter(f.limiter))
	rle, ok := errors.AsType[*RateLimitError](other.SendMessageText(t.Context(), "held"))
	if !ok {
		t.Fatalf("%s did not share cooldown: not a RateLimitError", alias)
	}
	if rle.RetryAfter <= 0 || rle.RetryAfter > time.Hour {
		t.Fatalf("%s did not share cooldown: RetryAfter = %v", alias, rle.RetryAfter)
	}
}

func (f cooldownFixture) assertAllAliasesHeld(t *testing.T) {
	t.Helper()
	for _, alias := range webhookHostAliases {
		f.assertHeld(t, alias)
	}
	if got := len(*f.requests); got != 1 {
		t.Fatalf("cooldown allowed %d network requests, want 1", got)
	}
}

// assertOtherTokenIndependent requires a different token to keep its own
// rate-limit bucket, so its send reaches the transport.
func (f cooldownFixture) assertOtherTokenIndependent(t *testing.T) {
	t.Helper()
	independent := NewClient("https://discord.com/api/webhooks/1/other-token", WithHTTPClient(f.httpClient), WithRateLimiter(f.limiter))
	err := independent.SendMessageText(t.Context(), "independent")
	if err == nil || len(*f.requests) != 2 {
		t.Fatalf("different webhook did not reach transport: requests=%d err=%v", len(*f.requests), err)
	}
}

func requireCanonicalHosts(t *testing.T, requests []sentRequest) {
	t.Helper()
	for _, r := range requests {
		if r.URL.Host != canonicalWebhookHost {
			t.Errorf("request host = %q", r.URL.Host)
		}
	}
}

// Exercise a real 429 response through the client: equal normalized strings
// alone would not catch a constructor using the original URL as its limiter key.
func TestSendMessage_NormalizedHostsShareCooldown(t *testing.T) {
	for _, host := range webhookHostAliases {
		t.Run(host, func(t *testing.T) {
			f := newCooldownFixture(t, host)
			f.assertAllAliasesHeld(t)
			f.assertOtherTokenIndependent(t)
			requireCanonicalHosts(t, *f.requests)
		})
	}
}

// requireMedia checks that an embed's media field is present exactly when a URL
// was supplied, and that it carries that URL.
func requireMedia(t *testing.T, embed map[string]json.RawMessage, field, want string) {
	t.Helper()
	raw, present := embed[field]
	if present != (want != "") {
		t.Fatalf("%s presence=%t, want URL %q", field, present, want)
	}
	if !present {
		return
	}
	var got discordEmbedMedia
	if err := json.Unmarshal(raw, &got); err != nil || got.URL != want {
		t.Fatalf("%s = %s, err=%v; want %q", field, raw, err, want)
	}
}

// decodeSingleEmbed decodes a request body twice: typed, for the payload, and
// raw, so absent media fields can be told apart from empty ones.
func decodeSingleEmbed(t *testing.T, body []byte) (discordPayload, map[string]json.RawMessage) {
	t.Helper()
	var payload discordPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(payload.Embeds))
	}
	var raw struct {
		Embeds []json.RawMessage `json:"embeds"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Embeds) != 1 {
		t.Fatalf("got %d raw embeds, want 1", len(raw.Embeds))
	}
	var media map[string]json.RawMessage
	if err := json.Unmarshal(raw.Embeds[0], &media); err != nil {
		t.Fatal(err)
	}
	return payload, media
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
			httpClient, requests := stubClient(t, http.StatusNoContent, nil)
			c := NewClient("https://discord.com/api/webhooks/1/token", WithHTTPClient(httpClient))
			if err := c.SendMessage(t.Context(), Message{Embed: &Embed{Title: "Title", ThumbnailURL: tc.thumbnail, ImageURL: tc.image}}); err != nil {
				t.Fatal(err)
			}
			if len(*requests) != 1 {
				t.Fatalf("got %d requests, want 1", len(*requests))
			}
			payload, media := decodeSingleEmbed(t, (*requests)[0].Body)
			requireMedia(t, media, "thumbnail", tc.thumbnail)
			requireMedia(t, media, "image", tc.image)
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

// mediaURL is the URL of an embed media field, or "" when the field is absent.
func mediaURL(m *discordEmbedMedia) string {
	if m == nil {
		return ""
	}
	return m.URL
}

func TestEmbedToPayload_ImageStyles(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		embed                Embed
		wantThumb, wantImage string
	}{
		{name: "thumbnail", embed: Embed{Title: "t", ThumbnailURL: imageFixtureURL}, wantThumb: imageFixtureURL},
		{name: "image", embed: Embed{Title: "t", ImageURL: imageFixtureURL}, wantImage: imageFixtureURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := embedToPayload(tc.embed)
			if thumb, image := mediaURL(got.Thumbnail), mediaURL(got.Image); thumb != tc.wantThumb || image != tc.wantImage {
				t.Fatalf("thumbnail=%q image=%q, want thumbnail=%q image=%q", thumb, image, tc.wantThumb, tc.wantImage)
			}
		})
	}
}
