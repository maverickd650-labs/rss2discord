package controller

import (
	"testing"
	"time"

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
