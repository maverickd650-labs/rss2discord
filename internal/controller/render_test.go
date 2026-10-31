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
	"testing"
	"time"

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/msgtemplate"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
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

func TestHTTPURLOrEmpty(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "https kept", input: "https://example.com/a", want: "https://example.com/a"},
		{name: "http kept", input: "http://example.com/a", want: "http://example.com/a"},
		{name: "javascript dropped", input: jsAlertURL, want: ""},
		{name: "data dropped", input: "data:text/html,<script>", want: ""},
		{name: "empty dropped", input: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpURLOrEmpty(tc.input); got != tc.want {
				t.Fatalf("httpURLOrEmpty(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestResolveEmbedSpec(t *testing.T) {
	feedEmbed := &v1alpha1.EmbedSpec{Color: "#feed00"}
	groupEmbed := &v1alpha1.EmbedSpec{Color: "#9009ff"}

	t.Run("feed embed takes precedence", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Embed: groupEmbed}}
		got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{Embed: feedEmbed})
		if got != feedEmbed {
			t.Fatal("expected feed embed to take precedence")
		}
	})

	t.Run("falls back to group embed", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Embed: groupEmbed}}
		got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{})
		if got != groupEmbed {
			t.Fatal("expected fallback to group embed")
		}
	})

	t.Run("nil when neither set", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		if got := resolveEmbedSpec(fg, &v1alpha1.FeedSpec{}); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})
}

func TestCompileMessageTemplate_Precedence(t *testing.T) {
	t.Run("feed format wins", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{Format: "group {{.Title}}"}}
		tmpl, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{Format: "feed {{.Title}}"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := tmpl.Execute(map[string]string{"Title": "X"})
		if err != nil {
			t.Fatalf("execute error: %v", err)
		}
		if got != "feed X" {
			t.Fatalf("expected feed format used, got %q", got)
		}
	})

	t.Run("falls back to default when both blank", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		tmpl, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tmpl == nil {
			t.Fatal("expected a compiled default template")
		}
	})

	t.Run("invalid template returns error", func(t *testing.T) {
		fg := &v1alpha1.FeedGroup{}
		if _, err := compileMessageTemplate(fg, &v1alpha1.FeedSpec{Format: "{{.Unclosed"}); err == nil {
			t.Fatal("expected parse error for invalid template, got nil")
		}
	})
}

