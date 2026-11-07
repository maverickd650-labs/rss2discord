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

package msgtemplate

import (
	"fmt"
	"strings"
	"testing"
	"text/template"
	"text/template/parse"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
)

// entry stands in for the controller's template data: the fields the tests'
// templates reference.
type entry struct {
	Title, Description, Author, Link string
	PublishedUnix                    int64
}

// tmplSafeText is arbitrary text a template can interpolate.
const tmplSafeText = "safe"

// authorField is a template that prints only the entry author.
var authorField = "{{" + ".Author}}"

// notSupportedErrText is the substring every rejected-template error carries.
const notSupportedErrText = "not supported"

// outputLimit is the template output cap (1 MiB) the boundary tests probe.
const outputLimit = 1 << 20

// mustCompile compiles a template that the test expects to be accepted.
func mustCompile(t *testing.T, name, format, fallback string) *Template {
	t.Helper()
	tmpl, err := Compile(name, format, fallback)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// assertRenders fails unless tmpl renders data to exactly want.
func assertRenders(t *testing.T, tmpl *Template, data any, want string) {
	t.Helper()
	got, err := tmpl.Execute(data)
	if err != nil {
		t.Fatalf("Execute: %v; want %.80q (len %d)", err, want, len(want))
	}
	if got != want {
		t.Fatalf("Execute = %.80q (len %d); want %.80q (len %d)", got, len(got), want, len(want))
	}
}

// failsWith reports whether err is non-nil and mentions substr.
func failsWith(err error, substr string) bool {
	return err != nil && strings.Contains(err.Error(), substr)
}

// assertFailsWith fails unless rendering data errors with substr and yields
// no partial output, which must never become a sendable message.
func assertFailsWith(t *testing.T, tmpl *Template, data any, substr string) {
	t.Helper()
	got, err := tmpl.Execute(data)
	if !failsWith(err, substr) {
		t.Fatalf("Execute err = %v; want an error containing %q", err, substr)
	}
	if got != "" {
		t.Fatalf("Execute returned %d bytes of partial output alongside the error", len(got))
	}
}

// assertParses fails if format is not valid text/template syntax, so a
// syntax error cannot masquerade as rejection by the safety check.
func assertParses(t *testing.T, format string) {
	t.Helper()
	if _, err := template.New("t").Funcs(templateFuncs).Parse(format); err != nil {
		t.Fatalf("invalid test template: %v", err)
	}
}

// assertNotSupported fails unless Compile rejects format (or an unsafe
// fallback) with the "not supported" error and returns no template.
func assertNotSupported(t *testing.T, format, fallback string) {
	t.Helper()
	tmpl, err := Compile("rejected", format, fallback)
	if tmpl != nil {
		t.Fatalf("unsafe template compiled: %v", tmpl)
	}
	if !failsWith(err, notSupportedErrText) {
		t.Fatalf("expected a %q error, got %v", notSupportedErrText, err)
	}
}

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
			assertRenders(t, mustCompile(t, "boundary", tc.format, ""), entry{Author: tc.author}, tc.want)
		})
	}
}

func TestTemplateFuncs_DiscordTimeStyles(t *testing.T) {
	for _, style := range []string{"t", "T", "d", "D", "f", "F", "R"} {
		t.Run(style, func(t *testing.T) {
			tmpl := mustCompile(t, "timestamp", fmt.Sprintf(`{{.PublishedUnix | discordTime %q}}`, style), "")
			for _, unix := range []int64{-1, 1714979289} {
				assertRenders(t, tmpl, entry{PublishedUnix: unix}, fmt.Sprintf("<t:%d:%s>", unix, style))
			}
			assertRenders(t, tmpl, entry{}, "") // undated renders empty
		})
	}
}

// Style validation must also run for undated entries. Partial output from a
// failed template must never become a sendable message.
func TestTemplateFuncs_DiscordTimeValidatesStyleForUndatedEntries(t *testing.T) {
	tmpl := mustCompile(t, "invalid-style", `prefix{{.PublishedUnix | discordTime ""}}`, "")
	assertFailsWith(t, tmpl, entry{}, "discordTime: unknown style")
}

func TestTemplateFuncs_ReplaceSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name, format, title, author, want string
	}{
		{name: "at limit", format: `{{replace "a" .Author .Title}}`, title: "a", author: strings.Repeat("x", maxTemplateReplaceBytes), want: strings.Repeat("x", maxTemplateReplaceBytes)},
		{name: "empty pattern unicode", format: `{{replace "" "x" .Title}}`, title: "é", want: "xéx"},
		{name: "shrinking replacement", format: `{{replace "xx" "x" .Title}}`, title: strings.Repeat("x", maxTemplateReplaceBytes+2), want: strings.Repeat("x", maxTemplateReplaceBytes/2+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustCompile(t, "replace", tc.format, "")
			assertRenders(t, tmpl, entry{Title: tc.title, Author: tc.author}, tc.want)
		})
	}
}

