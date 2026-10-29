package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

func TestTemplateFuncs_StringBoundaries(t *testing.T) {
	const japaneseText = "日本語"
	for _, tc := range []struct {
		name, format, author, want string
	}{
		{name: "negative truncate", format: `{{.Author | truncate -1}}`, author: japaneseText},
		{name: "zero truncate", format: `{{.Author | truncate 0}}`, author: japaneseText},
		{name: "one rune", format: `{{.Author | truncate 1}}`, author: japaneseText, want: "日"},
		{name: "ellipsis counts as one rune", format: `{{.Author | truncate 2}}`, author: japaneseText, want: "日…"},
		{name: "exact rune limit", format: `{{.Author | truncate 3}}`, author: japaneseText, want: japaneseText},
		{name: "empty truncate", format: `{{.Author | truncate 1}}`},
		{name: "unicode blank default", format: `{{.Author | default "unknown"}}`, author: "\t\n\u00a0\u2003", want: "unknown"},
		{name: "default preserves nonblank whitespace", format: `{{.Author | default "unknown"}}`, author: "  Alice  ", want: "  Alice  "},
		{name: "unicode lower", format: `{{.Author | lower}}`, author: "ÉCOLE 日本語", want: "école 日本語"},
		{name: "unicode upper", format: `{{.Author | upper}}`, author: "école 日本語", want: "ÉCOLE 日本語"},
		{name: "unicode trim", format: `{{.Author | trim}}`, author: "\u2003Alice\u00a0", want: "Alice"},
		{name: "replace all matches", format: `{{.Author | replace "猫" "犬"}}`, author: "猫と猫", want: "犬と犬"},
		{name: "replace deletes matches", format: `{{.Author | replace "猫" ""}}`, author: "猫と猫", want: "と"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := compileTemplate("boundary", tc.format, "")
			if err != nil {
				t.Fatal(err)
			}
			got, overflow, err := renderTemplate(tmpl, rss.Entry{Author: tc.author}, maxDiscordMessageLength)
			if err != nil || got != tc.want || overflow != 0 {
				t.Fatalf("renderTemplate = %q, overflow=%d, err=%v; want %q without overflow", got, overflow, err, tc.want)
			}
		})
	}
}

func TestTemplateFuncs_DiscordTimeStylesAndUndatedValidation(t *testing.T) {
	for _, style := range []string{"t", "T", "d", "D", "f", "F", "R"} {
		t.Run(style, func(t *testing.T) {
			tmpl, err := compileTemplate("timestamp", fmt.Sprintf(`{{.PublishedUnix | discordTime %q}}`, style), "")
			if err != nil {
				t.Fatal(err)
			}
			for _, unix := range []int64{-1, 1714979289} {
				got, _, err := renderTemplate(tmpl, rss.Entry{Published: time.Unix(unix, 0)}, maxDiscordMessageLength)
				if want := fmt.Sprintf("<t:%d:%s>", unix, style); err != nil || got != want {
					t.Fatalf("timestamp = %q, err=%v; want %q", got, err, want)
				}
			}
			if got, _, err := renderTemplate(tmpl, rss.Entry{}, maxDiscordMessageLength); err != nil || got != "" {
				t.Fatalf("undated timestamp = %q, err=%v; want empty", got, err)
			}
		})
	}
	// Style validation must also run for undated entries. Partial output
	// from a failed template must never become a sendable message.
	tmpl, err := compileTemplate("invalid-style", `prefix{{.PublishedUnix | discordTime ""}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	got, overflow, err := renderTemplate(tmpl, rss.Entry{}, maxDiscordMessageLength)
	if err == nil || !strings.Contains(err.Error(), "discordTime: unknown style") || got != "" || overflow != 0 {
		t.Fatalf("invalid undated timestamp = %q, overflow=%d, err=%v", got, overflow, err)
	}
}

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

func TestCompileTemplate_RejectsUnboundedConstructs(t *testing.T) {
	for name, format := range map[string]string{
		"range over int":        `{{range 100000000000}}{{end}}`,
		"range inside if":       `{{if .Title}}{{range 20000000}}aaaaaaaaaa{{end}}{{end}}`,
		"range inside else":     `{{if .Title}}x{{else}}{{range 5}}y{{end}}{{end}}`,
		"range inside with":     `{{with .Title}}{{range 5}}y{{end}}{{end}}`,
		"template invocation":   `{{template "x"}}`,
		"define":                `{{define "x"}}y{{end}}z`,
		"recursive define blow": `{{define "a"}}{{template "a"}}{{template "a"}}{{end}}{{template "a"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := compileTemplate("t", format, ""); err == nil || !strings.Contains(err.Error(), "not supported") {
				t.Fatalf("expected a 'not supported' error, got %v", err)
			}
		})
	}
	// if/with/else and helpers must keep working.
	tmpl, err := compileTemplate("t", `{{if .Title}}{{.Title}}{{else}}none{{end}}{{with .Author}}/{{.}}{{end}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := renderTemplate(tmpl, rss.Entry{Title: "a", Author: "b"}, maxDiscordMessageLength)
	if err != nil || got != "a/b" {
		t.Fatalf("got %q, err=%v", got, err)
	}
}

func TestRenderTemplate_OutputLimit(t *testing.T) {
	// A single large value passes through untruncated by the helpers, so only
	// the output cap stops it.
	tmpl, err := compileTemplate("t", `{{.Title}}{{.Title}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = renderTemplate(tmpl, rss.Entry{Title: strings.Repeat("x", maxTemplateOutputBytes/2+1)}, maxDiscordMessageLength)
	if err == nil || !strings.Contains(err.Error(), "template output exceeds") {
		t.Fatalf("expected output limit error, got %v", err)
	}
}

func TestRenderTemplate_LinkSchemeFiltered(t *testing.T) {
	tmpl, err := compileTemplate("t", `[{{.Link}}]`, "")
	if err != nil {
		t.Fatal(err)
	}
	for link, want := range map[string]string{
		"https://example.com/a": "[https://example.com/a]",
		"javascript:alert(1)":   "[]",
		"data:text/html,x":      "[]",
	} {
		got, _, err := renderTemplate(tmpl, rss.Entry{Link: link}, maxDiscordMessageLength)
		if err != nil || got != want {
			t.Fatalf("link %q rendered %q (err=%v), want %q", link, got, err, want)
		}
	}
}
