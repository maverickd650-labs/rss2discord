package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSendMessage_SuppressesMentionsByDefault(t *testing.T) {
	var receivedBody string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
	err := c.SendMessageText(t.Context(), "@everyone breaking news")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(receivedBody, `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("expected allowed_mentions to suppress all mentions, got %q", receivedBody)
	}
}

func TestSendMessage_ClampsCombinedEmbedLength(t *testing.T) {
	var receivedBody string
	webhookURL, httpClient := newWebhookServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	}))

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
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

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
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

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
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

	c := NewClient(webhookURL, WithHTTPClient(httpClient))
	err := c.SendMessage(t.Context(), Message{Content: "hello", ThreadID: "123456789"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedQuery != "thread_id=123456789" {
		t.Fatalf("expected thread_id query param, got %q", receivedQuery)
	}
}
