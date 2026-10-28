package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

func TestTemplateFuncs_ReplaceSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name, format, title, author, want string
		wantError                         bool
	}{
		{name: "at limit", format: `{{replace "a" .Author .Title}}`, title: "a", author: strings.Repeat("x", maxTemplateReplaceBytes), want: strings.Repeat("x", maxTemplateReplaceBytes)},
		{name: "over limit", format: `{{replace "a" .Author .Title}}`, title: "aa", author: strings.Repeat("x", maxTemplateReplaceBytes/2+1), wantError: true},
		{name: "empty pattern counts rune boundaries", format: `{{replace "" .Author .Title}}`, title: "é", author: strings.Repeat("x", maxTemplateReplaceBytes/2), wantError: true},
		{name: "empty pattern unicode", format: `{{replace "" "x" .Title}}`, title: "é", want: "xéx"},
		{name: "shrinking replacement", format: `{{replace "xx" "x" .Title}}`, title: strings.Repeat("x", maxTemplateReplaceBytes+2), want: strings.Repeat("x", maxTemplateReplaceBytes/2+1)},
		{name: "absent pattern oversized input", format: `{{replace "absent" "x" .Title}}`, title: strings.Repeat("x", maxTemplateReplaceBytes+1), wantError: true},
		{name: "pipeline expansion", format: `{{.Title | replace "a" .Author | replace "x" .Author | truncate 10}}`, title: "a", author: strings.Repeat("x", 1025), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := compileTemplate("replace", tc.format, "")
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := renderTemplate(tmpl, rss.Entry{Title: tc.title, Author: tc.author}, maxTemplateReplaceBytes)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "replace: result exceeds maximum size") {
					t.Fatalf("expected size error, got %v", err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("result length=%d, want=%d, err=%v", len(got), len(tc.want), err)
			}
		})
	}
}

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
			tmpl, err := compileTemplate("test", tc.format, "")
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, _, err := renderTemplate(tmpl, tc.entry, maxDiscordMessageLength)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTemplateFuncs_DiscordTimeRejectsUnknownStyle(t *testing.T) {
	for _, style := range []string{"x", "", "RR", "r"} {
		tmpl, err := compileTemplate("test", `{{.PublishedUnix | discordTime "`+style+`"}}`, "")
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if _, _, err := renderTemplate(tmpl, rss.Entry{Published: time.Unix(1700000000, 0)}, maxDiscordMessageLength); err == nil {
			t.Errorf("style %q: expected a render error", style)
		}
	}
}

func TestTemplateFuncs_UnknownFunctionFailsAtCompile(t *testing.T) {
	if _, err := compileTemplate("test", `{{.Title | shout}}`, ""); err == nil {
		t.Fatal("expected a compile error for an unknown function")
	}
	// Anything that would reach the host (env, files, network) must not exist.
	for _, name := range []string{"env", "readFile", "exec", "getenv"} {
		if _, ok := templateFuncs[name]; ok {
			t.Errorf("unexpected template function %q", name)
		}
	}
}
