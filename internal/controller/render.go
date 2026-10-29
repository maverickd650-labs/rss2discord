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
	"bytes"
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	"golang.org/x/net/html"
)

// defaultMessageFormat, like any feed/group Format, interpolates entry
// fields via text/template with no markdown escaping (stripHTML removes
// HTML tags, not Discord markdown syntax). A malicious feed publisher --
// not the operator who configured the feed URL -- could break out of
// `[text](url)` or inject formatting/links into the rendered message.
// allowed_mentions.parse=[] (see internal/discord) already blocks the
// high-severity case (@everyone/@here/role/user pings), so this is an
// accepted residual risk: link-spoofing/formatting injection, not
// channel-wide pings.
const defaultMessageFormat = "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"

// maxDiscordMessageLength is Discord's hard cap on webhook message content;
// the API rejects anything longer. Full article bodies in a feed's
// description can easily exceed this once stripped to plain text.
const maxDiscordMessageLength = 2000

// maxTemplateReplaceBytes bounds each replace result before allocating it,
// including intermediate values in pipelines, well above Discord's limits.
const maxTemplateReplaceBytes = 1024 * 1024

// compileMessageTemplate parses a feed's Discord message template once per
// feed per reconcile, instead of reparsing it for every entry sent.
// A blank feed format falls back to the group format, then defaultMessageFormat.
// Template parse errors are returned to the caller.
func compileMessageTemplate(feedGroup *v1alpha1.FeedGroup, feed *v1alpha1.FeedSpec) (*template.Template, error) {
	tmplText := strings.TrimSpace(feed.Format)
	if tmplText == "" {
		tmplText = strings.TrimSpace(feedGroup.Spec.Format)
	}
	if tmplText == "" {
		tmplText = defaultMessageFormat
	}

	return compileTemplate("discordMessage", tmplText, defaultMessageFormat)
}

// compileTemplate parses text as a Discord message template, falling back
// to fallback when text is blank. Leading and trailing whitespace is removed
// from text. Parse errors, including unknown helper names, are returned.
func compileTemplate(name, text, fallback string) (*template.Template, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = fallback
	}
	tmpl, err := template.New(name).Funcs(templateFuncs).Parse(text)
	if err != nil {
		return nil, err
	}
	if err := rejectUnboundedConstructs(tmpl); err != nil {
		return nil, err
	}
	return tmpl, nil
}

// maxTemplateOutputBytes bounds what one template execution may write, well
// above Discord's limits (2000 message / 4096 embed runes).
const maxTemplateOutputBytes = 1024 * 1024

// rejectUnboundedConstructs refuses the template actions that let a
// user-supplied template run unbounded: range (including over an integer,
// e.g. {{range 100000000000}}{{end}}, which spins the reconcile worker
// forever), and define/template (exponential recursion). Templates only
// interpolate a handful of strings, so none of these has a legitimate use;
// {{if}}, {{with}} and the helpers in templateFuncs remain available.
func rejectUnboundedConstructs(tmpl *template.Template) error {
	if len(tmpl.Templates()) > 1 {
		return errors.New("template: {{define}} is not supported")
	}
	if tmpl.Tree == nil {
		return nil
	}
	return checkNodes(tmpl.Root)
}

func checkNodes(node parse.Node) error {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := checkNodes(c); err != nil {
				return err
			}
		}
	case *parse.RangeNode:
		return errors.New("template: {{range}} is not supported")
	case *parse.TemplateNode:
		return errors.New("template: {{template}} is not supported")
	case *parse.IfNode:
		return checkBranch(&n.BranchNode)
	case *parse.WithNode:
		return checkBranch(&n.BranchNode)
	}
	return nil
}

func checkBranch(b *parse.BranchNode) error {
	if err := checkNodes(b.List); err != nil {
		return err
	}
	return checkNodes(b.ElseList)
}

// limitedBuffer is a bytes.Buffer that fails once it would exceed
// maxTemplateOutputBytes, as defense in depth behind rejectUnboundedConstructs.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxTemplateOutputBytes {
		return 0, fmt.Errorf("template output exceeds maximum size of %d bytes", maxTemplateOutputBytes)
	}
	return b.Buffer.Write(p)
}

