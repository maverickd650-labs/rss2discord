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
	"errors"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/msgtemplate"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	"golang.org/x/net/html"
)

// defaultMessageFormat, like any feed/group Format, interpolates entry
// fields via the msgtemplate sandbox (text/template) with no markdown escaping (stripHTML removes
// HTML tags, not Discord markdown syntax). A malicious feed publisher --
// not the operator who configured the feed URL -- could break out of
// `[text](url)` or inject formatting/links into the rendered message.
// allowed_mentions.parse=[] (see internal/discord) already blocks the
// high-severity case (@everyone/@here/role/user pings), so this is an
// accepted residual risk: link-spoofing/formatting injection, not
// channel-wide pings.
const defaultMessageFormat = "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"

// compileMessageTemplate parses a feed's Discord message template once per
// feed per reconcile, instead of reparsing it for every entry sent.
// A blank feed format falls back to the group format, then defaultMessageFormat.
// Template parse errors are returned to the caller.
func compileMessageTemplate(feedGroup *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) (*msgtemplate.Template, error) {
	tmplText := strings.TrimSpace(feed.Format)
	if tmplText == "" {
		tmplText = strings.TrimSpace(feedGroup.Spec.Format)
	}
	if tmplText == "" {
		tmplText = defaultMessageFormat
	}

	return msgtemplate.Compile("discordMessage", tmplText, defaultMessageFormat)
}

// resolveEmbedSpec returns the feed's embed config if it set one, otherwise
// falling back to the FeedGroup's. Returns nil if neither configured one,
// meaning the feed sends plain-text content.
func resolveEmbedSpec(feedGroup *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) *v1alpha1.EmbedSpec {
	if feed.Embed != nil {
		return feed.Embed
	}
	return feedGroup.Spec.Embed
}

// parseHexColor parses a hexadecimal embed color such as "#RRGGBB" or "RRGGBB",
// accepting surrounding whitespace. It returns 0 for blank input or a parse
// error; it does not enforce six digits or the 24-bit RGB range.
func parseHexColor(s string) int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 16, 32)
	if err != nil {
		return 0
	}
	return int(v)
}

// httpURLOrEmpty returns rawURL unchanged if it parses with an http or https
// scheme, or "" otherwise. It does not require a host or check reachability.
// Entry links/images come straight from feed XML, which is
// untrusted external content; Discord embeds (msg.Embed.URL/ThumbnailURL)
// are happy to carry through a javascript:/data: URI verbatim, so this keeps
// those out before they ever reach a user's client.
func httpURLOrEmpty(rawURL string) string {
	parsed, err := neturl.Parse(rawURL)
	if err != nil || (parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS) {
		return ""
	}
	return rawURL
}

// blankLineRegex collapses runs of 3+ newlines (left behind once tags are
// stripped) down to a single blank line.
var blankLineRegex = regexp.MustCompile(`\n{3,}`)

// continueReadingLinkRegex matches a trailing "Continue reading..." anchor,
// the boilerplate several feeds (e.g. the Guardian's) append to truncated
// descriptions. It's redundant in Discord: the embed title/message already
// links to the full article, so left in place it reads as a dead "read
// more" link with nowhere to click.
var continueReadingLinkRegex = regexp.MustCompile(`(?i)<a\b[^>]*>\s*continue reading\s*(\.{2,3}|…)?\s*</a>\s*(</[a-z0-9]+>\s*)*$`)

// continueReadingTextRegex is a fallback for feeds that ship the same
// boilerplate as plain text rather than wrapping it in an <a> tag.
var continueReadingTextRegex = regexp.MustCompile(`(?i)continue reading\s*(\.{2,3}|…)?\s*$`)

