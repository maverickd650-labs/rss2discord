package discord

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	c := NewClient("https://"+testWebhookHost+testWebhookPath, WithHTTPClient(client))
	err := c.SendMessageText(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected the unfollowed redirect's 3xx status to surface as an error")
	}
	if attackerHit {
		t.Fatal("redirect target must never receive the request")
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

	// WithHTTPClient lets the test supply a client trusting the
	// httptest TLS cert. The httptest server resolves to 127.0.0.1, so we
	// temporarily add it to the allow-list to exercise the "happy path"
	// without depending on a real discord.com endpoint.
	c := NewClient(webhookURL, WithHTTPClient(httpClient))
	if err := c.SendMessageText(t.Context(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !requestReceived {
		t.Fatal("expected request to reach the test server")
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

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
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
