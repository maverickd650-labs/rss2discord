package discord

import (
	"net/http"
	"strings"
	"testing"
)

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

func TestSendMessage_ReturnsErrorOnNon2xx(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad"}`))
	}))

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
	err := c.SendMessageText(t.Context(), "non-2xx test")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
}
