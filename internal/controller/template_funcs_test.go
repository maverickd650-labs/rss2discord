package controller

import (
	"fmt"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
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

// notSupportedErrText is the substring every rejected-template error carries.
const notSupportedErrText = "not supported"

// authorField is a template that prints only the entry author.
var authorField = "{{" + ".Author}}"

func TestCompileTemplate_RejectsUnboundedConstructs(t *testing.T) {
	for name, format := range map[string]string{
		"range over int":        `{{range 100000000000}}{{end}}`,
		"range inside if":       `{{if .Title}}{{range 20000000}}aaaaaaaaaa{{end}}{{end}}`,
		"range inside else":     `{{if .Title}}x{{else}}{{range 5}}y{{end}}{{end}}`,
		"with":                  `{{with .Title}}{{.}}{{end}}`,
		"variable assignment":   `{{$x := .Title}}{{$x = printf "%s%s" $x $x}}`,
		"variable declaration":  `{{$x := .Title}}{{$x}}`,
		"printf":                `{{printf "%s" .Title}}`,
		"print in pipeline":     `{{.Title | print}}`,
		"printf in parens":      `{{if eq (printf "a") "a"}}x{{end}}`,
		"printf in if pipe":     `{{if printf "%s" .Title}}x{{end}}`,
		"html":                  `{{html .Title}}`,
		"js":                    `{{js .Title}}`,
		"urlquery":              `{{urlquery .Title}}`,
		"call":                  `{{call .Title}}`,
		"template invocation":   `{{template "x"}}`,
		"define":                `{{define "x"}}y{{end}}z`,
		"recursive define blow": `{{define "a"}}{{template "a"}}{{template "a"}}{{end}}{{template "a"}}`,
		"same-name recursion":   `{{define "t"}}{{template "t"}}{{end}}`,
		"root assignment":       `{{$ = .Title}}`,
		"if declaration":        `{{if $x := .Title}}{{$x}}{{end}}`,
		"if assignment":         `{{if $ = .Title}}yes{{end}}`,
		"nested with":           `{{if .Title}}{{if .Author}}{{with .Link}}{{.}}{{end}}{{end}}{{end}}`,
		"with inside else":      `{{if .Title}}yes{{else}}{{with .Author}}{{.}}{{end}}{{end}}`,
		"range in else if":      `{{if .Title}}yes{{else if .Author}}{{range 2}}no{{end}}{{end}}`,
		"template inside else":  `{{if .Title}}yes{{else}}{{template "missing" .}}{{end}}`,
		"empty definition":      `{{define "unused"}}{{end}}safe`,
		"block definition":      `{{block "part" .}}safe{{end}}`,
		"println":               `{{println .Title}}`,
	} {
		t.Run(name, func(t *testing.T) {
			// A syntax error must not masquerade as rejection by the safety check.
			if _, err := template.New("t").Funcs(templateFuncs).Parse(format); err != nil {
				t.Fatalf("invalid test template: %v", err)
			}
			if tmpl, err := compileTemplate("t", format, ""); tmpl != nil || err == nil || !strings.Contains(err.Error(), notSupportedErrText) {
				t.Fatalf("expected a 'not supported' error, got %v", err)
			}
		})
	}
	// if/else and helpers must keep working.
	tmpl, err := compileTemplate("t", `{{if .Title}}{{.Title}}{{else}}none{{end}}{{if .Author}}/{{.Author}}{{end}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := renderTemplate(tmpl, rss.Entry{Title: "a", Author: "b"}, maxDiscordMessageLength)
	if err != nil || got != "a/b" {
		t.Fatalf("got %q, err=%v", got, err)
	}
}

func TestCompileTemplate_RejectsGrowthBuiltinsInNestedPipelines(t *testing.T) {
	for _, action := range []string{
		`print .Title`, `println .Title`, `printf "%s" .Title`,
		`html .Title`, `js .Title`, `urlquery .Title`, `call .Title`,
	} {
		for name, wrapper := range map[string]string{
			"nested argument": `{{.Title | default (%s)}}`,
			"if predicate":    `{{if eq (%s) "unused"}}yes{{end}}`,
			"chain operand":   `{{(%s).Title}}`,
		} {
			t.Run(action+"/"+name, func(t *testing.T) {
				format := fmt.Sprintf(wrapper, action)
				if _, err := template.New("nested").Funcs(templateFuncs).Parse(format); err != nil {
					t.Fatalf("invalid test template: %v", err)
				}
				tmpl, err := compileTemplate("nested", format, "")
				if tmpl != nil || err == nil || !strings.Contains(err.Error(), notSupportedErrText) {
					t.Fatalf("unsafe nested action compiled: template=%v, err=%v", tmpl, err)
				}
			})
		}
	}
}

func TestCompileTemplate_FallbackRestrictions(t *testing.T) {
	for _, fallback := range []string{
		`{{range 2}}x{{end}}`, `{{with .Title}}{{.}}{{end}}`,
		`{{$ = .Title}}`, `{{printf "%s" .Title}}`,
		`{{define "unused"}}x{{end}}`, `{{template "missing"}}`,
	} {
		t.Run(fallback, func(t *testing.T) {
			tmpl, err := compileTemplate("fallback", " \t\n", fallback)
			if tmpl != nil || err == nil || !strings.Contains(err.Error(), notSupportedErrText) {
				t.Fatalf("unsafe fallback compiled: template=%v, err=%v", tmpl, err)
			}
		})
	}
}

func TestCompileTemplate_SupportedConstructs(t *testing.T) {
	for _, tc := range []struct {
		name, format, fallback, want string
		entry                        rss.Entry
	}{
		{name: "if true", format: `{{if .Title}}{{$.Title | upper}}{{end}}`, entry: rss.Entry{Title: "title"}, want: "TITLE"},
		{name: "if false without else", format: `{{if .Title}}unused{{end}}`},
		{name: "else if", format: `{{if .Title}}title{{else if .Author}}{{.Author}}{{else}}none{{end}}`, entry: rss.Entry{Author: "author"}, want: "author"},
		{name: "else", format: `{{if .Title}}title{{else if .Author}}author{{else}}none{{end}}`, want: "none"},
		{name: "nested helpers", format: `{{if eq (lower .Author) "alice"}}{{.Author | upper | truncate 3}}{{end}}`, entry: rss.Entry{Author: "Alice"}, want: "AL…"},
		{name: "root field in parentheses", format: `{{($.Title) | lower}}`, entry: rss.Entry{Title: "TITLE"}, want: "title"},
		{name: "literal restricted words", format: `range define template with printf {{"print"}}`, want: "range define template with printf print"},
		{name: "comment only", format: `{{/* {{range 100000000000}}{{end}} */}}`},
		{name: "empty template"},
		{name: "safe fallback", format: " \t\n", fallback: `{{if .Title}}{{.Title}}{{else}}none{{end}}`, want: "none"},
		{name: "unused unsafe fallback", format: `{{ .Title }}`, fallback: `{{range 2}}{{end}}`, entry: rss.Entry{Title: "safe"}, want: "safe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := compileTemplate("supported", tc.format, tc.fallback)
			if err != nil {
				t.Fatal(err)
			}
			got, overflow, err := renderTemplate(tmpl, tc.entry, maxDiscordMessageLength)
			if err != nil || got != tc.want || overflow != 0 {
				t.Fatalf("render = %q, overflow=%d, err=%v; want %q without overflow", got, overflow, err, tc.want)
			}
		})
	}
}

func TestRenderTemplate_OutputByteBoundaries(t *testing.T) {
	const limit = 1 << 20
	truncated8 := strings.Repeat("x", 7) + "…" // an 8-rune cut of a run of x
	for _, tc := range []struct {
		name, format, author, want string
		wantOverflow               int
		wantError                  bool
	}{
		{name: "one byte below", format: authorField, author: strings.Repeat("x", limit-1), want: truncated8, wantOverflow: limit - 1 - 8},
		{name: "exactly one MiB", format: authorField, author: strings.Repeat("x", limit), want: truncated8, wantOverflow: limit - 8},
		{name: "one byte above", format: authorField, author: strings.Repeat("x", limit+1), wantError: true},
		{name: "cumulative exact limit", format: `{{.Author}}{{.Author}}`, author: strings.Repeat("x", limit/2), want: truncated8, wantOverflow: limit - 8},
		{name: "literal crosses cumulative limit", format: `{{.Author}}{{.Author}}x`, author: strings.Repeat("x", limit/2), wantError: true},
		{name: "multibyte exact limit", format: authorField, author: strings.Repeat("é", limit/2), want: "ééééééé…", wantOverflow: limit/2 - 8},
		{name: "bytes exceed limit but runes do not", format: `{{.Author}}x`, author: strings.Repeat("é", limit/2), wantError: true},
		{name: "oversized literal", format: strings.Repeat("x", limit+1), wantError: true},
		{name: "unexecuted text does not count", format: `{{if false}}` + strings.Repeat("x", limit+1) + `{{end}}ok`, want: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl, err := compileTemplate("limit", tc.format, "")
			if err != nil {
				t.Fatal(err)
			}
			// The byte cap applies before Discord's much smaller rune limit.
			got, overflow, err := renderTemplate(tmpl, rss.Entry{Author: tc.author}, 8)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "template output exceeds") || got != "" || overflow != 0 {
					t.Fatalf("render returned %d bytes, overflow=%d, err=%v; want output-limit error and no partial output", len(got), overflow, err)
				}
				return
			}
			if err != nil || got != tc.want || overflow != tc.wantOverflow {
				t.Fatalf("render = %q, overflow=%d, err=%v; want %q, overflow=%d", got, overflow, err, tc.want, tc.wantOverflow)
			}
		})
	}
}

func TestBuildDiscordMessage_OutputLimitAppliesToEveryTemplate(t *testing.T) {
	safe, err := compileTemplate("safe", "safe", "")
	if err != nil {
		t.Fatal(err)
	}
	oversized, err := compileTemplate("oversized", authorField, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                         string
		embed                        *v1alpha1.EmbedSpec
		content, description, thread *template.Template
	}{
		{name: "message", content: oversized},
		{name: "embed description", embed: &v1alpha1.EmbedSpec{Enabled: true}, description: oversized},
		{name: "forum thread name", content: safe, thread: oversized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := rss.Entry{Author: strings.Repeat("x", (1<<20)+1)}
			msg, _, err := buildDiscordMessage(&v1alpha1.FeedGroup{}, tc.embed, tc.content, tc.description, tc.thread, &v1alpha1.FeedSpec{}, entry)
			if err == nil || !strings.Contains(err.Error(), "template output exceeds") {
				t.Fatalf("expected output-limit error, got %v", err)
			}
			if msg.Content != "" || msg.Embed != nil || msg.ThreadName != "" {
				t.Fatalf("render failure returned a partial message: %+v", msg)
			}
		})
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
			got, overflow, err := renderTemplate(tmpl, rss.Entry{Link: link}, maxDiscordMessageLength)
			if err != nil || got != want || overflow != 0 {
				t.Fatalf("link %q rendered %q, overflow=%d, err=%v; want %q without overflow", link, got, overflow, err, want)
			}
		})
	}
}

func TestRenderTemplate_FilteredLinkIsEmptyInConditionalsAndHelpers(t *testing.T) {
	tmpl, err := compileTemplate("link", `{{if .Link}}linked{{else}}unlinked{{end}}/{{$.Link | default "none"}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"javascript:alert(1)", "data:text/html,x", "//example.com/a"} {
		t.Run(link, func(t *testing.T) {
			got, overflow, err := renderTemplate(tmpl, rss.Entry{Link: link}, maxDiscordMessageLength)
			if err != nil || got != "unlinked/none" || overflow != 0 {
				t.Fatalf("filtered link rendered %q, overflow=%d, err=%v; want unlinked/none without overflow", got, overflow, err)
			}
		})
	}
}