// stripHTML converts an RSS/Atom entry's title or description into
// Discord-friendly plain text. Many feeds (e.g. the Guardian's) ship these
// as raw or escaped HTML, which Discord otherwise renders as literal tag
// soup.
//
// Markup is removed with the HTML tokenizer rather than a regex so that a
// ">" inside a quoted attribute or comment, or a bare "<" in prose, can't
// derail tag detection, and so script/style bodies are dropped rather than
// leaked as text. Block-level tags (p, li, br, div, h1-h6) become newlines
// so paragraph and list structure survives.
func stripHTML(input string) string {
	input = continueReadingLinkRegex.ReplaceAllString(input, "")

	var sb strings.Builder
	sb.Grow(len(input))

	var skipUntil string // non-empty while inside a <script>/<style> element
	z := html.NewTokenizer(strings.NewReader(input))
	for {
		tokenType := z.Next()
		switch tokenType {
		case html.ErrorToken:
			return finishStrippedText(sb.String())
		case html.TextToken:
			if skipUntil == "" {
				sb.Write(z.Text())
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			// Atom XHTML may use prefixes inherited from the feed element.
			if _, local, ok := strings.Cut(tag, ":"); ok {
				tag = local
			}
			switch {
			case skipUntil != "":
				if tag == skipUntil {
					skipUntil = ""
				}
			case tag == "script" || tag == "style":
				// A self-closing <script/> has no body to skip.
				if tokenType == html.StartTagToken {
					skipUntil = tag
				}
			case isBlockTag(tag):
				sb.WriteByte('\n')
			}
		}
	}
}

// isBlockTag reports whether tag delimits block-level content (or a line
// break), which stripHTML converts to a newline.
func isBlockTag(tag string) bool {
	switch tag {
	case "p", "li", "br", "div", "h1", "h2", "h3", "h4", "h5", "h6":
		return true
	}
	return false
}

// finishStrippedText normalizes whitespace in text left after tag removal:
// trims each line, collapses runs of blank lines, and drops a trailing
// plain-text "Continue reading" stub.
func finishStrippedText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	text = blankLineRegex.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	text = continueReadingTextRegex.ReplaceAllString(strings.TrimSpace(text), "")

	return strings.TrimSpace(text)
}

// defaultDescriptionFormat is used for an embed's description when neither
// the feed nor the group set EmbedSpec.DescriptionFormat.
const defaultDescriptionFormat = "{{.Description}}"

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

// templateData is the value a message template executes against; its
// exported fields and methods are the documented {{.Placeholders}}.
type templateData struct {
	Title       string
	Description string
	Link        string
	Image       string
	Published   string
	// PublishedUnix is Published as Unix seconds (0 when undated, like
	// Published's ""), for Discord's <t:UNIX:R> dynamic timestamps.
	PublishedUnix int64
	Author        string
	Categories    string
	FeedTitle     string

	content string
}

// Content is the entry's full body with HTML stripped. It is a method rather
// than a field so the strip -- over a body that can run to tens of KB -- only
// happens for templates that actually reference {{.Content}}.
func (d templateData) Content() string {
	return stripHTML(d.content)
}

// renderTemplate executes tmpl against entry and returns the raw output. It
// is not clamped to Discord's limits (feedRenderer.build does that once, via
// discord.Fit); its size is already bounded by the msgtemplate sandbox. Execution errors, including helper errors, are
// returned with empty text.
func renderTemplate(tmpl *msgtemplate.Template, entry rss.Entry) (string, error) {
	data := templateData{
		Title:         stripHTML(entry.Title),
		Description:   stripHTML(entry.Description),
		Link:          httpURLOrEmpty(entry.Link),
		Image:         httpURLOrEmpty(entry.Image),
		Published:     formatPublished(entry.Published),
		PublishedUnix: publishedUnix(entry.Published),
		Author:        entry.Author,
		Categories:    strings.Join(entry.Categories, ", "),
		FeedTitle:     stripHTML(entry.FeedTitle),
		content:       entry.Content,
	}

	return tmpl.Execute(data)
}

// feedRenderer turns one feed's entries into Discord messages. It owns the
// preparation -- filter compilation, embed inheritance and template
// compilation -- so callers can't assemble inconsistent state (such as an
// enabled embed with no description template). Build one with newFeedRenderer
// once per processFeed.
type feedRenderer struct {
	group                            *v1alpha1.FeedGroup
	feed                             *v1alpha1.FeedSpec
	embed                            *v1alpha1.EmbedSpec
	content, description, threadName *msgtemplate.Template
	filter                           entryFilter
}