// templateFuncs are the helpers available in message templates. Keep the set
// small and pure: no environment, file, network or reflection access, since
// templates are user-supplied. Arguments are ordered so they pipeline
// ({{.Description | truncate 200}}).
var templateFuncs = template.FuncMap{
	// truncate clamps s to n runes, ending in an ellipsis when it cuts and n > 1.
	// Non-positive limits return empty text.
	"truncate": func(n int, s string) string {
		out, _ := truncateMessage(s, n)
		return out
	},
	// default returns def when v is blank.
	"default": func(def, v string) string {
		if strings.TrimSpace(v) == "" {
			return def
		}
		return v
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"trim":  strings.TrimSpace,
	// replace replaces all occurrences of old in s, returning an error if the
	// result would exceed maxTemplateReplaceBytes, even in an intermediate pipeline.
	"replace": func(old, new, s string) (string, error) {
		count := strings.Count(s, old)
		remaining := len(s) - count*len(old)
		// Compare before multiplying to avoid overflow. Count handles an
		// empty old string as one match per UTF-8 boundary, like ReplaceAll.
		if remaining > maxTemplateReplaceBytes || (count > 0 && len(new) > (maxTemplateReplaceBytes-remaining)/count) {
			return "", fmt.Errorf("replace: result exceeds maximum size of %d bytes", maxTemplateReplaceBytes)
		}
		return strings.ReplaceAll(s, old, new), nil
	},
	// discordTime renders a Discord dynamic timestamp, e.g.
	// {{.PublishedUnix | discordTime "R"}}. An undated entry
	// (PublishedUnix 0) renders "" rather than "<t:0:R>" (1970).
	// style must be one of t, T, d, D, f, F, or R; other values return an error,
	// including when unix is 0.
	"discordTime": func(style string, unix int64) (string, error) {
		if !strings.Contains("tTdDfFR", style) || len(style) != 1 {
			return "", fmt.Errorf("discordTime: unknown style %q (want one of t T d D f F R)", style)
		}
		if unix == 0 {
			return "", nil
		}
		return fmt.Sprintf("<t:%d:%s>", unix, style), nil
	},
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

// Discord's embed and forum API limits; messages exceeding these are
// rejected outright rather than truncated server-side.
const (
	maxEmbedTitleLength       = 256
	maxEmbedDescriptionLength = 4096
	maxForumThreadNameLength  = 100
)

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

// renderTemplate executes tmpl against entry and clamps the result to max
// runes. The second return value is how many runes were trimmed off (0 if
// the rendered text already fit), as reported by truncateMessage.
// Execution errors, including helper errors, are returned with empty text and zero overflow.
func renderTemplate(tmpl *template.Template, entry rss.Entry, max int) (string, int, error) {
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

	var buf limitedBuffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", 0, err
	}

	content, overflow := truncateMessage(buf.String(), max)
	return content, overflow, nil
}

// buildDiscordMessage renders entry into the message that should actually
// be sent: an embed (colored bubble with title/description and an optional image) when
// embedSpec is enabled, or plain text content otherwise. It also resolves
// forum-channel thread targeting (ForumThreadID takes precedence over a
// rendered ForumThreadName) and group-level webhook branding.
// ImageStyle selects a large image or, by default, a thumbnail. Template execution
// errors and blank plain-text content or rendered thread names return an empty
// message and an error. Successful builds record truncation in messageOverflowChars.
func buildDiscordMessage(
	feedGroup *v1alpha1.FeedGroup,
	embedSpec *v1alpha1.EmbedSpec,
	contentTmpl, descriptionTmpl, threadNameTmpl *template.Template,
	feed *v1alpha1.FeedSpec,
	entry rss.Entry,
) (discord.Message, error) {
	msg := discord.Message{
		Username:  feedGroup.Spec.Username,
		AvatarURL: feedGroup.Spec.AvatarURL,
	}

	var overflow int

	if embedSpec != nil && embedSpec.Enabled {
		description, descOverflow, err := renderTemplate(descriptionTmpl, entry, maxEmbedDescriptionLength)
		if err != nil {
			return discord.Message{}, err
		}
		title, titleOverflow := truncateMessage(stripHTML(entry.Title), maxEmbedTitleLength)
		overflow += descOverflow + titleOverflow
		msg.Embed = &discord.Embed{
			Title:       title,
			Description: description,
			URL:         httpURLOrEmpty(entry.Link),
			Color:       parseHexColor(embedSpec.Color),
			Timestamp:   formatPublished(entry.Published),
			AuthorName:  embedSpec.AuthorName,
			FooterText:  embedSpec.FooterText,
		}
		if image := httpURLOrEmpty(entry.Image); embedSpec.ImageStyle == v1alpha1.EmbedImageStyleImage {
			msg.Embed.ImageURL = image
		} else {
			msg.Embed.ThumbnailURL = image
		}
		// SendMessage clamps title+description+footer+author to Discord's
		// combined 6000-char embed budget, trimming Description further
		// still if the per-field truncation above wasn't enough. That clamp
		// happens inside the discord package, after this function returns,
		// so without folding it in here it would silently undercount actual
		// truncation.
		overflow += discord.EmbedTotalLengthOverflow(*msg.Embed)
	} else {
		content, contentOverflow, err := renderTemplate(contentTmpl, entry, maxDiscordMessageLength)
		if err != nil {
			return discord.Message{}, err
		}
		// Discord rejects a blank message, and a send failure holds the feed
		// on this entry indefinitely. Failing here routes it through the
		// render-error path instead, which skips the entry once retries are
		// exhausted (e.g. "{{.Published}}" for an entry with no date).
		if strings.TrimSpace(content) == "" {
			return discord.Message{}, errors.New("message template rendered empty content for this entry")
		}
		overflow += contentOverflow
		msg.Content = content
	}

	if strings.TrimSpace(feed.ForumThreadID) != "" {
		msg.ThreadID = feed.ForumThreadID
	} else if threadNameTmpl != nil {
		threadName, threadOverflow, err := renderTemplate(threadNameTmpl, entry, maxForumThreadNameLength)
		if err != nil {
			return discord.Message{}, err
		}
		// Without a thread_name a forum webhook rejects the post; fail as a
		// render error for the same reason as blank content above.
		if strings.TrimSpace(threadName) == "" {
			return discord.Message{}, errors.New("forum thread name template rendered empty for this entry")
		}
		overflow += threadOverflow
		msg.ThreadName = threadName
	}

	if overflow > 0 {
		messageOverflowChars.WithLabelValues(feedGroup.Namespace, feedGroup.Name).Observe(float64(overflow))
	}

	return msg, nil
}

// truncateMessage trims content to at most max characters (by rune), since
// Discord rejects webhook messages over its content length limit outright
// rather than truncating them itself. The second return value is how many
// runes were cut (0 if content already fit), so callers can report how
// often/how severely truncation actually happens.
// A cut ends in an ellipsis when max > 1; max == 1 keeps the first rune and
// max <= 0 returns empty text. Overflow is the original rune count minus max,
// floored at zero, so a negative max increases the reported count.
func truncateMessage(content string, max int) (string, int) {
	// len(content) >= rune count, so a byte length within max can't overflow;
	// this skips the []rune allocation for the overwhelmingly common case.
	if len(content) <= max {
		return content, 0
	}
	runes := []rune(content)
	if len(runes) <= max {
		return content, 0
	}
	overflow := len(runes) - max

	const ellipsis = "…"
	// Guard against a max too small to even hold the ellipsis: slicing to a
	// negative bound would panic. No live caller passes a max this small (the
	// limits are all Discord's hundreds/thousands), but keep it total so a
	// future caller can't trip it.
	if max <= 0 {
		return "", overflow
	}
	if max <= len([]rune(ellipsis)) {
		return string(runes[:max]), overflow
	}
	return string(runes[:max-len([]rune(ellipsis))]) + ellipsis, overflow
}

// feedRenderer bundles what turning one feed's entries into Discord messages
// needs: the group and feed specs, the resolved embed config, the templates
// compiled once per processFeed, and the entry filter. Passing it around
// replaces the loose parameters sendNewEntries and friends used to take.
type feedRenderer struct {
	group                            *v1alpha1.FeedGroup
	feed                             *v1alpha1.FeedSpec
	embed                            *v1alpha1.EmbedSpec
	content, description, threadName *template.Template
	filter                           *regexp.Regexp
}

// matches reports whether entry passes the feed's filter.
func (fr *feedRenderer) matches(entry rss.Entry) bool {
	return matchesFilter(fr.feed.Filter, entry, fr.filter)
}

// build renders entry into the message to send; see buildDiscordMessage.
func (fr *feedRenderer) build(entry rss.Entry) (discord.Message, error) {
	return buildDiscordMessage(fr.group, fr.embed, fr.content, fr.description, fr.threadName, fr.feed, entry)
}
