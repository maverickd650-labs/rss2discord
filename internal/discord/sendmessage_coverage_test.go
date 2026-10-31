package discord

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// http.NewRequestWithContext refuses a nil context. SendMessage must return
// that error as is (it carries no URL, so no webhook token) without making a
// request.
func TestSendMessage_RejectsNilContext(t *testing.T) {
	var requests int
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))

	var ctx context.Context
	err := NewClient(webhookURL, WithHTTPClient(httpClient)).SendMessage(ctx, Message{Content: "nil context"})
	if err == nil {
		t.Fatal("expected an error for a nil context")
	}
	if _, ok := errors.AsType[*HTTPStatusError](err); ok {
		t.Fatalf("a request-construction failure must not look like an HTTP status: %v", err)
	}
	if strings.Contains(err.Error(), "mock-token") {
		t.Fatalf("error leaks the webhook token: %v", err)
	}
	if requests != 0 {
		t.Fatalf("server saw %d requests, want none", requests)
	}
	// Pin the error to the one http.NewRequestWithContext produces, so a
	// different failure or an added wrapper fails the test.
	_, want := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, nil)
	if want == nil || err.Error() != want.Error() || errors.Unwrap(err) != nil {
		t.Fatalf("err = %q (unwraps to %v), want the unwrapped request-construction error %q", err, errors.Unwrap(err), want)
	}
}
