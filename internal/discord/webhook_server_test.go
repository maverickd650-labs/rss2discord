package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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

// SendMessageText sends plain, embed-less content; only tests use it.
func (c *Client) SendMessageText(ctx context.Context, content string) error {
	return c.SendMessage(ctx, Message{Content: content})
}
