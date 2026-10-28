package discord

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

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
			c := NewClientWithHTTP("https://"+host+"/api/webhooks/1/token", recordingClient(&hosts))
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
			c := NewClientWithHTTP(raw, recordingClient(&hosts))
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

func TestEmbedToPayload_ImageStyles(t *testing.T) {
	thumb := embedToPayload(Embed{Title: "t", ThumbnailURL: "https://x/a.png"})
	if thumb.Thumbnail == nil || thumb.Image != nil {
		t.Fatalf("thumbnail style: %+v", thumb)
	}
	img := embedToPayload(Embed{Title: "t", ImageURL: "https://x/a.png"})
	if img.Image == nil || img.Image.URL != "https://x/a.png" || img.Thumbnail != nil {
		t.Fatalf("image style: %+v", img)
	}
}
