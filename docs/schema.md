# FeedGroup v1beta1 Schema Reference

`rss2discord.maverickd650.dev/v1beta1` is the served and storage API version of the `FeedGroup` custom resource. It is intended to become `v1` unchanged after a bedding-in period, so its shape is meant to be final: new features should arrive as new optional fields, not as changes to these.

## Overview

A `FeedGroup` declares a set of RSS/Atom feeds that post to one Discord webhook. The operator polls each feed, filters and renders its new entries into Discord messages, and records each feed's delivery state in `.status`.

```yaml
apiVersion: rss2discord.maverickd650.dev/v1beta1
kind: FeedGroup
metadata:
  name: engineering-updates
  namespace: default
spec:
  discordWebhookSecretRef:
    name: discord-webhook
    key: url
  interval: 30m
  username: "Engineering Feeds"
  avatarURL: "https://example.com/avatar.png"
  message:
    content: "New on {{.FeedTitle}}"
    mentions:
      roleIDs: ["123456789012345678"]
    embed:
      color: "#326CE5"
      author:
        name: "{{.Author}}"
        url: "https://kubernetes.io/blog/"
        iconURL: "https://kubernetes.io/images/favicon.png"
      footer:
        text: "Delivered via rss2discord"
  feeds:
    - name: k8s-blog
      url: "https://kubernetes.io/feed.xml"
      filter:
        include:
          categories: ["release", "security"]
        exclude:
          keywords: ["internship"]
    - name: k8s-forum
      url: "https://discuss.kubernetes.io/latest.rss"
      message:
        content: "**{{.Title}}**\n{{.Link}}"
        silent: true
      forumPost:
        name: "{{.Title}}"
        appliedTagIDs: ["234567890123456789"]
```

---

## Specification (`.spec`)

### `FeedGroupSpec`

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `discordWebhookSecretRef` | `SecretKeySelector` | **Yes** | — | Secret and key holding the Discord webhook URL. |
| `suspend` | `bool` | No | `false` | Stops processing every feed in the group. Status and delivery history are kept. |
| `interval` | `string` | No | `30m` | How often feeds are checked. Go duration syntax; at least `1m`. |
| `retries` | `int32` | No | `3` | How many consecutive transient fetch or send failures, the first included, are counted before they are reported as persistent (0–100). Each one below the limit is retried after `retryInterval`, so `3` means two quick retries and `0` or `1` mean none. |
| `retryInterval` | `string` | No | `5m` | Delay between retries, and the base of the permanent-failure backoff. At least `10s`. |
| `message` | `MessageSpec` | No | content `**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})` | The message sent for each entry, for every feed that doesn't set its own. |
| `username` | `string` | No | — | Overrides the webhook's display name. 1–80 characters; must not contain `clyde`, `discord` (any case), `@`, `#`, `:` or <code>```</code>, or be `everyone`/`here`. |
| `avatarURL` | `string` | No | — | Overrides the webhook's avatar. `http://` or `https://`. |
| `catchUpLimit` | `int32` | No | `5` | Caps how many entries are sent when a feed has no usable watermark (first sync, or the watermark scrolled out of the feed). `0` sends no backlog. 0–100. |
| `feeds` | `[]FeedSpec` | **Yes** | — | 1–50 feeds, keyed by `name`. Each `url` must also be unique. |

### `FeedSpec`

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `name` | `string` | **Yes** | — | The feed's identity within the group: a DNS-1123 label (lowercase alphanumerics and `-`, at most 63). The list-map key of `spec.feeds` and `status.feeds`, and the `feed` label of per-feed metrics. |
| `url` | `string` | **Yes** | — | RSS or Atom feed URL, `http://` or `https://`. |
| `suspend` | `bool` | No | `false` | Stops processing this feed. Status and delivery history are kept. |
| `filter` | `Filter` | No | — | Which entries are delivered. |
| `message` | `MessageSpec` | No | the group's | Replaces the group's `message` for this feed, as a whole. |
| `username` | `string` | No | the group's | Overrides the group's `username`. Same rules. |
| `avatarURL` | `string` | No | the group's | Overrides the group's `avatarURL`. |
| `thread` | `ThreadTarget` | No | — | `id`: post into this existing thread or forum post. |
| `forumPost` | `ForumPostSpec` | No | — | Create a new post per entry, for a forum or media channel's webhook. |

`thread` and `forumPost` are mutually exclusive.

### `Filter` and `FilterRules`

`filter.include` and `filter.exclude` are both `FilterRules`. An entry is delivered when it matches **every** criterion `include` sets and **none** of the criteria `exclude` sets. A `FilterRules` must set at least one field.

| Field | Type | Matching |
|---|---|---|
| `keywords` | `[]string` | Case-insensitive substring of the title or description. Any one keyword is enough. |
| `regex` | `string` | RE2 regex over the title and description (joined by a newline). Case-sensitive; use `(?i)` to ignore case. |
| `categories` | `[]string` | The entry's categories (RSS `<category>`, Atom `<category term>`), compared whole and case-insensitively. Any one category is enough. |

So `include: {keywords: [a, b], categories: [c]}` means "(a or b) and c", and `exclude: {keywords: [x], regex: y}` means "drop if x or y".

