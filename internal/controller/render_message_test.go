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
*/package controller

import (
	"slices"
	"testing"
	"time"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

func TestFeedRendererBuild_DefaultMessage(t *testing.T) {
	msg := buildOK(t, &v1beta1.FeedGroup{}, &v1beta1.FeedSpec{}, rss.Entry{Title: "T", Description: "D", Link: testArticleURL})
	if want := "**T**\nD\n[Read more](https://example.com/a)"; msg.Content != want || msg.Embed != nil {
		t.Fatalf("msg = %+v, want content %q and no embed", msg, want)
	}
}

// TestFeedRendererBuild_ContentAndEmbed asserts content and embed are sent
// together, and that blank content is only an error without an embed.
func TestFeedRendererBuild_ContentAndEmbed(t *testing.T) {
	entry := rss.Entry{Title: "T"}
	group := &v1beta1.FeedGroup{}

	t.Run("content beside embed", func(t *testing.T) {
		msg := buildOK(t, group, msgFeed(v1beta1.MessageSpec{Content: "New: {{.Title}}", Embed: &v1beta1.EmbedSpec{}}), entry)
		if msg.Content != "New: T" || msg.Embed == nil || msg.Embed.Title != "T" {
			t.Fatalf("msg = %+v, want both content and embed", msg)
		}
	})

	t.Run("blank content beside an embed is not an error", func(t *testing.T) {
		msg := buildOK(t, group, msgFeed(v1beta1.MessageSpec{Content: tmplPublished, Embed: &v1beta1.EmbedSpec{}}), entry)
		if msg.Embed == nil {
			t.Fatal("expected the embed")
		}
	})

	t.Run("content execution error", func(t *testing.T) {
		requireBuildError(t, group, msgFeed(v1beta1.MessageSpec{Content: "x{{.Unknown}}"}), entry)
	})
}

// TestFeedRendererBuild_EmptyRenderIsError asserts a template that renders
// to blank content or a blank forum post name (here: only {{.Published}},
// for an entry with no date) is reported as a render error, so the
// render-error path can skip the entry, rather than producing a message
// Discord would reject on every retry.
func TestFeedRendererBuild_EmptyRenderIsError(t *testing.T) {
	group := &v1beta1.FeedGroup{}
	entry := rss.Entry{Title: "T"}

	failing := map[string]*v1beta1.FeedSpec{
		"blank content": msgFeed(v1beta1.MessageSpec{Content: " {{.Published}} "}),
		// Blank content is an error whether or not it would have pinged.
		"blank content with mentions":     msgFeed(v1beta1.MessageSpec{Content: tmplPublished, Mentions: &v1beta1.Mentions{RoleIDs: []string{"1"}}}),
		"blank forum post name":           {ForumPost: &v1beta1.ForumPostSpec{Name: tmplPublished}},
		"forum post name execution error": {ForumPost: &v1beta1.ForumPostSpec{Name: tmplMissingField}},
	}
	for name, feed := range failing {
		t.Run(name, func(t *testing.T) {
			requireBuildError(t, group, feed, entry)
		})
	}

	t.Run("dated entry still renders", func(t *testing.T) {
		dated := rss.Entry{Title: "T", Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		msg := buildOK(t, group, msgFeed(v1beta1.MessageSpec{Content: " {{.Published}} "}), dated)
		if msg.Content != publishedRFC3339 {
			t.Fatalf("msg.Content = %q, want %q", msg.Content, publishedRFC3339)
		}
	})
}

func TestFeedRendererBuild_ThreadTargets(t *testing.T) {
	entry := rss.Entry{Title: "Hello"}

	t.Run("existing thread", func(t *testing.T) {
		msg := buildOK(t, &v1beta1.FeedGroup{}, &v1beta1.FeedSpec{Thread: &v1beta1.ThreadTarget{ID: "123456"}}, entry)
		if msg.ThreadID != "123456" || msg.ThreadName != "" || msg.AppliedTags != nil {
			t.Fatalf("msg = %+v, want only ThreadID", msg)
		}
	})

	t.Run("tagged forum post", func(t *testing.T) {
		feed := &v1beta1.FeedSpec{ForumPost: &v1beta1.ForumPostSpec{Name: tmplTitle, AppliedTagIDs: []string{"7", "8"}}}
		msg := buildOK(t, &v1beta1.FeedGroup{}, feed, entry)
		if msg.ThreadName != "Hello" || msg.ThreadID != "" || !slices.Equal(msg.AppliedTags, []string{"7", "8"}) {
			t.Fatalf("msg = %+v, want a tagged forum post", msg)
		}
	})
}

// TestFeedRendererBuild_MessageResolution asserts a feed's message replaces
// the group's wholesale, and that a feed with none inherits the group's.
func TestFeedRendererBuild_MessageResolution(t *testing.T) {
	group := &v1beta1.FeedGroup{Spec: v1beta1.FeedGroupSpec{Message: &v1beta1.MessageSpec{
		Silent: true,
		Embed:  &v1beta1.EmbedSpec{Description: "group:{{.Title}}", Footer: &v1beta1.EmbedFooter{Text: "group-footer"}},
	}}}
	entry := rss.Entry{Title: "T"}

	t.Run("feed message overrides group message", func(t *testing.T) {
		msg := buildOK(t, group, embedFeed(v1beta1.EmbedSpec{Description: "feed:{{.Title}}"}), entry)
		if got := viewOfOrZero(msg); got.Description != "feed:T" || got.FooterText != "" || msg.Silent {
			t.Fatalf("msg = %+v, want the feed's message only", msg)
		}
	})

	t.Run("feed text message replaces group embed", func(t *testing.T) {
		msg := buildOK(t, group, msgFeed(v1beta1.MessageSpec{Content: "plain {{.Title}}"}), entry)
		if msg.Embed != nil || msg.Content != "plain T" {
			t.Fatalf("msg = %+v, want plain content without embed", msg)
		}
	})

	t.Run("group message inherited when feed has none", func(t *testing.T) {
		msg := buildOK(t, group, &v1beta1.FeedSpec{}, entry)
		if got := viewOfOrZero(msg); got.Description != "group:T" || got.FooterText != "group-footer" || !msg.Silent {
			t.Fatalf("msg = %+v, want the group's message", msg)
		}
	})
}

func TestFeedRendererBuild_MentionsAndFlags(t *testing.T) {
	mentions := &v1beta1.Mentions{RoleIDs: []string{"11", "12"}, UserIDs: []string{"21"}}
	entry := rss.Entry{Title: "T"}

	t.Run("pings, content and flags", func(t *testing.T) {
		msg := buildOK(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Content: tmplTitle, Mentions: mentions, Silent: true, SuppressLinkPreviews: true}), entry)
		if want := "<@&11> <@&12> <@21>\nT"; msg.Content != want {
			t.Fatalf("Content = %q, want %q", msg.Content, want)
		}
		if !slices.Equal(msg.MentionRoleIDs, mentions.RoleIDs) || !slices.Equal(msg.MentionUserIDs, mentions.UserIDs) || !msg.Silent || !msg.SuppressEmbeds {
			t.Fatalf("msg = %+v", msg)
		}
	})

	t.Run("pings alone beside an embed", func(t *testing.T) {
		msg := buildOK(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{}, Mentions: &v1beta1.Mentions{UserIDs: []string{"5"}}}), entry)
		if msg.Content != "<@5>" || msg.MentionRoleIDs != nil {
			t.Fatalf("msg = %+v, want the pings alone beside the embed", msg)
		}
	})
}