func TestTemplateFuncs_ReplaceRejectsOversizedResult(t *testing.T) {
	for _, tc := range []struct {
		name, format, title, author string
	}{
		{name: "over limit", format: `{{replace "a" .Author .Title}}`, title: "aa", author: strings.Repeat("x", maxTemplateReplaceBytes/2+1)},
		{name: "empty pattern counts rune boundaries", format: `{{replace "" .Author .Title}}`, title: "é", author: strings.Repeat("x", maxTemplateReplaceBytes/2)},
		{name: "absent pattern oversized input", format: `{{replace "absent" "x" .Title}}`, title: strings.Repeat("x", maxTemplateReplaceBytes+1)},
		{name: "pipeline expansion", format: `{{.Title | replace "a" .Author | replace "x" .Author | truncate 10}}`, title: "a", author: strings.Repeat("x", 1025)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustCompile(t, "replace", tc.format, "")
			assertFailsWith(t, tmpl, entry{Title: tc.title, Author: tc.author}, "replace: result exceeds maximum size")
		})
	}
}

func TestTemplateFuncs_DiscordTimeRejectsUnknownStyle(t *testing.T) {
	for _, style := range []string{"x", "", "RR", "r"} {
		t.Run(fmt.Sprintf("style %q", style), func(t *testing.T) {
			tmpl := mustCompile(t, "test", `{{.PublishedUnix | discordTime "`+style+`"}}`, "")
			assertFailsWith(t, tmpl, entry{PublishedUnix: 1700000000}, "discordTime: unknown style")
		})
	}
}

func TestTemplateFuncs_UnknownFunctionFailsAtCompile(t *testing.T) {
	if _, err := Compile("test", `{{.Title | shout}}`, ""); err == nil {
		t.Fatal("expected a compile error for an unknown function")
	}
	// Anything that would reach the host (env, files, network) must not exist.
	for _, name := range []string{"env", "readFile", "exec", "getenv"} {
		if _, ok := templateFuncs[name]; ok {
			t.Errorf("unexpected template function %q", name)
		}
	}
}

func TestCompile_RejectsUnboundedConstructs(t *testing.T) {
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
			assertParses(t, format)
			assertNotSupported(t, format, "")
		})
	}
}

// if/else and helpers must keep working alongside the rejections.
func TestCompile_AcceptsIfElseAndHelpers(t *testing.T) {
	tmpl := mustCompile(t, "t", `{{if .Title}}{{.Title}}{{else}}none{{end}}{{if .Author}}/{{.Author}}{{end}}`, "")
	assertRenders(t, tmpl, entry{Title: "a", Author: "b"}, "a/b")
}

func TestCompile_RejectsGrowthBuiltinsInNestedPipelines(t *testing.T) {
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
				assertParses(t, format)
				assertNotSupported(t, format, "")
			})
		}
	}
}

func TestCompile_FallbackRestrictions(t *testing.T) {
	for _, fallback := range []string{
		`{{range 2}}x{{end}}`, `{{with .Title}}{{.}}{{end}}`,
		`{{$ = .Title}}`, `{{printf "%s" .Title}}`,
		`{{define "unused"}}x{{end}}`, `{{template "missing"}}`,
	} {
		t.Run(fallback, func(t *testing.T) {
			assertNotSupported(t, " \t\n", fallback)
		})
	}
}

func TestCompile_SupportedConstructs(t *testing.T) {
	for _, tc := range []struct {
		name, format, fallback, want string
		entry                        entry
	}{
		{name: "if true", format: `{{if .Title}}{{$.Title | upper}}{{end}}`, entry: entry{Title: "title"}, want: "TITLE"},
		{name: "if false without else", format: `{{if .Title}}unused{{end}}`},
		{name: "else if", format: `{{if .Title}}title{{else if .Author}}{{.Author}}{{else}}none{{end}}`, entry: entry{Author: "author"}, want: "author"},
		{name: "else", format: `{{if .Title}}title{{else if .Author}}author{{else}}none{{end}}`, want: "none"},
		{name: "nested helpers", format: `{{if eq (lower .Author) "alice"}}{{.Author | upper | truncate 3}}{{end}}`, entry: entry{Author: "Alice"}, want: "AL…"},
		{name: "root field in parentheses", format: `{{($.Title) | lower}}`, entry: entry{Title: "TITLE"}, want: "title"},
		{name: "literal restricted words", format: `range define template with printf {{"print"}}`, want: "range define template with printf print"},
		{name: "comment only", format: `{{/* {{range 100000000000}}{{end}} */}}`},
		{name: "empty template"},
		{name: "safe fallback", format: " \t\n", fallback: `{{if .Title}}{{.Title}}{{else}}none{{end}}`, want: "none"},
		{name: "unused unsafe fallback", format: `{{ .Title }}`, fallback: `{{range 2}}{{end}}`, entry: entry{Title: tmplSafeText}, want: tmplSafeText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRenders(t, mustCompile(t, "supported", tc.format, tc.fallback), tc.entry, tc.want)
		})
	}
}

