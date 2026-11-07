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
	"time"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// embedView is the part of a discord.Embed the text-field tests compare.
type embedView struct {
	Title, Description, URL              string
	AuthorName, AuthorURL, AuthorIconURL string
	FooterText, FooterIconURL            string
	Color                                int
}

func viewOf(e *discord.Embed) embedView {
	return embedView{
		Title: e.Title, Description: e.Description, URL: e.URL,
		AuthorName: e.AuthorName, AuthorURL: e.AuthorURL, AuthorIconURL: e.AuthorIconURL,
		FooterText: e.FooterText, FooterIconURL: e.FooterIconURL,
		Color: e.Color,
	}
}

// viewOfOrZero is viewOf for a message that may carry no embed; a missing embed
// reads as the zero view, so assertions on its fields fail rather than panic.
func viewOfOrZero(msg discord.Message) embedView {
	if msg.Embed == nil {
		return embedView{}
	}
	return viewOf(msg.Embed)
}

// TestFeedRendererBuild_EmbedTimestamp asserts the embed timestamp is the
// entry's published time by default, empty for an undated entry (discord.Embed
// omits it from the payload instead of sending year 0001) and for
// timestamp None.
const publishedRFC3339 = "2024-05-06T07:08:09Z"

func TestFeedRendererBuild_EmbedTimestamp(t *testing.T) {
	dated := time.Date(2024, 5, 6, 9, 8, 9, 0, time.FixedZone("x", 2*3600))
	cases := []struct {
		name, timestamp string
		published       time.Time
		want            string
	}{
		{name: "zero published", want: ""},
		{name: "default is published", published: dated, want: publishedRFC3339},
		{name: "explicit published", timestamp: v1beta1.EmbedTimestampPublished, published: dated, want: publishedRFC3339},
		{name: "none", timestamp: v1beta1.EmbedTimestampNone, published: dated, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := buildOK(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{Timestamp: tc.timestamp}), rss.Entry{Title: "T", Published: tc.published})
			if msg.Embed == nil || msg.Embed.Timestamp != tc.want {
				t.Fatalf("embed = %+v, want timestamp %q", msg.Embed, tc.want)
			}
		})
	}
}

// TestFeedRendererBuild_EmbedImageStyle asserts the entry image goes to the
// small thumbnail by default, to the large image slot for Image, nowhere for
// None, and that non-http(s) image URLs are dropped either way.
func TestFeedRendererBuild_EmbedImageStyle(t *testing.T) {
	cases := []struct {
		name, style, image   string
		wantThumb, wantLarge string
	}{
		{name: "default is thumbnail", image: imageFixtureURL, wantThumb: imageFixtureURL},
		{name: "explicit thumbnail", style: v1beta1.EmbedImageStyleThumbnail, image: imageFixtureURL, wantThumb: imageFixtureURL},
		{name: "image style", style: v1beta1.EmbedImageStyleImage, image: imageFixtureURL, wantLarge: imageFixtureURL},
		{name: "none", style: v1beta1.EmbedImageStyleNone, image: imageFixtureURL},
		{name: "unsafe scheme dropped", style: v1beta1.EmbedImageStyleImage, image: jsAlertURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := buildOK(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{ImageStyle: tc.style}), rss.Entry{Title: "T", Image: tc.image})
			if msg.Embed.ThumbnailURL != tc.wantThumb || msg.Embed.ImageURL != tc.wantLarge {
				t.Fatalf("thumbnail=%q image=%q, want %q / %q", msg.Embed.ThumbnailURL, msg.Embed.ImageURL, tc.wantThumb, tc.wantLarge)
			}
		})
	}
}

