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
	"testing"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// This file holds the renderer's configuration tests and the helpers shared by
// the render_*_test.go files: render_template_test.go (renderTemplate over the
// sanitized entry), render_embed_test.go (embed fields) and
// render_message_test.go (content, threads, mentions, sender).

func TestParseHexColor(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{name: "hash prefix", input: "#00FF00", want: 0x00FF00},
		{name: "no prefix", input: "FF0000", want: 0xFF0000},
		{name: "lowercase", input: "#ffffff", want: 0xFFFFFF},
		{name: "blank", input: "  ", want: 0},
		{name: "malformed", input: "#nothex", want: 0},
		{name: "with surrounding space", input: "  #123456  ", want: 0x123456},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHexColor(tc.input); got != tc.want {
				t.Fatalf("parseHexColor(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestResolveMessage(t *testing.T) {
	feedMsg := &v1beta1.MessageSpec{Content: "feed"}
	groupMsg := &v1beta1.MessageSpec{Content: "group"}

	if got := resolveMessage(&v1beta1.FeedGroup{Spec: v1beta1.FeedGroupSpec{Message: groupMsg}}, &v1beta1.FeedSpec{Message: feedMsg}); got != feedMsg {
		t.Fatal("expected the feed's message to take precedence")
	}
	if got := resolveMessage(&v1beta1.FeedGroup{Spec: v1beta1.FeedGroupSpec{Message: groupMsg}}, &v1beta1.FeedSpec{}); got != groupMsg {
		t.Fatal("expected fallback to the group's message")
	}
	if got := resolveMessage(&v1beta1.FeedGroup{}, &v1beta1.FeedSpec{}); got.Content != v1beta1.DefaultMessageContent || got.Embed != nil {
		t.Fatalf("expected the default message, got %+v", got)
	}
}

func TestNewFeedRenderer_ConfigErrors(t *testing.T) {
	cases := map[string]*v1beta1.FeedSpec{
		"content":     msgFeed(v1beta1.MessageSpec{Content: tmplUnclosed}),
		"title":       embedFeed(v1beta1.EmbedSpec{Title: tmplUnclosed}),
		"description": embedFeed(v1beta1.EmbedSpec{Description: tmplUnclosed}),
		"author":      embedFeed(v1beta1.EmbedSpec{Author: &v1beta1.EmbedAuthor{Name: tmplUnclosed}}),
		"footer":      embedFeed(v1beta1.EmbedSpec{Footer: &v1beta1.EmbedFooter{Text: tmplUnclosed}}),
		"forum post":  {ForumPost: &v1beta1.ForumPostSpec{Name: tmplUnclosed}},
		"filter":      {Filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "("}}},
	}
	for name, feed := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newFeedRenderer(&v1beta1.FeedGroup{}, feed); err == nil {
				t.Fatal("expected a configuration error")
			}
		})
	}
}

func TestMentionText(t *testing.T) {
	if got := mentionText(nil); got != "" {
		t.Fatalf("mentionText(nil) = %q", got)
	}
	if got := mentionText(&v1beta1.Mentions{}); got != "" {
		t.Fatalf("mentionText(empty) = %q", got)
	}
}

// msgFeed is a feed whose message is m.
func msgFeed(m v1beta1.MessageSpec) *v1beta1.FeedSpec { return &v1beta1.FeedSpec{Message: &m} }

// embedFeed is a feed whose message is just the embed e.
func embedFeed(e v1beta1.EmbedSpec) *v1beta1.FeedSpec {
	return msgFeed(v1beta1.MessageSpec{Embed: &e})
}

// mustBuild renders entry through newFeedRenderer(group, feed).build, the same
// path processFeed uses, failing the test on a configuration error. The render
// error (if any) is returned for tests that assert on it.
func mustBuild(t *testing.T, group *v1beta1.FeedGroup, feed *v1beta1.FeedSpec, entry rss.Entry) (discord.Message, error) {
	t.Helper()
	fr, err := newFeedRenderer(group, feed)
	if err != nil {
		t.Fatalf("newFeedRenderer: %v", err)
	}
	msg, _, err := fr.build(entry)
	return msg, err
}

// buildOK is mustBuild for a render that must succeed.
func buildOK(t *testing.T, group *v1beta1.FeedGroup, feed *v1beta1.FeedSpec, entry rss.Entry) discord.Message {
	t.Helper()
	msg, err := mustBuild(t, group, feed, entry)
	if err != nil {
		t.Fatalf("unexpected render error: %v", err)
	}
	return msg
}

// requireBuildError is mustBuild for a render that must fail.
func requireBuildError(t *testing.T, group *v1beta1.FeedGroup, feed *v1beta1.FeedSpec, entry rss.Entry) {
	t.Helper()
	if _, err := mustBuild(t, group, feed, entry); err == nil {
		t.Fatal("expected a render error")
	}
}
