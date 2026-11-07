package discord

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeErrorDetail(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty body", body: "", want: ""},
		{name: "empty object", body: `{}`, want: ""},
		{name: "message only", body: `{"message":"invalid webhook token"}`, want: ": invalid webhook token"},
		{name: "code and message", body: `{"message":"Unknown Webhook","code":10015}`, want: ": Unknown Webhook (code 10015)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeErrorDetail(strings.NewReader(tc.body)); got != tc.want {
				t.Fatalf("decodeErrorDetail(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestEmbedToPayload_OmitsUnsetMedia(t *testing.T) {
	// An embed with only a title should produce no thumbnail/author/footer
	// sub-objects, so they stay omitted from the JSON.
	p := embedToPayload(Embed{Title: "only title"})
	if p.Thumbnail != nil {
		t.Fatalf("expected Thumbnail to be nil, got %+v", p.Thumbnail)
	}
	if p.Author != nil {
		t.Fatalf("expected Author to be nil, got %+v", p.Author)
	}
	if p.Footer != nil {
		t.Fatalf("expected Footer to be nil, got %+v", p.Footer)
	}
	if p.Title != "only title" {
		t.Fatalf("expected title preserved, got %q", p.Title)
	}
}

func TestEmbedToPayload_AuthorAndFooterDetails(t *testing.T) {
	e := Embed{
		AuthorName:    "Author",
		AuthorURL:     "https://example.com/author",
		AuthorIconURL: "https://example.com/author.png",
		FooterText:    "Footer",
		FooterIconURL: "https://example.com/footer.png",
	}
	p := embedToPayload(e)
	wantAuthor := &discordEmbedAuthor{Name: e.AuthorName, URL: e.AuthorURL, IconURL: e.AuthorIconURL}
	if !reflect.DeepEqual(p.Author, wantAuthor) {
		t.Fatalf("Author = %+v, want %+v", p.Author, wantAuthor)
	}
	wantFooter := &discordEmbedFooter{Text: e.FooterText, IconURL: e.FooterIconURL}
	if !reflect.DeepEqual(p.Footer, wantFooter) {
		t.Fatalf("Footer = %+v, want %+v", p.Footer, wantFooter)
	}
}

func TestEmbedToPayload_AuthorAndFooterRequireText(t *testing.T) {
	// Discord requires name for author and text for footer. URL/IconURL without
	// the required text must not create payload objects.
	pMissing := embedToPayload(Embed{
		AuthorURL:     "https://example.com/author",
		AuthorIconURL: "https://example.com/author.png",
		FooterIconURL: "https://example.com/footer.png",
	})
	if pMissing.Author != nil {
		t.Fatalf("expected Author to be nil when AuthorName is empty, got %+v", pMissing.Author)
	}
	if pMissing.Footer != nil {
		t.Fatalf("expected Footer to be nil when FooterText is empty, got %+v", pMissing.Footer)
	}
}

// preparedPayload runs msg through prepareRequest and decodes the body.
func preparedPayload(t *testing.T, msg Message) (map[string]any, string) {
	t.Helper()
	p, err := prepareRequest("https://discord.com/api/webhooks/1/token", msg)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(p.body, &body); err != nil {
		t.Fatal(err)
	}
	return body, p.target
}

func TestPrepareRequest_DefaultsSuppressMentionsAndSetNoFlags(t *testing.T) {
	body, _ := preparedPayload(t, Message{Content: "x", AppliedTags: []string{"9"}})
	if got, want := body["allowed_mentions"], map[string]any{"parse": []any{}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_mentions = %v, want %v", got, want)
	}
	if _, ok := body["flags"]; ok {
		t.Fatalf("flags = %v, want omitted", body["flags"])
	}
	if _, ok := body["applied_tags"]; ok {
		t.Fatal("applied_tags sent without a new forum post")
	}
}

func TestPrepareRequest_ConfiguredMentionsFlagsAndTags(t *testing.T) {
	body, target := preparedPayload(t, Message{
		Content:        "<@&1> <@2> x",
		MentionRoleIDs: []string{"1"},
		MentionUserIDs: []string{"2"},
		Silent:         true,
		SuppressEmbeds: true,
		ThreadName:     "post",
		AppliedTags:    []string{"7", "8"},
	})
	want := map[string]any{"parse": []any{}, "roles": []any{"1"}, "users": []any{"2"}}
	if got := body["allowed_mentions"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_mentions = %v, want %v", got, want)
	}
	if got := body["flags"]; got != float64(flagSuppressEmbeds|flagSuppressNotifications) {
		t.Fatalf("flags = %v, want %d", got, flagSuppressEmbeds|flagSuppressNotifications)
	}
	if got := body["applied_tags"]; !reflect.DeepEqual(got, []any{"7", "8"}) {
		t.Fatalf("applied_tags = %v", got)
	}
	if strings.Contains(target, "thread_id") {
		t.Fatalf("target = %q, want no thread_id for a new post", target)
	}
}

func TestPrepareRequest_SilentAlone(t *testing.T) {
	body, _ := preparedPayload(t, Message{Content: "x", Silent: true})
	if got := body["flags"]; got != float64(flagSuppressNotifications) {
		t.Fatalf("flags = %v, want %d", got, flagSuppressNotifications)
	}
}
