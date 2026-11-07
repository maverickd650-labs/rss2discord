/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"
	"testing"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// TestProcessFeed_FeedTitleReachesTemplate asserts the title the fetch
// returned for the feed is what {{.FeedTitle}} renders for its entries.
func TestProcessFeed_FeedTitleReachesTemplate(t *testing.T) {
	ctx := t.Context()
	ns, name := "processfeed-feed-title", "fg-feed-title"

	discordServer := NewMockDiscordServer(t)
	defer discordServer.Close()

	fg, feed := newMetricsFeedGroup(ns, name, "[{{.FeedTitle}}] {{.Title}}")
	client := discordServer.DiscordClientBuilder()(discordServer.URL())

	fetchResult := oneEntryFetch()
	fetchResult.FeedTitle = "Example Blog"
	processFeed(ctx, testRun(t, fg, feed, fetchResult, nil, client))

	bodies := discordServer.Bodies()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "[Example Blog] ") {
		t.Fatalf("expected one message carrying the feed title, got %q", bodies)
	}
}

func TestProcessFeed_Filter(t *testing.T) {
	tests := []struct {
		name         string
		filter       *v1beta1.Filter
		entries      []rss.Entry
		wantContent  string
		wantLastSeen string
	}{
		{
			name:   "regex match",
			filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "Kubernetes|K8s"}},
			entries: []rss.Entry{
				{ID: "k8s-1", Title: "Kubernetes Release v1.30", Description: "New Kubernetes version released", Seq: 1},
				{ID: "python-conf", Title: "Python Conference Schedule", Description: "Python conference talks announced", Seq: 0},
			},
			wantContent:  "Kubernetes Release v1.30",
			wantLastSeen: "k8s-1",
		},
		{
			name:   "keyword match in title",
			filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Keywords: []string{"Python", "Django"}}},
			entries: []rss.Entry{
				{ID: "python-1", Title: "Python Tutorial", Description: "Learn Python basics", Seq: 1},
				{ID: "js-1", Title: "JavaScript Best Practices", Description: "Advanced JS patterns", Seq: 0},
			},
			wantContent:  "Python Tutorial",
			wantLastSeen: "python-1",
		},
		{
			name:   "keyword match in description",
			filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Keywords: []string{"Django"}}},
			entries: []rss.Entry{
				{ID: "web-frameworks", Title: "Web Frameworks", Description: "Deep dive into Django internals", Seq: 1},
				{ID: "unrelated", Title: "Rust Updates", Description: "Rust 2026 edition", Seq: 0},
			},
			wantContent:  "Web Frameworks",
			wantLastSeen: "web-frameworks",
		},
		{
			name:   "zero matches does not send or advance watermark",
			filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Keywords: []string{"Nomatch"}}},
			entries: []rss.Entry{
				{ID: "filtered-1", Title: "Article 1", Description: "Desc 1", Seq: 1},
				{ID: "filtered-2", Title: "Article 2", Description: "Desc 2", Seq: 0},
			},
			wantContent:  "",
			wantLastSeen: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg, feed := newMetricsFeedGroup("processfeed-filter", "fg-filter", "")
			feed.Filter = tc.filter
			fg.Spec.Feeds[0] = feed
			sender := &fakeSender{}

			processFeed(t.Context(), testRun(t, fg, feed, rss.FetchResult{Entries: tc.entries}, nil, sender))

			assertSentOnly(t, sender, tc.wantContent)
			fs := feedStatusFor(fg, feed.Name)
			if fs.LastSeenEntry != tc.wantLastSeen {
				t.Fatalf("LastSeenEntry = %q, want %q", fs.LastSeenEntry, tc.wantLastSeen)
			}
		})
	}
}