### `MessageSpec`

| Field | Type | Default | Description |
|---|---|---|---|
| `content` | `string` | — | Template for the message text, sent alongside the embed when both are set. |
| `embed` | `EmbedSpec` | — | Render the entry as an embed. |
| `mentions` | `Mentions` | — | `roleIDs`, `userIDs` (each up to 20 Discord IDs) to ping. They are put at the start of the message text. |
| `silent` | `bool` | `false` | Post without push or desktop notifications. |
| `suppressLinkPreviews` | `bool` | `false` | Don't expand links in the text into previews. Not allowed with `embed`. |

A message must set `content`, `embed`, or both.

**Mentions are an allow-list.** Discord is told to ping exactly the configured `roleIDs` and `userIDs` and nothing else, so a mention written in a template, or appearing in feed text, never pings, and `@everyone`/`@here` are always suppressed. An entry whose `content` renders blank (with no embed) is a render error, whatever the mentions.

**Overrides replace, they don't merge.** A feed's `message` is used instead of the group's whole: to change one field for a feed, repeat the others. This keeps "what does this feed send" answerable from one place, and lets a feed drop the group's embed by setting a message without one.

### `EmbedSpec`

Every text field is a template with the same placeholders and functions as `content`.

| Field | Type | Default | Description |
|---|---|---|---|
| `title` | `string` | `{{.Title}}` | The title, which links to the entry. |
| `description` | `string` | `{{.Description}}` | The body. |
| `color` | `string` | — | Side-bar color as hex: `#5865F2` or `5865F2`. |
| `author` | `EmbedAuthor` | — | `name` (required), `url`, `iconURL`. |
| `footer` | `EmbedFooter` | — | `text` (required), `iconURL`. |
| `imageStyle` | `Thumbnail` \| `Image` \| `None` | `Thumbnail` | How the entry's lead image is shown. |
| `timestamp` | `Published` \| `None` | `Published` | Which time the embed shows. |

An author or footer whose template renders blank for an entry is left out of that entry's embed, since Discord requires their text. All URLs must be `http://` or `https://`; image URLs taken from feed entries that aren't are dropped.

### `ThreadTarget` and `ForumPostSpec`

| Field | Type | Description |
|---|---|---|
| `thread.id` | `string` | The Discord ID of an existing thread (in a text channel) or post (in a forum or media channel). |
| `forumPost.name` | `string` | Template for each new post's title (at most 100 characters once rendered). Required. |
| `forumPost.appliedTagIDs` | `[]string` | Up to 5 IDs of the channel's forum tags to apply to each post. |

### Template placeholders

`{{.Title}}`, `{{.Description}}`, `{{.Content}}`, `{{.Link}}`, `{{.Image}}`, `{{.Published}}`, `{{.PublishedUnix}}`, `{{.Author}}`, `{{.Categories}}`, `{{.FeedTitle}}`, and the functions `truncate`, `default`, `lower`, `upper`, `trim`, `replace` and `discordTime`. See the [README](../README.md#template-fields) for what each holds.

---

## Status (`.status`)

### `FeedGroupStatus`

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | `int64` | The generation the controller last reconciled. |
| `conditions` | `[]Condition` | `Ready` (`Unknown`/`Suspended` while suspended) and `FeedReachable`. |
| `webhookSecretResourceVersion` | `string` | The webhook Secret's resourceVersion when last read; a change gives send-backed-off feeds an immediate retry. |
| `feeds` | `[]FeedStatus` | One entry per `spec.feeds` item, in spec order, keyed by `name`. |

### `FeedStatus`

| Field | Type | Description |
|---|---|---|
| `name` | `string` | The feed's `spec.feeds[].name`. |
| `url` | `string` | The URL this status was last synced from. |
| `conditions` | `[]Condition` | `Reachable` (fetch) and `Delivered` (render/send), with machine-readable reasons such as `HTTP404`. |
| `initialized` | `bool` | The feed has been fetched successfully with a usable configuration. |
| `stateVersion` | `int32` | Format of `lastSeenEntry` and `lastSent` (currently 1). |
| `lastChecked` | `Time` | When the feed was last checked successfully (a 304 counts). |
| `lastSeenEntry` | `string` | The delivery watermark: the newest entry handled. |
| `lastSent` | `map[string]Time` | When each recently sent entry was sent, keyed by entry identity hash. Bounded at 50 per feed. |
| `etag`, `lastModified` | `string` | Conditional-GET validators from the last 200 response. |
| `retryCount` | `int32` | Consecutive failures in the current failure run. |
| `backoffUntil` | `Time` | Permanent-failure backoff: the feed isn't fetched before this. |
| `lastError` | `string` | The last failure's message, at most 2048 bytes. |

### What a URL change keeps

Because a feed is identified by its `name`, changing its `url` keeps its delivery history (`lastSeenEntry`, `lastSent`, `initialized`), so entries already posted aren't posted again. Everything that described the old URL's server is cleared: `etag` and `lastModified` (sending another server's validators could earn a wrong `304 Not Modified`), `lastChecked` (so the new URL is checked right away) and the retry state. Renaming a feed is a different feed: its old status is dropped and it starts over.
