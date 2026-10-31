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

	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// TestDisplayOfSanitizesPerField pins which entry fields displayOf sanitizes,
// so the answer is readable from one place. HTML markup is used as the probe:
// a sanitized field loses it, a raw one keeps it.
func TestDisplayOfSanitizesPerField(t *testing.T) {
	published := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	entry := rss.Entry{
		Title:       "<b>Title</b>",
		Description: "<p>Desc</p>",
		Link:        jsAlertURL,
		Image:       "data:text/html,<script>",
		Author:      "<b>Jane</b>",
		Categories:  []string{"<i>Go</i>", "K8s"},
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
		{"Author is raw", got.Author, "<b>Jane</b>"},
		{"Categories are raw", got.Categories, []string{"<i>Go</i>", "K8s"}},
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