func TestProcessFeed_StripHTMLFromDescription(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-strip-html", "fg-strip-html", defaultEmbedDescription)
	sender := &fakeSender{}

	fetch := rss.FetchResult{Entries: []rss.Entry{{
		ID:          "html-article",
		Title:       "HTML Article",
		Description: "<p>First paragraph.</p><ul><li>One</li></ul>",
		Link:        "https://example.com/html-article",
	}}}

	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender))

	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	body := sender.sent[0].Content
	if strings.Contains(body, "<p>") || strings.Contains(body, "<li>") {
		t.Fatalf("sent content contains HTML tags: %q", body)
	}
	if !strings.Contains(body, "First paragraph.") || !strings.Contains(body, "One") {
		t.Fatalf("sent content missing text: %q", body)
	}
}

func TestProcessFeed_EmbedRendering(t *testing.T) {
	const embedTitle = "Embed Article"
	fg, feed := newMetricsFeedGroup("processfeed-embed", "fg-embed", "")
	fg.Spec.Message = &v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{Color: "#00FF00"}}
	feed.ForumPost = &v1beta1.ForumPostSpec{Name: tmplTitle}
	fg.Spec.Feeds[0] = feed
	sender := &fakeSender{}

	fetch := rss.FetchResult{Entries: []rss.Entry{{
		ID:          "embed-article",
		Title:       embedTitle,
		Description: "Embed body text",
		Link:        "https://example.com/embed-article",
		Image:       "https://example.com/pic.jpg",
	}}}

	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender))

	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.ThreadName != embedTitle {
		t.Fatalf("ThreadName = %q, want %q", msg.ThreadName, embedTitle)
	}
	if msg.Embed == nil {
		t.Fatal("msg.Embed is nil")
	}
	embed := msg.Embed
	if embed.Title != embedTitle {
		t.Fatalf("embed.Title = %q, want %q", embed.Title, embedTitle)
	}
	const wantColor = 0x00FF00 // EmbedSpec.Color "#00FF00"
	if embed.Color != wantColor {
		t.Fatalf("embed.Color = %#06x, want %#06x", embed.Color, wantColor)
	}
	if embed.ThumbnailURL != "https://example.com/pic.jpg" {
		t.Fatalf("embed.ThumbnailURL = %q, want https://example.com/pic.jpg", embed.ThumbnailURL)
	}
}

func TestProcessFeed_EmbedDropsUnsafeURLs(t *testing.T) {
	fg, feed := newMetricsFeedGroup("processfeed-embed-unsafe", "fg-embed-unsafe", "")
	fg.Spec.Message = &v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{}}
	fg.Spec.Feeds[0] = feed
	sender := &fakeSender{}

	fetch := rss.FetchResult{Entries: []rss.Entry{{
		ID:          "unsafe-url-article",
		Title:       "Unsafe URL Article",
		Description: "Body",
		Link:        jsAlertURL,
		Image:       "data:image/png;base64,AAAA",
	}}}

	processFeed(t.Context(), testRun(t, fg, feed, fetch, nil, sender))

	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	embed := sender.sent[0].Embed
	if embed == nil {
		t.Fatal("sender.sent[0].Embed is nil")
	}
	if embed.URL != "" {
		t.Fatalf("embed.URL = %q, want empty for unsafe URL", embed.URL)
	}
	if embed.ThumbnailURL != "" {
		t.Fatalf("embed.ThumbnailURL = %q, want empty for unsafe enclosure", embed.ThumbnailURL)
	}
}

// assertSentOnly fails unless sender received exactly one message containing
// want, or, when want is empty, no message at all.
func assertSentOnly(t *testing.T, sender *fakeSender, want string) {
	t.Helper()
	if want == "" {
		if len(sender.sent) != 0 {
			t.Fatalf("sender called %d times, want 0", len(sender.sent))
		}
		return
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sender called %d times, want 1", len(sender.sent))
	}
	if !strings.Contains(sender.sent[0].Content, want) {
		t.Fatalf("sent content = %q, want %q", sender.sent[0].Content, want)
	}
}
