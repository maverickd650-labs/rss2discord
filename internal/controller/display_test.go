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
	"reflect"
	"testing"
	"time"

	"github.com/maverickd650/rss2discord-operator/internal/msgtemplate"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// TestDisplayOfSanitizesPerField pins which entry fields displayOf sanitizes,
// so the answer is readable from one place. HTML markup is used as the probe:
// a sanitized field loses it, a raw one keeps it.
// categoryOperators is a plain (markup-free) category used across these tests.
const categoryOperators = "Operators"

func TestDisplayOfSanitizesPerField(t *testing.T) {
	published := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	entry := rss.Entry{
		Title:       "<b>Title</b>",
		Description: "<p>Desc</p>",
		Link:        jsAlertURL,
		Image:       "data:text/html,<script>",
		Author:      "<b>Jane</b>",
		Categories:  []string{"<i>Go</i>", categoryOperators},
		FeedTitle:   "<em>Feed</em>",
		Content:     "<p>Body</p>",
		Published:   published,
	}
	got := displayOf(entry)

	cases := []struct {
		field     string
		got, want any
	}{
		{"Title is HTML-stripped", got.Title, "Title"},
		{"Description is HTML-stripped", got.Description, "Desc"},
		{"FeedTitle is HTML-stripped", got.FeedTitle, "Feed"},
		{"Link drops a non-http(s) scheme", got.Link, ""},
		{"Image drops a non-http(s) scheme", got.Image, ""},
		{"Published is RFC 3339 UTC", got.Published, "2026-03-04T05:06:07Z"},
		{"PublishedUnix is Unix seconds", got.PublishedUnix, published.Unix()},
		{"Author is HTML-stripped", got.Author, "Jane"},
		{"Categories are HTML-stripped", got.Categories, []string{"Go", categoryOperators}},
		{"Content is HTML-stripped on demand", got.content(), "Body"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Fatalf("got %#v, want %#v", tc.got, tc.want)
			}
		})
	}

	t.Run("http(s) link and image are kept", func(t *testing.T) {
		const link, image = "https://display.test/entry", "http://display.test/i.png"
		d := displayOf(rss.Entry{Link: link, Image: image})
		if d.Link != link || d.Image != image {
			t.Fatalf("got link %q image %q", d.Link, d.Image)
		}
	})

	t.Run("undated entry has no timestamp", func(t *testing.T) {
		d := displayOf(rss.Entry{})
		if d.Published != "" || d.PublishedUnix != 0 {
			t.Fatalf("got %q / %d, want empty / 0", d.Published, d.PublishedUnix)
		}
	})
}

// TestDisplayOfShortValues checks that stripHTML's trimming and blank-line
// collapsing behave sensibly on short single-line author and category values.
func TestDisplayOfShortValues(t *testing.T) {
	t.Run("plain values pass through unchanged", func(t *testing.T) {
		d := displayOf(rss.Entry{Author: "Jane Q. Doe", Categories: []string{"Go", "C++", "R&D", "a < b"}})
		if d.Author != "Jane Q. Doe" || !reflect.DeepEqual(d.Categories, []string{"Go", "C++", "R&D", "a < b"}) {
			t.Fatalf("got %q %q", d.Author, d.Categories)
		}
	})

	t.Run("inner spacing is kept and edges are trimmed", func(t *testing.T) {
		if got := displayOf(rss.Entry{Author: "  Jane   Doe  "}).Author; got != "Jane   Doe" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("entities decode like title", func(t *testing.T) {
		if got := displayOf(rss.Entry{Author: "Tom &amp; Jerry"}).Author; got != "Tom & Jerry" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("categories that strip to nothing are dropped", func(t *testing.T) {
		d := displayOf(rss.Entry{Categories: []string{"<br>", "<script>x</script>", "Go", "<p> </p>"}})
		if !reflect.DeepEqual(d.Categories, []string{"Go"}) {
			t.Fatalf("got %q", d.Categories)
		}
	})

	t.Run("no categories stays nil", func(t *testing.T) {
		if got := displayOf(rss.Entry{}).Categories; got != nil {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("author markup renders without tags in a template", func(t *testing.T) {
		tmpl, err := msgtemplate.Compile("t", "{{.Author}} | {{.Categories}}", "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := renderTemplate(tmpl, displayOf(rss.Entry{Author: "<b>Jane</b>", Categories: []string{"<i>Go</i>", categoryOperators}}))
		if err != nil {
			t.Fatal(err)
		}
		if want := "Jane | Go, Operators"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	// Inherited stripHTML quirk, pinned so a change is noticed: a trailing
	// "Continue reading" stub is removed from any field, so a value that is
	// only that text strips to empty.
	t.Run("continue reading stub is removed as in titles", func(t *testing.T) {
		if got := displayOf(rss.Entry{Author: "Continue reading"}).Author; got != "" {
			t.Fatalf("got %q", got)
		}
	})
}
