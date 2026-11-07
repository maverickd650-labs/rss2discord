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
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// msgView is the comparable part of a discord.Message the message tests assert
// on. Slices are comma-joined, so a nil and an empty slice both read "".
type msgView struct {
	Content, Username, AvatarURL         string
	ThreadID, ThreadName, Tags           string
	MentionRoles, MentionUsers           string
	EmbedDescription, EmbedFooter        string
	HasEmbed, Silent, SuppressLinkEmbeds bool
}

func viewOfMsg(m discord.Message) msgView {
	embed := viewOfOrZero(m)
	return msgView{
		Content: m.Content, Username: m.Username, AvatarURL: m.AvatarURL,
		ThreadID: m.ThreadID, ThreadName: m.ThreadName, Tags: strings.Join(m.AppliedTags, ","),
		MentionRoles: strings.Join(m.MentionRoleIDs, ","), MentionUsers: strings.Join(m.MentionUserIDs, ","),
		EmbedDescription: embed.Description, EmbedFooter: embed.FooterText,
		HasEmbed: m.Embed != nil, Silent: m.Silent, SuppressLinkEmbeds: m.SuppressEmbeds,
	}
}

// messageCase renders entry through feed (inside group, default empty) and
// expects exactly want.
type messageCase struct {
	name  string
	group *v1beta1.FeedGroup
	feed  *v1beta1.FeedSpec
	entry rss.Entry
	want  msgView
}

func runMessageCases(t *testing.T, cases []messageCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := tc.group
			if group == nil {
				group = &v1beta1.FeedGroup{}
			}
			if got := viewOfMsg(buildOK(t, group, tc.feed, tc.entry)); got != tc.want {
				t.Fatalf("msg = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// requireBuildErrors asserts every feed fails to render entry.
func requireBuildErrors(t *testing.T, entry rss.Entry, feeds map[string]*v1beta1.FeedSpec) {
	t.Helper()
	for name, feed := range feeds {
		t.Run(name, func(t *testing.T) {
			requireBuildError(t, &v1beta1.FeedGroup{}, feed, entry)
		})
	}
}

func TestFeedRendererBuild_DefaultMessage(t *testing.T) {
	runMessageCases(t, []messageCase{{
		name:  "default",
		feed:  &v1beta1.FeedSpec{},
		entry: rss.Entry{Title: "T", Description: "D", Link: testArticleURL},
		want:  msgView{Content: "**T**\nD\n[Read more](https://example.com/a)"},
	}})
}

// TestFeedRendererBuild_ContentAndEmbed asserts content and embed are sent
// together, and that blank content is only an error without an embed.
func TestFeedRendererBuild_ContentAndEmbed(t *testing.T) {
	entry := rss.Entry{Title: "T"}
	runMessageCases(t, []messageCase{
		{
			name:  "content beside embed",
			feed:  msgFeed(v1beta1.MessageSpec{Content: "New: {{.Title}}", Embed: &v1beta1.EmbedSpec{}}),
			entry: entry,
			want:  msgView{Content: "New: T", HasEmbed: true},
		},
		{
			name:  "blank content beside an embed is not an error",
			feed:  msgFeed(v1beta1.MessageSpec{Content: tmplPublished, Embed: &v1beta1.EmbedSpec{}}),
			entry: entry,
			want:  msgView{HasEmbed: true},
		},
	})
	t.Run("content execution error", func(t *testing.T) {
		requireBuildError(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Content: "x{{.Unknown}}"}), entry)
	})
}

// TestFeedRendererBuild_EmptyRenderIsError asserts a template that renders
// to blank content or a blank forum post name (here: only {{.Published}},
// for an entry with no date) is reported as a render error, so the
// render-error path can skip the entry, rather than producing a message
// Discord would reject on every retry.
func TestFeedRendererBuild_EmptyRenderIsError(t *testing.T) {
	requireBuildErrors(t, rss.Entry{Title: "T"}, map[string]*v1beta1.FeedSpec{
		"blank content": msgFeed(v1beta1.MessageSpec{Content: " {{.Published}} "}),
		// Blank content is an error whether or not it would have pinged.
		"blank content with mentions":     msgFeed(v1beta1.MessageSpec{Content: tmplPublished, Mentions: &v1beta1.Mentions{RoleIDs: []string{"1"}}}),
		"blank forum post name":           {ForumPost: &v1beta1.ForumPostSpec{Name: tmplPublished}},
		"forum post name execution error": {ForumPost: &v1beta1.ForumPostSpec{Name: tmplMissingField}},
	})

	runMessageCases(t, []messageCase{{
		name:  "dated entry still renders",
		feed:  msgFeed(v1beta1.MessageSpec{Content: " {{.Published}} "}),
		entry: rss.Entry{Title: "T", Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)},
		want:  msgView{Content: publishedRFC3339},
	}})
}

