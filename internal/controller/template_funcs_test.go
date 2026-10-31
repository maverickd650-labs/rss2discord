package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/msgtemplate"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

func TestTemplateFuncs(t *testing.T) {
	published := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	entry := rss.Entry{
		Title:       "  Hello World  ",
		Description: "<p>héllo wörld, a long description</p>",
		Author:      "",
		Published:   published,
	}
	cases := []struct {
		name   string
		format string
		entry  rss.Entry
		want   string
	}{
		{name: "truncate pipeline", format: "{{.Description | truncate 8}}", entry: entry, want: "héllo w…"},
		{name: "truncate shorter than limit", format: "{{.Title | trim | truncate 50}}", entry: entry, want: "Hello World"},
		{name: "default for blank", format: `{{.Author | default "anonymous"}}`, entry: entry, want: "anonymous"},
		{name: "default keeps value", format: `{{.Title | trim | default "x"}}`, entry: entry, want: "Hello World"},
		{name: "lower and upper", format: "{{.Title | trim | lower}}/{{.Title | trim | upper}}", entry: entry, want: "hello world/HELLO WORLD"},
		{name: "replace", format: `{{replace "World" "Go" .Title | trim}}`, entry: entry, want: "Hello Go"},
		{name: "discordTime", format: `{{.PublishedUnix | discordTime "R"}}`, entry: entry, want: discordRelativeTS},
		{name: "discordTime undated renders empty", format: `[{{.PublishedUnix | discordTime "R"}}]`, entry: rss.Entry{}, want: "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := msgtemplate.Compile("test", tc.format, "")
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := renderTemplate(tmpl, displayOf(tc.entry))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFeedRendererBuild_OutputLimitAppliesToEveryTemplate(t *testing.T) {
	for _, tc := range []struct {
		name string
		feed *v1alpha1.FeedSpec
	}{
		{name: "message", feed: &v1alpha1.FeedSpec{Format: authorField}},
		{name: "embed description", feed: &v1alpha1.FeedSpec{Embed: &v1alpha1.EmbedSpec{Enabled: true, DescriptionFormat: authorField}}},
		{name: "forum thread name", feed: &v1alpha1.FeedSpec{Format: "safe", ForumThreadName: authorField}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := rss.Entry{Author: strings.Repeat("x", (1<<20)+1)}
			msg, err := mustBuild(t, &v1alpha1.FeedGroup{}, tc.feed, entry)
			if err == nil || !strings.Contains(err.Error(), "template output exceeds") {
				t.Fatalf("expected output-limit error, got %v", err)
			}
			if msg.Content != "" || msg.Embed != nil || msg.ThreadName != "" {
				t.Fatalf("render failure returned a partial message: %+v", msg)
			}
		})
	}
}

func TestRenderTemplate_LinkSchemeFiltered(t *testing.T) {
	tmpl, err := msgtemplate.Compile("t", `[{{.Link}}]`, "")
	if err != nil {
		t.Fatal(err)
	}
	for link, want := range map[string]string{
		"https://example.com/b":         "[https://example.com/b]",
		"javascript:alert(1)":           "[]",
		"data:text/html,x":              "[]",
		"http://example.com/a?q=1#part": "[http://example.com/a?q=1#part]",
		"HTTPS://example.com/a":         "[HTTPS://example.com/a]",
		"JaVaScRiPt:alert(1)":           "[]",
		"file:///etc/passwd":            "[]",
		"ftp://example.com/a":           "[]",
		"mailto:alice@example.com":      "[]",
		"/relative/path":                "[]",
		"//example.com/a":               "[]",
		"https://example.com/%zz":       "[]",
		"https://example.com/\npath":    "[]",
		" https://example.com/a":        "[]",
		"":                              "[]",
	} {
		t.Run(link, func(t *testing.T) {
			got, err := renderTemplate(tmpl, displayOf(rss.Entry{Link: link}))
			if err != nil || got != want {
				t.Fatalf("link %q rendered %q, err=%v; want %q", link, got, err, want)
			}
		})
	}
}

func TestRenderTemplate_FilteredLinkIsEmptyInConditionalsAndHelpers(t *testing.T) {
	tmpl, err := msgtemplate.Compile("link", `{{if .Link}}linked{{else}}unlinked{{end}}/{{$.Link | default "none"}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"javascript:alert(1)", "data:text/html,x", "//example.com/a"} {
		t.Run(link, func(t *testing.T) {
			got, err := renderTemplate(tmpl, displayOf(rss.Entry{Link: link}))
			if err != nil || got != "unlinked/none" {
				t.Fatalf("filtered link rendered %q, err=%v; want unlinked/none", got, err)
			}
		})
	}
}

// authorField is a template that prints only the entry author.
var authorField = "{{" + ".Author}}"