func TestRenderTemplate_AuthorAndCategories(t *testing.T) {
	tmpl, err := msgtemplate.Compile("test", "{{.Author}} | {{.Categories}}", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry := rss.Entry{Author: "Jane Doe", Categories: []string{"Go", "Kubernetes"}}
	got, err := renderTemplate(tmpl, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Jane Doe | Go, Kubernetes"; got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}

	t.Run("empty author and categories render blank", func(t *testing.T) {
		got, err := renderTemplate(tmpl, rss.Entry{})
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
		got, err := renderTemplate(tmpl, rss.Entry{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "[]"; got != want {
			t.Fatalf("renderTemplate() = %q, want %q", got, want)
		}
	})

	t.Run("non-zero published unchanged", func(t *testing.T) {
		entry := rss.Entry{Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		got, err := renderTemplate(tmpl, entry)
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
		got, err := renderTemplate(tmpl, entry)
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
func mustBuild(t *testing.T, group *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec, entry rss.Entry) (discord.Message, error) {
	t.Helper()
	fr, err := newFeedRenderer(group, feed)
	if err != nil {
		t.Fatalf("newFeedRenderer: %v", err)
	}
	msg, _, err := fr.build(entry)
	return msg, err
}

// TestFeedRendererBuild_EmbedTimestamp asserts an entry without a publish
// date leaves the embed timestamp empty (discord.Embed omits it from the
// payload) instead of sending year 0001, and that real dates are unchanged.
func TestFeedRendererBuild_EmbedTimestamp(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	feed := &v1alpha1.FeedSpec{Embed: &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: defaultDescriptionFormat}}

	cases := []struct {
		name      string
		published time.Time
		want      string
	}{
		{name: "zero published", want: ""},
		{name: "non-zero published", published: time.Date(2024, 5, 6, 9, 8, 9, 0, time.FixedZone("x", 2*3600)), want: "2024-05-06T07:08:09Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := mustBuild(t, feedGroup, feed, rss.Entry{Title: "T", Published: tc.published})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if msg.Embed == nil {
				t.Fatal("expected an embed to be set")
			}
			if msg.Embed.Timestamp != tc.want {
				t.Fatalf("msg.Embed.Timestamp = %q, want %q", msg.Embed.Timestamp, tc.want)
			}
		})
	}
}

// TestFeedRendererBuild_EmbedImageStyle asserts the entry image goes to the
// small thumbnail by default and to the large image slot for imageStyle
// "image", and that non-http(s) image URLs are dropped either way.
func TestFeedRendererBuild_EmbedImageStyle(t *testing.T) {
	cases := []struct {
		name, style, image   string
		wantThumb, wantLarge string
	}{
		{name: "default is thumbnail", image: imageFixtureURL, wantThumb: imageFixtureURL},
		{name: "explicit thumbnail", style: "thumbnail", image: imageFixtureURL, wantThumb: imageFixtureURL},
		{name: "image style", style: "image", image: imageFixtureURL, wantLarge: imageFixtureURL},
		{name: "unsafe scheme dropped", style: "image", image: jsAlertURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feed := &v1alpha1.FeedSpec{Embed: &v1alpha1.EmbedSpec{Enabled: true, ImageStyle: tc.style, DescriptionFormat: defaultDescriptionFormat}}
			msg, err := mustBuild(t, &v1alpha1.FeedGroup{}, feed, rss.Entry{Title: "T", Image: tc.image})
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
	got, err := renderTemplate(tmpl, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Breaking News & Views"; got != want {
		t.Fatalf("renderTemplate() = %q, want %q", got, want)
	}
}

func TestFeedRendererBuild_EmbedTitleStripsHTML(t *testing.T) {
	feed := &v1alpha1.FeedSpec{Embed: &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: defaultDescriptionFormat}}

	msg, err := mustBuild(t, &v1alpha1.FeedGroup{}, feed, rss.Entry{Title: "<b>Breaking</b> News"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Embed == nil {
		t.Fatal("expected an embed to be set")
	}
	if want := "Breaking News"; msg.Embed.Title != want {
		t.Fatalf("msg.Embed.Title = %q, want %q", msg.Embed.Title, want)
	}
}

// TestFeedRendererBuild_EmbedDescriptionRenderError asserts a description
// template that parses but fails at execution (referencing a field absent
// from renderTemplate's render data) surfaces as an error instead of a
// blank/partial embed.
func TestFeedRendererBuild_EmbedDescriptionRenderError(t *testing.T) {
	feed := &v1alpha1.FeedSpec{Embed: &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: tmplMissingField}}

	if _, err := mustBuild(t, &v1alpha1.FeedGroup{}, feed, rss.Entry{}); err == nil {
		t.Fatal("expected an error from a description template that fails at execution")
	}
}

// TestFeedRendererBuild_ForumThreadNameRenderError asserts a forum thread
// name template that parses but fails at execution surfaces as an error,
// distinct from the content/description render-error paths.
func TestFeedRendererBuild_ForumThreadNameRenderError(t *testing.T) {
	feed := &v1alpha1.FeedSpec{Format: tmplTitle, ForumThreadName: tmplMissingField}

	if _, err := mustBuild(t, &v1alpha1.FeedGroup{}, feed, rss.Entry{}); err == nil {
		t.Fatal("expected an error from a forum thread name template that fails at execution")
	}
}

// TestFeedRendererBuild_EmptyRenderIsError asserts a template that renders
// to blank content or a blank forum thread name (here: only {{.Published}},
// for an entry with no date) is reported as a render error, so the
// render-error path can skip the entry, rather than producing a message
// Discord would reject on every retry.
func TestFeedRendererBuild_EmptyRenderIsError(t *testing.T) {
	feedGroup := &v1alpha1.FeedGroup{}
	entry := rss.Entry{Title: "T"}

	t.Run("blank content", func(t *testing.T) {
		feed := &v1alpha1.FeedSpec{Format: " {{.Published}} "}
		if _, err := mustBuild(t, feedGroup, feed, entry); err == nil {
			t.Fatal("expected an error for content that renders blank")
		}
	})

	t.Run("blank forum thread name", func(t *testing.T) {
		feed := &v1alpha1.FeedSpec{Format: tmplTitle, ForumThreadName: "{{.Published}}"}
		if _, err := mustBuild(t, feedGroup, feed, entry); err == nil {
			t.Fatal("expected an error for a forum thread name that renders blank")
		}
	})

	t.Run("dated entry still renders", func(t *testing.T) {
		dated := rss.Entry{Title: "T", Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		feed := &v1alpha1.FeedSpec{Format: " {{.Published}} "}
		msg, err := mustBuild(t, feedGroup, feed, dated)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "2024-05-06T07:08:09Z"; msg.Content != want {
			t.Fatalf("msg.Content = %q, want %q", msg.Content, want)
		}
	})
}

// TestFeedRendererBuild_ForumThreadIDTakesPrecedenceOverThreadName asserts
// an explicit ForumThreadID (posting into an existing thread) wins over a
// configured ForumThreadName template (which creates a new thread), even
// when both are set.
func TestFeedRendererBuild_ForumThreadIDTakesPrecedenceOverThreadName(t *testing.T) {
	feed := &v1alpha1.FeedSpec{Format: tmplTitle, ForumThreadID: "123456", ForumThreadName: tmplTitle}

	msg, err := mustBuild(t, &v1alpha1.FeedGroup{}, feed, rss.Entry{Title: "Hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.ThreadID != "123456" {
		t.Fatalf("msg.ThreadID = %q, want %q", msg.ThreadID, "123456")
	}
	if msg.ThreadName != "" {
		t.Fatalf("expected ThreadName to stay empty when ForumThreadID is set, got %q", msg.ThreadName)
	}
}

// TestFeedRendererBuild_EmbedResolution asserts a feed-level Embed replaces
// the group-level one wholesale, and that a feed with none inherits the group's.
func TestFeedRendererBuild_EmbedResolution(t *testing.T) {
	group := &v1alpha1.FeedGroup{Spec: v1alpha1.FeedGroupSpec{
		Embed: &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: "group:{{.Title}}", FooterText: "group-footer"},
	}}
	entry := rss.Entry{Title: "T"}

	t.Run("feed embed overrides group embed", func(t *testing.T) {
		feed := &v1alpha1.FeedSpec{Embed: &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: "feed:{{.Title}}"}}
		msg, err := mustBuild(t, group, feed, entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg.Embed == nil || msg.Embed.Description != "feed:T" || msg.Embed.FooterText != "" {
			t.Fatalf("embed = %+v, want the feed's embed only", msg.Embed)
		}
	})

	t.Run("feed embed can disable group embed", func(t *testing.T) {
		feed := &v1alpha1.FeedSpec{Format: "plain {{.Title}}", Embed: &v1alpha1.EmbedSpec{Enabled: false}}
		msg, err := mustBuild(t, group, feed, entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg.Embed != nil || msg.Content != "plain T" {
			t.Fatalf("msg = %+v, want plain content without embed", msg)
		}
	})

	t.Run("group embed inherited when feed has none", func(t *testing.T) {
		msg, err := mustBuild(t, group, &v1alpha1.FeedSpec{}, entry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg.Embed == nil || msg.Embed.Description != "group:T" || msg.Embed.FooterText != "group-footer" {
			t.Fatalf("embed = %+v, want the group's embed", msg.Embed)
		}
	})
}

// A forum thread-name template that fails at execution is a render error,
// like a failing content template, so the entry goes through retry/skip.
func TestFeedRendererBuild_ThreadNameRenderError(t *testing.T) {
	feed := &v1alpha1.FeedSpec{Format: tmplTitle, ForumThreadName: tmplMissingField}
	if _, err := mustBuild(t, &v1alpha1.FeedGroup{}, feed, rss.Entry{Title: "T"}); err == nil {
		t.Fatal("expected a render error from the thread name template")
	}
}