func TestFeedRendererBuild_ThreadTargets(t *testing.T) {
	entry := rss.Entry{Title: "Hello"}
	runMessageCases(t, []messageCase{
		{
			name:  "existing thread",
			feed:  &v1beta1.FeedSpec{Thread: &v1beta1.ThreadTarget{ID: "123456"}},
			entry: entry,
			want:  msgView{Content: "**Hello**\n\n[Read more]()", ThreadID: "123456"},
		},
		{
			name:  "tagged forum post",
			feed:  &v1beta1.FeedSpec{ForumPost: &v1beta1.ForumPostSpec{Name: tmplTitle, AppliedTagIDs: []string{"7", "8"}}},
			entry: entry,
			want:  msgView{Content: "**Hello**\n\n[Read more]()", ThreadName: "Hello", Tags: "7,8"},
		},
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

	runMessageCases(t, []messageCase{
		{
			name:  "feed message overrides group message",
			group: group,
			feed:  embedFeed(v1beta1.EmbedSpec{Description: "feed:{{.Title}}"}),
			entry: entry,
			want:  msgView{HasEmbed: true, EmbedDescription: "feed:T"},
		},
		{
			name:  "feed text message replaces group embed",
			group: group,
			feed:  msgFeed(v1beta1.MessageSpec{Content: "plain {{.Title}}"}),
			entry: entry,
			want:  msgView{Content: "plain T"},
		},
		{
			name:  "group message inherited when feed has none",
			group: group,
			feed:  &v1beta1.FeedSpec{},
			entry: entry,
			want:  msgView{HasEmbed: true, EmbedDescription: "group:T", EmbedFooter: "group-footer", Silent: true},
		},
	})
}

func TestFeedRendererBuild_MentionsAndFlags(t *testing.T) {
	entry := rss.Entry{Title: "T"}
	runMessageCases(t, []messageCase{
		{
			name: "pings, content and flags",
			feed: msgFeed(v1beta1.MessageSpec{
				Content:              tmplTitle,
				Mentions:             &v1beta1.Mentions{RoleIDs: []string{"11", "12"}, UserIDs: []string{"21"}},
				Silent:               true,
				SuppressLinkPreviews: true,
			}),
			entry: entry,
			want:  msgView{Content: "<@&11> <@&12> <@21>\nT", MentionRoles: "11,12", MentionUsers: "21", Silent: true, SuppressLinkEmbeds: true},
		},
		{
			name:  "pings alone beside an embed",
			feed:  msgFeed(v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{}, Mentions: &v1beta1.Mentions{UserIDs: []string{"5"}}}),
			entry: entry,
			want:  msgView{Content: "<@5>", MentionUsers: "5", HasEmbed: true},
		},
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

	messages := make([]messageCase, 0, len(cases))
	for _, tc := range cases {
		messages = append(messages, messageCase{
			name:  tc.name,
			group: &v1beta1.FeedGroup{Spec: v1beta1.FeedGroupSpec{Username: tc.groupUsername, AvatarURL: tc.groupAvatar}},
			feed:  &v1beta1.FeedSpec{Username: tc.feedUsername, AvatarURL: tc.feedAvatar},
			entry: rss.Entry{Title: "Headline"},
			want:  msgView{Content: "**Headline**\n\n[Read more]()", Username: tc.wantUsername, AvatarURL: tc.wantAvatar},
		})
	}
	runMessageCases(t, messages)
}
