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
	"slices"
	"testing"
	"time"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/msgtemplate"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

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

// msgFeed is a feed whose message is m.
func msgFeed(m v1beta1.MessageSpec) *v1beta1.FeedSpec { return &v1beta1.FeedSpec{Message: &m} }

// embedFeed is a feed whose message is just the embed e.
func embedFeed(e v1beta1.EmbedSpec) *v1beta1.FeedSpec {
	return msgFeed(v1beta1.MessageSpec{Embed: &e})
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

func TestRenderTemplate_AuthorAndCategories(t *testing.T) {
	tmpl, err := msgtemplate.Compile("test", "{{.Author}} | {{.Categories}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := rss.Entry{Author: "Jane Doe", Categories: []string{"Go", "Kubernetes"}}
	got, err := renderTemplate(tmpl, displayOf(entry))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Jane Doe | Go, Kubernetes"; got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}

	t.Run("empty author and categories render blank", func(t *testing.T) {
		got, err := renderTemplate(tmpl, displayOf(rss.Entry{}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := " | "; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})
}

// TestRenderTemplate_Published asserts a date-less entry (zero Published)
// renders an empty {{.Published}} rather than "0001-01-01T00:00:00Z", while a
// real date is still formatted as RFC 3339 UTC.
func TestRenderTemplate_Published(t *testing.T) {
	tmpl, err := msgtemplate.Compile("test", "[{{.Published}}]", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("zero published renders empty", func(t *testing.T) {
		got, err := renderTemplate(tmpl, displayOf(rss.Entry{}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "[]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("non-zero published unchanged", func(t *testing.T) {
		entry := rss.Entry{Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		got, err := renderTemplate(tmpl, displayOf(entry))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "[2024-05-06T07:08:09Z]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})
}

// TestRenderTemplate_NewFields covers the placeholders added after the
// original six: Content, Image, FeedTitle and PublishedUnix.
func TestRenderTemplate_NewFields(t *testing.T) {
	render := func(t *testing.T, format string, entry rss.Entry) string {
		t.Helper()
		tmpl, err := msgtemplate.Compile("test", format, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := renderTemplate(tmpl, displayOf(entry))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return got
	}

	t.Run("Content is HTML-stripped", func(t *testing.T) {
		entry := rss.Entry{Content: "<p>Full <b>body</b> &amp; more</p><script>alert(1)</script>"}
		if got, want := render(t, "{{.Content}}", entry), "Full body & more"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("Content is distinct from Description", func(t *testing.T) {
		entry := rss.Entry{Description: shortContent, Content: "long"}
		if got, want := render(t, "{{.Description}}/{{.Content}}", entry), shortContent+"/long"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("Image passes http(s) URLs through", func(t *testing.T) {
		entry := rss.Entry{Image: "https://cdn.example.com/a.jpg"}
		if got, want := render(t, "{{.Image}}", entry), "https://cdn.example.com/a.jpg"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("Image with unsafe scheme is blanked", func(t *testing.T) {
		for _, image := range []string{"javascript:void(0)", "data:text/html,<script>x</script>", "ftp://example.com/a.jpg"} {
			if got := render(t, "[{{.Image}}]", rss.Entry{Image: image}); got != "[]" {
				t.Fatalf("Image %q rendered %q, want it blanked", image, got)
			}
		}
	})

	t.Run("FeedTitle is HTML-stripped", func(t *testing.T) {
		entry := rss.Entry{FeedTitle: "<b>Example</b> &amp; Co"}
		if got, want := render(t, "{{.FeedTitle}}", entry), "Example & Co"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("PublishedUnix is Unix seconds", func(t *testing.T) {
		// A non-UTC zone: the Unix value is zone-independent.
		published := time.Date(2024, 5, 6, 9, 8, 9, 0, time.FixedZone("x", 2*3600))
		got := render(t, "<t:{{.PublishedUnix}}:R>", rss.Entry{Published: published})
		if want := discordRelativeTS; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("zero PublishedUnix is 0 and guardable", func(t *testing.T) {
		const format = "{{if .PublishedUnix}}<t:{{.PublishedUnix}}:R>{{end}}"
		if got := render(t, "<t:{{.PublishedUnix}}:R>", rss.Entry{}); got != "<t:0:R>" {
			t.Fatalf("renderTemplate() = %q, want %q", got, "<t:0:R>")
		}
		if got := render(t, format, rss.Entry{}); got != "" {
			t.Fatalf("guarded template rendered %q for an undated entry, want empty", got)
		}
		dated := rss.Entry{Published: time.Unix(1714979289, 0)}
		if got, want := render(t, format, dated), discordRelativeTS; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("all new fields empty on a zero entry", func(t *testing.T) {
		if got, want := render(t, "[{{.Content}}|{{.Image}}|{{.FeedTitle}}]", rss.Entry{}), "[||]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})
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

func TestFeedRendererBuild_DefaultMessage(t *testing.T) {
	msg, err := mustBuild(t, &v1beta1.FeedGroup{}, &v1beta1.FeedSpec{}, rss.Entry{Title: "T", Description: "D", Link: testArticleURL})
	if err != nil {
		t.Fatal(err)
	}
	if want := "**T**\nD\n[Read more](https://example.com/a)"; msg.Content != want || msg.Embed != nil {
		t.Fatalf("msg = %+v, want content %q and no embed", msg, want)
	}
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
			msg, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{Timestamp: tc.timestamp}), rss.Entry{Title: "T", Published: tc.published})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
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
			msg, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{ImageStyle: tc.style}), rss.Entry{Title: "T", Image: tc.image})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg.Embed.ThumbnailURL != tc.wantThumb || msg.Embed.ImageURL != tc.wantLarge {
				t.Fatalf("thumbnail=%q image=%q, want %q / %q", msg.Embed.ThumbnailURL, msg.Embed.ImageURL, tc.wantThumb, tc.wantLarge)
			}
		})
	}
}

func TestRenderTemplate_StripsHTMLFromTitle(t *testing.T) {
	tmpl, err := msgtemplate.Compile("test", tmplTitle, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := rss.Entry{Title: "<b>Breaking</b> News &amp; Views"}
	got, err := renderTemplate(tmpl, displayOf(entry))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Breaking News & Views"; got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}
}

// TestFeedRendererBuild_EmbedTemplates asserts every embed text field is a
// template over the sanitized entry, with title and description defaulting to
// the entry's.
func TestFeedRendererBuild_EmbedTemplates(t *testing.T) {
	entry := rss.Entry{Title: "<b>Breaking</b> News", Description: testDesc, Author: "Jane", FeedTitle: "Feed", Link: testArticleURL}

	t.Run("defaults", func(t *testing.T) {
		msg, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{}), entry)
		if err != nil {
			t.Fatal(err)
		}
		e := msg.Embed
		if e.Title != "Breaking News" || e.Description != testDesc || e.URL != testArticleURL || e.AuthorName != "" || e.FooterText != "" {
			t.Fatalf("embed = %+v", e)
		}
	})

	t.Run("templated", func(t *testing.T) {
		msg, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{
			Title:       "[{{.FeedTitle}}] {{.Title}}",
			Description: "{{upper .Description}}",
			Color:       "#00ff00",
			Author:      &v1beta1.EmbedAuthor{Name: "by {{.Author}}"},
			Footer:      &v1beta1.EmbedFooter{Text: "via {{.FeedTitle}}"},
		}), entry)
		if err != nil {
			t.Fatal(err)
		}
		e := msg.Embed
		if e.Title != "[Feed] Breaking News" || e.Description != "DESC" || e.AuthorName != "by Jane" || e.FooterText != "via Feed" || e.Color != 0x00ff00 {
			t.Fatalf("embed = %+v", e)
		}
	})

	t.Run("blank author and footer are left out", func(t *testing.T) {
		msg, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{
			Author: &v1beta1.EmbedAuthor{Name: "{{.Author}}", URL: "https://a.example.com", IconURL: "https://a.example.com/i.png"},
			Footer: &v1beta1.EmbedFooter{Text: " {{.Author}} ", IconURL: "https://f.example.com/i.png"},
		}), rss.Entry{Title: "T"})
		if err != nil {
			t.Fatal(err)
		}
		e := msg.Embed
		if e.AuthorName != "" || e.AuthorURL != "" || e.AuthorIconURL != "" || e.FooterText != "" || e.FooterIconURL != "" {
			t.Fatalf("embed = %+v, want no author or footer", e)
		}
	})

	for name, spec := range map[string]v1beta1.EmbedSpec{
		"title":       {Title: tmplMissingField},
		"description": {Description: tmplMissingField},
		"author":      {Author: &v1beta1.EmbedAuthor{Name: tmplMissingField}},
		"footer":      {Footer: &v1beta1.EmbedFooter{Text: tmplMissingField}},
	} {
		t.Run(name+" execution error", func(t *testing.T) {
			if _, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(spec), rss.Entry{}); err == nil {
				t.Fatal("expected a render error")
			}
		})
	}
}

// TestFeedRendererBuild_ContentAndEmbed asserts content and embed are sent
// together, and that blank content is only an error without an embed.
func TestFeedRendererBuild_ContentAndEmbed(t *testing.T) {
	entry := rss.Entry{Title: "T"}

	msg, err := mustBuild(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Content: "New: {{.Title}}", Embed: &v1beta1.EmbedSpec{}}), entry)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "New: T" || msg.Embed == nil || msg.Embed.Title != "T" {
		t.Fatalf("msg = %+v, want both content and embed", msg)
	}

	msg, err = mustBuild(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Content: tmplPublished, Embed: &v1beta1.EmbedSpec{}}), entry)
	if err != nil {
		t.Fatalf("blank content beside an embed should not be an error: %v", err)
	}
	if msg.Embed == nil {
		t.Fatal("expected the embed")
	}

	if _, err := mustBuild(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Content: "x{{.Unknown}}"}), entry); err == nil {
		t.Fatal("expected a content execution error")
	}
}

// TestFeedRendererBuild_EmptyRenderIsError asserts a template that renders
// to blank content or a blank forum post name (here: only {{.Published}},
// for an entry with no date) is reported as a render error, so the
// render-error path can skip the entry, rather than producing a message
// Discord would reject on every retry.
func TestFeedRendererBuild_EmptyRenderIsError(t *testing.T) {
	feedGroup := &v1beta1.FeedGroup{}
	entry := rss.Entry{Title: "T"}

	t.Run("blank content", func(t *testing.T) {
		if _, err := mustBuild(t, feedGroup, msgFeed(v1beta1.MessageSpec{Content: " {{.Published}} "}), entry); err == nil {
			t.Fatal("expected an error for content that renders blank")
		}
	})

	t.Run("blank content with mentions", func(t *testing.T) {
		feed := msgFeed(v1beta1.MessageSpec{Content: tmplPublished, Mentions: &v1beta1.Mentions{RoleIDs: []string{"1"}}})
		if _, err := mustBuild(t, feedGroup, feed, entry); err == nil {
			t.Fatal("expected an error for content that renders blank, pings or not")
		}
	})

	t.Run("blank forum post name", func(t *testing.T) {
		feed := &v1beta1.FeedSpec{ForumPost: &v1beta1.ForumPostSpec{Name: tmplPublished}}
		if _, err := mustBuild(t, feedGroup, feed, entry); err == nil {
			t.Fatal("expected an error for a forum post name that renders blank")
		}
	})

	t.Run("forum post name execution error", func(t *testing.T) {
		feed := &v1beta1.FeedSpec{ForumPost: &v1beta1.ForumPostSpec{Name: tmplMissingField}}
		if _, err := mustBuild(t, feedGroup, feed, entry); err == nil {
			t.Fatal("expected an error from a forum post name that fails at execution")
		}
	})

	t.Run("dated entry still renders", func(t *testing.T) {
		dated := rss.Entry{Title: "T", Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		msg, err := mustBuild(t, feedGroup, msgFeed(v1beta1.MessageSpec{Content: " {{.Published}} "}), dated)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := publishedRFC3339; msg.Content != want {
			t.Fatalf("msg.Content = %q, want %q", msg.Content, want)
		}
	})
}

func TestFeedRendererBuild_ThreadTargets(t *testing.T) {
	entry := rss.Entry{Title: "Hello"}

	msg, err := mustBuild(t, &v1beta1.FeedGroup{}, &v1beta1.FeedSpec{Thread: &v1beta1.ThreadTarget{ID: "123456"}}, entry)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ThreadID != "123456" || msg.ThreadName != "" || msg.AppliedTags != nil {
		t.Fatalf("msg = %+v, want only ThreadID", msg)
	}

	msg, err = mustBuild(t, &v1beta1.FeedGroup{}, &v1beta1.FeedSpec{ForumPost: &v1beta1.ForumPostSpec{Name: tmplTitle, AppliedTagIDs: []string{"7", "8"}}}, entry)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ThreadName != "Hello" || msg.ThreadID != "" || !slices.Equal(msg.AppliedTags, []string{"7", "8"}) {
		t.Fatalf("msg = %+v, want a tagged forum post", msg)
	}
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
		msg, err := mustBuild(t, group, embedFeed(v1beta1.EmbedSpec{Description: "feed:{{.Title}}"}), entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg.Embed == nil || msg.Embed.Description != "feed:T" || msg.Embed.FooterText != "" || msg.Silent {
			t.Fatalf("msg = %+v, want the feed's message only", msg)
		}
	})

	t.Run("feed text message replaces group embed", func(t *testing.T) {
		msg, err := mustBuild(t, group, msgFeed(v1beta1.MessageSpec{Content: "plain {{.Title}}"}), entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg.Embed != nil || msg.Content != "plain T" {
			t.Fatalf("msg = %+v, want plain content without embed", msg)
		}
	})

	t.Run("group message inherited when feed has none", func(t *testing.T) {
		msg, err := mustBuild(t, group, &v1beta1.FeedSpec{}, entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg.Embed == nil || msg.Embed.Description != "group:T" || msg.Embed.FooterText != "group-footer" || !msg.Silent {
			t.Fatalf("msg = %+v, want the group's message", msg)
		}
	})
}

func TestFeedRendererBuild_MentionsAndFlags(t *testing.T) {
	mentions := &v1beta1.Mentions{RoleIDs: []string{"11", "12"}, UserIDs: []string{"21"}}
	entry := rss.Entry{Title: "T"}

	msg, err := mustBuild(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Content: tmplTitle, Mentions: mentions, Silent: true, SuppressLinkPreviews: true}), entry)
	if err != nil {
		t.Fatal(err)
	}
	if want := "<@&11> <@&12> <@21>\nT"; msg.Content != want {
		t.Fatalf("Content = %q, want %q", msg.Content, want)
	}
	if !slices.Equal(msg.MentionRoleIDs, mentions.RoleIDs) || !slices.Equal(msg.MentionUserIDs, mentions.UserIDs) || !msg.Silent || !msg.SuppressEmbeds {
		t.Fatalf("msg = %+v", msg)
	}

	msg, err = mustBuild(t, &v1beta1.FeedGroup{}, msgFeed(v1beta1.MessageSpec{Embed: &v1beta1.EmbedSpec{}, Mentions: &v1beta1.Mentions{UserIDs: []string{"5"}}}), entry)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "<@5>" || msg.MentionRoleIDs != nil {
		t.Fatalf("msg = %+v, want the pings alone beside the embed", msg)
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
			msg, err := mustBuild(t, group, feed, entry)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg.Username != tc.wantUsername || msg.AvatarURL != tc.wantAvatar {
				t.Errorf("Username, AvatarURL = %q, %q, want %q, %q", msg.Username, msg.AvatarURL, tc.wantUsername, tc.wantAvatar)
			}
		})
	}
}

// TestFeedRendererBuild_EmbedAuthorAndFooterURLs tests author/footer URL
// sanitization and propagation to the discord.Embed.
func TestFeedRendererBuild_EmbedAuthorAndFooterURLs(t *testing.T) {
	entry := rss.Entry{Title: testTitle}
	build := func(authorURL, authorIcon, footerIcon string) *discord.Embed {
		t.Helper()
		msg, err := mustBuild(t, &v1beta1.FeedGroup{}, embedFeed(v1beta1.EmbedSpec{
			Author: &v1beta1.EmbedAuthor{Name: "Author", URL: authorURL, IconURL: authorIcon},
			Footer: &v1beta1.EmbedFooter{Text: "Footer", IconURL: footerIcon},
		}), entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return msg.Embed
	}

	t.Run("valid http and https URLs are preserved", func(t *testing.T) {
		e := build("https://author.example.com", "http://author.example.com/icon.png", "https://footer.example.com/icon.png")
		if e.AuthorURL != "https://author.example.com" || e.AuthorIconURL != "http://author.example.com/icon.png" || e.FooterIconURL != "https://footer.example.com/icon.png" {
			t.Fatalf("embed = %+v", e)
		}
	})

	t.Run("unsafe URL schemes are sanitized to empty", func(t *testing.T) {
		e := build("javascript:alert(1)", "data:image/png;base64,abc", "ftp://example.com/icon.png")
		if e.AuthorURL != "" || e.AuthorIconURL != "" || e.FooterIconURL != "" {
			t.Fatalf("embed = %+v, want unsafe URLs dropped", e)
		}
	})
}
