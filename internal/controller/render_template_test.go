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
	"slices"
	"testing"
	"time"

	"github.com/maverickd650-labs/rss2discord/internal/msgtemplate"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// renderOf compiles format and renders it over the sanitized entry.
func renderOf(t *testing.T, format string, entry rss.Entry) string {
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

type renderCase struct {
	name, format string
	entry        rss.Entry
	want         string
}

func runRenderCases(t *testing.T, cases []renderCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderOf(t, tc.format, tc.entry); got != tc.want {
				t.Fatalf("renderTemplate() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderTemplate_AuthorAndCategories(t *testing.T) {
	const format = "{{.Author}} | {{.Categories}}"
	runRenderCases(t, []renderCase{
		{name: "author and categories", format: format, entry: rss.Entry{Author: "Jane Doe", Categories: []string{"Go", "Kubernetes"}}, want: "Jane Doe | Go, Kubernetes"},
		{name: "empty author and categories render blank", format: format, want: " | "},
	})
}

// TestRenderTemplate_Published asserts a date-less entry (zero Published)
// renders an empty {{.Published}} rather than "0001-01-01T00:00:00Z", while a
// real date is still formatted as RFC 3339 UTC.
func TestRenderTemplate_Published(t *testing.T) {
	const format = "[{{.Published}}]"
	runRenderCases(t, []renderCase{
		{name: "zero published renders empty", format: format, want: "[]"},
		{name: "non-zero published unchanged", format: format, entry: rss.Entry{Published: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}, want: "[2024-05-06T07:08:09Z]"},
	})
}

func TestRenderTemplate_StripsHTMLFromTitle(t *testing.T) {
	runRenderCases(t, []renderCase{
		{name: "Title is HTML-stripped", format: tmplTitle, entry: rss.Entry{Title: "<b>Breaking</b> News &amp; Views"}, want: "Breaking News & Views"},
	})
}

// TestRenderTemplate_NewFields covers the placeholders added after the
// original six: Content, Image, FeedTitle and PublishedUnix.
func TestRenderTemplate_NewFields(t *testing.T) {
	const guarded = "{{if .PublishedUnix}}<t:{{.PublishedUnix}}:R>{{end}}"
	const unix = "<t:{{.PublishedUnix}}:R>"
	// A non-UTC zone: the Unix value is zone-independent.
	zoned := time.Date(2024, 5, 6, 9, 8, 9, 0, time.FixedZone("x", 2*3600))

	cases := []renderCase{
		{name: "Content is HTML-stripped", format: "{{.Content}}", entry: rss.Entry{Content: "<p>Full <b>body</b> &amp; more</p><script>alert(1)</script>"}, want: "Full body & more"},
		{name: "Content is distinct from Description", format: "{{.Description}}/{{.Content}}", entry: rss.Entry{Description: shortContent, Content: "long"}, want: shortContent + "/long"},
		{name: "Image passes http(s) URLs through", format: "{{.Image}}", entry: rss.Entry{Image: "https://cdn.example.com/a.jpg"}, want: "https://cdn.example.com/a.jpg"},
		{name: "FeedTitle is HTML-stripped", format: "{{.FeedTitle}}", entry: rss.Entry{FeedTitle: "<b>Example</b> &amp; Co"}, want: "Example & Co"},
		{name: "PublishedUnix is Unix seconds", format: unix, entry: rss.Entry{Published: zoned}, want: discordRelativeTS},
		{name: "zero PublishedUnix is 0", format: unix, want: "<t:0:R>"},
		{name: "zero PublishedUnix is guardable", format: guarded},
		{name: "dated PublishedUnix passes the guard", format: guarded, entry: rss.Entry{Published: time.Unix(1714979289, 0)}, want: discordRelativeTS},
		{name: "all new fields empty on a zero entry", format: "[{{.Content}}|{{.Image}}|{{.FeedTitle}}]", want: "[||]"},
	}
	runRenderCases(t, slices.Concat(cases, unsafeImageCases()))
}

// unsafeImageCases are entries whose Image has a non-http(s) scheme, which
// must render blank.
func unsafeImageCases() []renderCase {
	images := []string{"javascript:void(0)", "data:text/html,<script>x</script>", "ftp://example.com/a.jpg"}
	cases := make([]renderCase, 0, len(images))
	for _, image := range images {
		cases = append(cases, renderCase{name: "Image with unsafe scheme is blanked: " + image, format: "[{{.Image}}]", entry: rss.Entry{Image: image}, want: "[]"})
	}
	return cases
}