// newFeedRenderer prepares the renderer for feed within group: it compiles the
// filter regex, resolves the embed config (feed over group), and compiles the
// description template if the embed is enabled or the content template if not,
// plus the forum thread-name template when one is set. The first config error
// is returned unwrapped, checked in that order.
func newFeedRenderer(group *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) (*feedRenderer, error) {
	filter, err := compileEntryFilter(feed.Filter)
	if err != nil {
		return nil, err
	}

	fr := &feedRenderer{group: group, feed: feed, embed: resolveEmbedSpec(group, feed), filter: filter}
	if fr.embed != nil && fr.embed.Enabled {
		fr.description, err = msgtemplate.Compile("discordEmbedDescription", fr.embed.DescriptionFormat, defaultDescriptionFormat)
	} else {
		fr.content, err = compileMessageTemplate(group, feed)
	}
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(feed.ForumThreadName) != "" {
		fr.threadName, err = msgtemplate.Compile("discordThreadName", feed.ForumThreadName, "")
		if err != nil {
			return nil, err
		}
	}
	return fr, nil
}

// matches reports whether entry passes the feed's filter.
func (fr *feedRenderer) matches(entry rss.Entry) bool {
	return fr.filter.matches(entry)
}

// build renders entry into the message that should actually
// be sent: an embed (colored bubble with title/description and an optional image) when
// the resolved embed is enabled, or plain text content otherwise. It also resolves
// forum-channel thread targeting (ForumThreadID takes precedence over a
// rendered ForumThreadName) and group-level webhook branding.
// ImageStyle selects a large image or, by default, a thumbnail. Template execution
// errors and blank plain-text content or rendered thread names return an empty
// message and an error. The message is fitted to Discord's length limits by a
// single discord.Fit call at the end; the second return value is how many
// runes that trimmed (0 when nothing was); it is data only. The caller
// that actually sends the message records it in messageOverflowChars, so
// diagnostic renders (see otherEntryRenders) don't add samples.
func (fr *feedRenderer) build(entry rss.Entry) (discord.Message, int, error) {
	msg := discord.Message{
		Username:  fr.group.Spec.Username,
		AvatarURL: fr.group.Spec.AvatarURL,
	}

	if fr.embed != nil && fr.embed.Enabled {
		description, err := renderTemplate(fr.description, entry)
		if err != nil {
			return discord.Message{}, 0, err
		}
		msg.Embed = &discord.Embed{
			Title:       stripHTML(entry.Title),
			Description: description,
			URL:         httpURLOrEmpty(entry.Link),
			Color:       parseHexColor(fr.embed.Color),
			Timestamp:   formatPublished(entry.Published),
			AuthorName:  fr.embed.AuthorName,
			FooterText:  fr.embed.FooterText,
		}
		if image := httpURLOrEmpty(entry.Image); fr.embed.ImageStyle == v1alpha1.EmbedImageStyleImage {
			msg.Embed.ImageURL = image
		} else {
			msg.Embed.ThumbnailURL = image
		}
	} else {
		content, err := renderTemplate(fr.content, entry)
		if err != nil {
			return discord.Message{}, 0, err
		}
		// Discord rejects a blank message, and a send failure holds the feed
		// on this entry indefinitely. Failing here routes it through the
		// render-error path instead, which skips the entry once retries are
		// exhausted (e.g. "{{.Published}}" for an entry with no date).
		if strings.TrimSpace(content) == "" {
			return discord.Message{}, 0, errors.New("message template rendered empty content for this entry")
		}
		msg.Content = content
	}

	if strings.TrimSpace(fr.feed.ForumThreadID) != "" {
		msg.ThreadID = fr.feed.ForumThreadID
	} else if fr.threadName != nil {
		threadName, err := renderTemplate(fr.threadName, entry)
		if err != nil {
			return discord.Message{}, 0, err
		}
		// Without a thread_name a forum webhook rejects the post; fail as a
		// render error for the same reason as blank content above.
		if strings.TrimSpace(threadName) == "" {
			return discord.Message{}, 0, errors.New("forum thread name template rendered empty for this entry")
		}
		msg.ThreadName = threadName
	}

	fitted, overflow := discord.Fit(msg)
	return fitted, overflow, nil
}

// otherEntryRenders reports whether any entry other than skip renders
// successfully with the feed's templates, which distinguishes "this entry is
// the problem" from "the template is broken for everything".
func (fr *feedRenderer) otherEntryRenders(entries []rss.Entry, skip rss.Entry) bool {
	for _, other := range entries {
		if entryIdentity(other) == entryIdentity(skip) {
			continue
		}
		if _, _, err := fr.build(other); err == nil {
			return true
		}
	}
	return false
}