func TestExecute_OutputByteBoundaries(t *testing.T) {
	const limit = outputLimit
	truncated8 := strings.Repeat("x", 7) + "…" // an 8-rune cut of a run of x
	for _, tc := range []struct {
		name, format, author, want string
		wantOverflow               int
	}{
		{name: "one byte below", format: authorField, author: strings.Repeat("x", limit-1), want: truncated8, wantOverflow: limit - 1 - 8},
		{name: "exactly one MiB", format: authorField, author: strings.Repeat("x", limit), want: truncated8, wantOverflow: limit - 8},
		{name: "cumulative exact limit", format: `{{.Author}}{{.Author}}`, author: strings.Repeat("x", limit/2), want: truncated8, wantOverflow: limit - 8},
		{name: "multibyte exact limit", format: authorField, author: strings.Repeat("é", limit/2), want: "ééééééé…", wantOverflow: limit/2 - 8},
		{name: "unexecuted text does not count", format: `{{if false}}` + strings.Repeat("x", limit+1) + `{{end}}ok`, want: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := mustCompile(t, "limit", tc.format, "").Execute(entry{Author: tc.author})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			// The byte cap applies before any rune limit (here a stand-in
			// 8-rune clamp applied to the raw output).
			got, overflow := discord.Truncate(raw, 8)
			if got != tc.want {
				t.Fatalf("render = %q; want %q", got, tc.want)
			}
			if overflow != tc.wantOverflow {
				t.Fatalf("overflow = %d; want %d", overflow, tc.wantOverflow)
			}
		})
	}
}

func TestExecute_OutputByteBoundariesExceeded(t *testing.T) {
	const limit = outputLimit
	for _, tc := range []struct {
		name, format, author string
	}{
		{name: "one byte above", format: authorField, author: strings.Repeat("x", limit+1)},
		{name: "literal crosses cumulative limit", format: `{{.Author}}{{.Author}}x`, author: strings.Repeat("x", limit/2)},
		{name: "bytes exceed limit but runes do not", format: `{{.Author}}x`, author: strings.Repeat("é", limit/2)},
		{name: "oversized literal", format: strings.Repeat("x", limit+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := mustCompile(t, "limit", tc.format, "")
			assertFailsWith(t, tmpl, entry{Author: tc.author}, "template output exceeds")
		})
	}
}

func TestExecute_OutputLimit(t *testing.T) {
	// A single large value passes through untruncated by the helpers, so only
	// the output cap stops it.
	tmpl := mustCompile(t, "t", `{{.Title}}{{.Title}}`, "")
	assertFailsWith(t, tmpl, entry{Title: strings.Repeat("x", maxTemplateOutputBytes/2+1)}, "template output exceeds")
}

func TestRejectUnboundedConstructs_UnparsedTemplateIsAccepted(t *testing.T) {
	// A template that was never parsed has a nil Tree and nothing to reject.
	tmpl := template.New("unparsed")
	if tmpl.Tree != nil {
		t.Fatal("precondition: expected a nil Tree")
	}
	if err := rejectUnboundedConstructs(tmpl); err != nil {
		t.Fatalf("rejectUnboundedConstructs = %v, want nil", err)
	}
}

func TestRejectUnboundedConstructs_RejectsDefinedTemplates(t *testing.T) {
	tmpl, err := template.New("main").Parse(`{{define "x"}}x{{end}}main`)
	if err != nil {
		t.Fatal(err)
	}
	err = rejectUnboundedConstructs(tmpl)
	if err == nil || !strings.Contains(err.Error(), "{{define}} is not supported") {
		t.Fatalf("rejectUnboundedConstructs = %v, want the define rejection", err)
	}
}

// The parser never produces a nil *parse.PipeNode (every action and branch
// has a pipeline), but checkNode guards against one like it does for a nil
// *parse.ListNode, which an {{if}} without {{else}} does produce.
func TestCheckNode_NilNodesAreAccepted(t *testing.T) {
	for name, node := range map[string]parse.Node{
		"nil pipe": (*parse.PipeNode)(nil),
		"nil list": (*parse.ListNode)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkNode(node); err != nil {
				t.Fatalf("checkNode = %v, want nil", err)
			}
		})
	}
}
