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
	"strings"
	"sync"
	"time"

	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// displayEntry is an rss.Entry as it is shown to Discord: every presentation
// field sanitized exactly once, by displayOf. Both the embed (feedRenderer.build)
// and message templates read from it directly and never sanitize raw entry fields
// themselves, so what is and isn't sanitized is readable here.
//
// Sanitization per field:
//   - Title, Description, FeedTitle: stripHTML.
//   - Link, Image: httpURLOrEmpty (http/https only).
//   - Published, PublishedUnix: formatted by formatPublished/publishedUnix ("" and 0 when undated).
//   - Author: stripHTML.
//   - Categories: stripHTML each, drop blanks and join with ", ".
//   - Content: stripHTML, on demand (see Content).
//
// Its exported fields and methods are exactly the template placeholders
// documented on FeedGroupSpec.Format. Adding an exported member changes that
// public template interface and must be reflected in the documented contract.
type displayEntry struct {
	Title       string
	Description string
	Link        string
	Image       string
	Published   string
	// PublishedUnix is Published as Unix seconds for Discord's <t:UNIX:R>
	// relative timestamps; it is 0 when the entry is undated.
	PublishedUnix int64
	Author        string
	Categories    string
	FeedTitle     string

	// content is the lazily stripped body. It is a pointer so that the copies
	// of a displayEntry (it is passed by value to templates) share one cache;
	// nil, as in a zero displayEntry, means an empty body.
	content *lazyContent
}

// lazyContent strips a raw entry body at most once, on first use.
type lazyContent struct {
	raw      string
	once     sync.Once
	stripped string
}

func (c *lazyContent) get() string {
	c.once.Do(func() { c.stripped = stripHTML(c.raw) })
	return c.stripped
}

// displayOf sanitizes entry's presentation fields. The body is the exception:
// it is kept raw and stripped by Content, because the strip -- over a body that
// can run to tens of KB -- should only happen for templates that actually
// reference {{.Content}}.
func displayOf(entry rss.Entry) displayEntry {
	return displayEntry{
		Title:         stripHTML(entry.Title),
		Description:   stripHTML(entry.Description),
		Link:          httpURLOrEmpty(entry.Link),
		Image:         httpURLOrEmpty(entry.Image),
		Published:     formatPublished(entry.Published),
		PublishedUnix: publishedUnix(entry.Published),
		Author:        stripHTML(entry.Author),
		Categories:    strings.Join(stripCategories(entry.Categories), ", "),
		FeedTitle:     stripHTML(entry.FeedTitle),
		content:       &lazyContent{raw: entry.Content},
	}
}

// stripCategories strips HTML from each category and drops any that come out
// empty, which keeps the parsers' guarantee that categories are never blank.
// It returns nil when none remain.
func stripCategories(categories []string) []string {
	var out []string
	for _, c := range categories {
		if c = stripHTML(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Content is the entry's full body with HTML stripped, computed on first call
// and shared by every copy of the displayEntry. It is a method so large bodies
// are only stripped when a template references {{.Content}}.
func (d displayEntry) Content() string {
	if d.content == nil {
		return ""
	}
	return d.content.get()
}

// formatPublished renders t as RFC 3339 UTC, or "" for the zero time. Feeds
// that omit dates leave Entry.Published zero; formatting that verbatim would
// show "0001-01-01T00:00:00Z" in templates and a "1/1/0001" embed footer
// (the embed Timestamp is omitempty, so "" drops it from the payload).
func formatPublished(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// publishedUnix is t as Unix seconds, or 0 for the zero time (time.Time{}.Unix()
// is a large negative number), consistent with formatPublished.
func publishedUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
