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
	"cmp"
	"errors"
	"strconv"
	"strings"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/msgtemplate"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// v1beta1.DefaultMessageContent, like any message template, interpolates
// entry fields via the msgtemplate sandbox (text/template) with no markdown
// escaping (stripHTML removes HTML tags, not Discord markdown syntax). A
// malicious feed publisher -- not the operator who configured the feed URL --
// could break out of `[text](url)` or inject formatting/links into the
// rendered message. allowed_mentions (see internal/discord) already blocks
// the high-severity case: nothing but the configured Mentions can ping, so a
// feed's "@everyone" or "<@&role>" text is inert. This is an accepted
// residual risk: link-spoofing/formatting injection, not pings.

// defaultMessage is the message used when neither the feed nor the group sets
// one.
var defaultMessage = v1beta1.MessageSpec{Content: v1beta1.DefaultMessageContent}

// resolveMessage returns the feed's message if it set one, otherwise the
// group's, otherwise defaultMessage. The winner is used whole: a feed's
// message is never merged with the group's.
func resolveMessage(feedGroup *v1beta1.FeedGroup, feed *v1beta1.FeedSpec) *v1beta1.MessageSpec {
	return cmp.Or(feed.Message, feedGroup.Spec.Message, &defaultMessage)
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

// Default embed templates, used when EmbedSpec leaves Title/Description unset.
const (
	defaultEmbedTitle       = "{{.Title}}"
	defaultEmbedDescription = "{{.Description}}"
)

// renderTemplate executes tmpl directly against the sanitized display entry and
// returns the raw output. It is not clamped to Discord's limits (feedRenderer.build
// does that once, via discord.Fit); its size is already bounded by the msgtemplate
// sandbox. Execution errors, including helper errors, are returned with empty text.
func renderTemplate(tmpl *msgtemplate.Template, entry displayEntry) (string, error) {
	return tmpl.Execute(entry)
}

// feedRenderer turns one feed's entries into Discord messages. It owns the
// preparation -- filter compilation, message resolution (feed over group) and
// template compilation -- so callers can't assemble inconsistent state (such
// as an embed with no description template). Build one with newFeedRenderer
// once per processFeed.
type feedRenderer struct {
	group   *v1beta1.FeedGroup
	feed    *v1beta1.FeedSpec
	message *v1beta1.MessageSpec
	filter  entryFilter
	// content is nil when the message sets no content. The embed templates
	// are nil without an embed; authorName and footerText also without an
	// author/footer. threadName is nil without a forumPost.
	content, threadName                        *msgtemplate.Template
	title, description, authorName, footerText *msgtemplate.Template
	// mentions is the ping text put before the content ("" for none).
	mentions string
}

// newFeedRenderer prepares the renderer for feed within group: it compiles the
// filter, resolves the message, and compiles every template the message and
// the feed's forum post use. The first config error is returned unwrapped.
func newFeedRenderer(group *v1beta1.FeedGroup, feed *v1beta1.FeedSpec) (*feedRenderer, error) {
	filter, err := compileEntryFilter(feed.Filter)
	if err != nil {
		return nil, err
	}

	fr := &feedRenderer{group: group, feed: feed, message: resolveMessage(group, feed), filter: filter}
	fr.mentions = mentionText(fr.message.Mentions)
	var errs []error
	compile := func(name, text, fallback string) *msgtemplate.Template {
		tmpl, err := msgtemplate.Compile(name, text, fallback)
		errs = append(errs, err)
		return tmpl
	}
	if fr.message.Content != "" {
		fr.content = compile("discordMessage", fr.message.Content, "")
	}
	if embed := fr.message.Embed; embed != nil {
		fr.title = compile("discordEmbedTitle", embed.Title, defaultEmbedTitle)
		fr.description = compile("discordEmbedDescription", embed.Description, defaultEmbedDescription)
		if embed.Author != nil {
			fr.authorName = compile("discordEmbedAuthorName", embed.Author.Name, "")
		}
		if embed.Footer != nil {
			fr.footerText = compile("discordEmbedFooterText", embed.Footer.Text, "")
		}
	}
	if feed.ForumPost != nil {
		fr.threadName = compile("discordThreadName", feed.ForumPost.Name, "")
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return fr, nil
}

// mentionText is the ping text for m: each role as <@&id> and each user as
// <@id>, space-separated, or "" for no mentions.
func mentionText(m *v1beta1.Mentions) string {
	if m == nil {
		return ""
	}
	pings := make([]string, 0, len(m.RoleIDs)+len(m.UserIDs))
	for _, id := range m.RoleIDs {
		pings = append(pings, "<@&"+id+">")
	}
	for _, id := range m.UserIDs {
		pings = append(pings, "<@"+id+">")
	}
	return strings.Join(pings, " ")
}

// matches reports whether entry passes the feed's filter.
func (fr *feedRenderer) matches(entry rss.Entry) bool {
	return fr.filter.matches(entry)
}

// errEmptyContent is the render error for a message with no embed whose
// content rendered blank: Discord rejects a blank message, and a send failure
// holds the feed on this entry indefinitely, whereas a render error skips the
// entry once retries are exhausted (e.g. "{{.Published}}" for an entry with no
// date).
var errEmptyContent = errors.New("message template rendered empty content for this entry")

// build renders entry into the message that should actually be sent: the
// resolved message's content (after its mentions) and/or embed, its flags and
// mentions, the feed's thread target, and webhook branding (FeedSpec overrides
// FeedGroupSpec, which in turn overrides Discord webhook defaults). Template
// execution errors, blank content without an embed and a blank rendered forum
// post name return an empty message and an error. The message is fitted to
// Discord's length limits by a single discord.Fit call at the end; the second
// return value is how many runes that trimmed (0 when nothing was); it is data
// only. The caller that actually sends the message records it in
// messageOverflowChars, so diagnostic renders (see
// entryDelivery.otherEntryRenders) don't add samples.
func (fr *feedRenderer) build(raw rss.Entry) (discord.Message, int, error) {
	entry := displayOf(raw)
	msg := discord.Message{
		Username:       cmp.Or(fr.feed.Username, fr.group.Spec.Username),
		AvatarURL:      cmp.Or(fr.feed.AvatarURL, fr.group.Spec.AvatarURL),
		Silent:         fr.message.Silent,
		SuppressEmbeds: fr.message.SuppressLinkPreviews,
	}
	if m := fr.message.Mentions; m != nil {
		msg.MentionRoleIDs, msg.MentionUserIDs = m.RoleIDs, m.UserIDs
	}

	var content string
	if fr.content != nil {
		var err error
		if content, err = renderTemplate(fr.content, entry); err != nil {
			return discord.Message{}, 0, err
		}
	}
	if fr.message.Embed != nil {
		embed, err := fr.buildEmbed(entry)
		if err != nil {
			return discord.Message{}, 0, err
		}
		msg.Embed = embed
	} else if strings.TrimSpace(content) == "" {
		return discord.Message{}, 0, errEmptyContent
	}
	msg.Content = joinNonBlank(fr.mentions, content)

	switch {
	case fr.feed.Thread != nil:
		msg.ThreadID = fr.feed.Thread.ID
	case fr.threadName != nil:
		threadName, err := renderTemplate(fr.threadName, entry)
		if err != nil {
			return discord.Message{}, 0, err
		}
		// Without a thread_name a forum webhook rejects the post; fail as a
		// render error for the same reason as blank content.
		if strings.TrimSpace(threadName) == "" {
			return discord.Message{}, 0, errors.New("forum post name template rendered empty for this entry")
		}
		msg.ThreadName = threadName
		msg.AppliedTags = fr.feed.ForumPost.AppliedTagIDs
	}

	fitted, overflow := discord.Fit(msg)
	return fitted, overflow, nil
}

// joinNonBlank joins the non-blank of mentions and content with a newline.
func joinNonBlank(mentions, content string) string {
	switch {
	case mentions == "":
		return content
	case strings.TrimSpace(content) == "":
		return mentions
	default:
		return mentions + "\n" + content
	}
}

// buildEmbed renders the resolved message's embed for entry. An author or
// footer whose text renders blank is left out, since Discord requires it.
func (fr *feedRenderer) buildEmbed(entry displayEntry) (*discord.Embed, error) {
	spec := fr.message.Embed
	embed := &discord.Embed{URL: entry.Link, Color: parseHexColor(spec.Color)}
	var err error
	if embed.Title, err = renderTemplate(fr.title, entry); err != nil {
		return nil, err
	}
	if embed.Description, err = renderTemplate(fr.description, entry); err != nil {
		return nil, err
	}
	if spec.Author != nil {
		name, err := renderTemplate(fr.authorName, entry)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(name) != "" {
			embed.AuthorName = name
			embed.AuthorURL = httpURLOrEmpty(spec.Author.URL)
			embed.AuthorIconURL = httpURLOrEmpty(spec.Author.IconURL)
		}
	}
	if spec.Footer != nil {
		text, err := renderTemplate(fr.footerText, entry)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) != "" {
			embed.FooterText = text
			embed.FooterIconURL = httpURLOrEmpty(spec.Footer.IconURL)
		}
	}
	if spec.Timestamp != v1beta1.EmbedTimestampNone {
		embed.Timestamp = entry.Published
	}
	switch spec.ImageStyle {
	case v1beta1.EmbedImageStyleNone:
	case v1beta1.EmbedImageStyleImage:
		embed.ImageURL = entry.Image
	default:
		embed.ThumbnailURL = entry.Image
	}
	return embed, nil
}