// TestFeedRendererBuild_EmbedTemplates asserts every embed text field is a
// template over the sanitized entry, with title and description defaulting to
// the entry's.
func TestFeedRendererBuild_EmbedTemplates(t *testing.T) {
	entry := rss.Entry{Title: "<b>Breaking</b> News", Description: testDesc, Author: "Jane", FeedTitle: "Feed", Link: testArticleURL}

	cases := []struct {
		name  string
		spec  v1beta1.EmbedSpec
		entry rss.Entry
		want  embedView
	}{
		{
			name:  "defaults",
			entry: entry,
			want:  embedView{Title: "Breaking News", Description: testDesc, URL: testArticleURL},
		},
		{
			name: "templated",
			spec: v1beta1.EmbedSpec{
				Title:       "[{{.FeedTitle}}] {{.Title}}",
				Description: "{{upper .Description}}",
				Color:       "#00ff00",
				Author:      &v1beta1.EmbedAuthor{Name: "by {{.Author}}"},
				Footer:      &v1beta1.EmbedFooter{Text: "via {{.FeedTitle}}"},
			},
			entry: entry,
			want:  embedView{Title: "[Feed] Breaking News", Description: "DESC", URL: testArticleURL, AuthorName: "by Jane", FooterText: "via Feed", Color: 0x00ff00},
		},
		{
			name: "blank author and footer are left out",
			spec: v1beta1.EmbedSpec{
				Author: &v1beta1.EmbedAuthor{Name: "{{.Author}}", URL: "https://a.example.com", IconURL: "https://a.example.com/i.png"},
				Footer: &v1beta1.EmbedFooter{Text: " {{.Author}} ", IconURL: "https://f.example.com/i.png"},
			},
			entry: rss.Entry{Title: "T"},
			want:  embedView{Title: "T"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := buildOK(t, &v1beta1.FeedGroup{}, embedFeed(tc.spec), tc.entry)
			if got := viewOf(msg.Embed); got != tc.want {
				t.Fatalf("embed = %+v, want %+v", got, tc.want)
			}
		})
	}

	for name, spec := range map[string]v1beta1.EmbedSpec{
		"title":       {Title: tmplMissingField},
		"description": {Description: tmplMissingField},
		"author":      {Author: &v1beta1.EmbedAuthor{Name: tmplMissingField}},
		"footer":      {Footer: &v1beta1.EmbedFooter{Text: tmplMissingField}},
	} {
		t.Run(name+" execution error", func(t *testing.T) {
			requireBuildError(t, &v1beta1.FeedGroup{}, embedFeed(spec), rss.Entry{})
		})
	}
}

// TestFeedRendererBuild_EmbedAuthorAndFooterURLs tests author/footer URL
// sanitization and propagation to the discord.Embed.
func TestFeedRendererBuild_EmbedAuthorAndFooterURLs(t *testing.T) {
	cases := []struct {
		name                              string
		authorURL, authorIcon, footerIcon string
		want                              embedView
	}{
		{
			name:      "valid http and https URLs are preserved",
			authorURL: "https://author.example.com", authorIcon: "http://author.example.com/icon.png", footerIcon: "https://footer.example.com/icon.png",
			want: embedView{AuthorURL: "https://author.example.com", AuthorIconURL: "http://author.example.com/icon.png", FooterIconURL: "https://footer.example.com/icon.png"},
		},
		{
			name:      "unsafe URL schemes are sanitized to empty",
			authorURL: "javascript:alert(1)", authorIcon: "data:image/png;base64,abc", footerIcon: "ftp://example.com/icon.png",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := buildOK(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{
				Author: &v1beta1.EmbedAuthor{Name: "Author", URL: tc.authorURL, IconURL: tc.authorIcon},
				Footer: &v1beta1.EmbedFooter{Text: "Footer", IconURL: tc.footerIcon},
			}), rss.Entry{Title: testTitle})
			got := viewOf(msg.Embed)
			urls := embedView{AuthorURL: got.AuthorURL, AuthorIconURL: got.AuthorIconURL, FooterIconURL: got.FooterIconURL}
			if urls != tc.want {
				t.Fatalf("embed = %+v, want %+v", got, tc.want)
			}
		})
	}
}
