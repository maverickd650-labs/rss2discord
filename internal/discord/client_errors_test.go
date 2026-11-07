package discord

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSendMessage_SurfacesDiscordErrorDetail(t *testing.T) {
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Invalid Webhook Token","code":50027}`))
	}))

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
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

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
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
		c := NewClient(webhookURL, WithHTTPClient(client))
		err := c.SendMessageText(t.Context(), "hello")
		if err == nil {
			t.Fatal("expected timeout-related error, got nil")
		}
	})
}