// TestFeedRendererBuild_SenderCustomizationOverrides tests the hierarchy:
// FeedSpec override -> FeedGroupSpec fallback -> Discord default.
func TestFeedRendererBuild_SenderCustomizationOverrides(t *testing.T) {
	const (
		botGroup    = "GroupBot"
		botFeed     = "FeedBot"
		avatarGroup = "https://cdn.example.com/group.png"
		avatarFeed  = "https://cdn.example.com/feed.png"
	)
	entry := rss.Entry{Title: "Headline"}

	cases := []struct {
		name                       string
		groupUsername, groupAvatar string
		feedUsername, feedAvatar   string
		wantUsername, wantAvatar   string
	}{
		{name: "feed username only inherits group avatar", groupUsername: botGroup, groupAvatar: avatarGroup, feedUsername: botFeed, wantUsername: botFeed, wantAvatar: avatarGroup},
		{name: "feed avatar only inherits group username", groupUsername: botGroup, groupAvatar: avatarGroup, feedAvatar: avatarFeed, wantUsername: botGroup, wantAvatar: avatarFeed},
		{name: "both feed overrides active", groupUsername: botGroup, groupAvatar: avatarGroup, feedUsername: botFeed, feedAvatar: avatarFeed, wantUsername: botFeed, wantAvatar: avatarFeed},
		{name: "neither feed override inherits group config", groupUsername: botGroup, groupAvatar: avatarGroup, wantUsername: botGroup, wantAvatar: avatarGroup},
		{name: "neither group nor feed override leaves empty (webhook default)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := &v1beta1.FeedGroup{Spec: v1beta1.FeedGroupSpec{Username: tc.groupUsername, AvatarURL: tc.groupAvatar}}
			feed := &v1beta1.FeedSpec{Username: tc.feedUsername, AvatarURL: tc.feedAvatar}
			msg := buildOK(t, group, feed, entry)
			if msg.Username != tc.wantUsername || msg.AvatarURL != tc.wantAvatar {
				t.Errorf("Username, AvatarURL = %q, %q, want %q, %q", msg.Username, msg.AvatarURL, tc.wantUsername, tc.wantAvatar)
			}
		})
	}
}
