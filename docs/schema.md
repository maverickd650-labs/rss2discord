# FeedGroup v1beta1 Schema Reference

`rss2discord.maverickd650.dev/v1beta1` is the current served and storage API version for the `FeedGroup` custom resource definition in `rss2discord-operator`.

## Overview

A `FeedGroup` resource declaratively defines a collection of RSS/Atom feeds to monitor and dispatch to a designated Discord webhook. The operator periodically polls each feed, parses entries, filters and templates them into Discord messages, and tracks delivery state per feed in `.status`.

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
  retries: 3
  retryInterval: 5m
  username: "Engineering Feeds"
  avatarURL: "https://example.com/avatar.png"
  feeds:
    - name: k8s-blog
      rssUrl: "https://blog.kubernetes.io/feed.xml"
      username: "Kubernetes Blog"
      filter:
        keywords:
          - "release"
          - "security"
        excludeKeywords:
          - "internship"
      embed:
        enabled: true
        color: "#326CE5"
        authorName: "Kubernetes"
        authorURL: "https://blog.kubernetes.io"
        authorIconURL: "https://kubernetes.io/images/favicon.png"
        footerText: "Delivered via rss2discord"
        footerIconURL: "https://kubernetes.io/images/favicon.png"
```

---

## Specification (`.spec`)

### `FeedGroupSpec`

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `discordWebhookSecretRef` | `corev1.SecretKeySelector` | **Yes** | — | Reference to a Kubernetes Secret and key containing the Discord webhook URL. |
| `interval` | `string` | No | `30m` | Base polling cadence for feeds in the group. Minimum `1m`. Uses standard Go duration format (e.g. `15m`, `1h`). |
| `retries` | `int32` | No | `3` | Maximum consecutive retry attempts for transient fetch or send errors before backing off. Range: `0` to `100`. |
| `retryInterval` | `string` | No | `5m` | Delay between retry attempts. Minimum `10s`. Uses standard Go duration format. |
| `suspend` | `bool` | No | `false` | When `true`, suspends all reconciliation for this group without deleting state or watermarks. |
| `username` | `string` | No | — | Default webhook username override for all feeds in the group. Max 80 chars; cannot contain "clyde" or "discord" (case-insensitive). |
| `avatarURL` | `string` | No | — | Default avatar image URL for messages sent by this group. Must start with `http://` or `https://`. |
| `format` | `string` | No | `**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})` | Default Go text template for plain-text Discord messages (used when embed is not enabled). |
| `embed` | `*EmbedSpec` | No | — | Default embed configuration for all feeds in the group. |
| `catchUpLimit` | `*int32` | No | `5` | Caps how many entries are sent when a feed has no usable watermark (e.g. initial sync or scrolled-past watermark). Range: `0` to `100`. Setting `0` skips initial backlogs entirely. |
| `feeds` | `[]FeedSpec` | **Yes** | — | List of up to 50 feeds to poll and process in this group. |

---

### `FeedSpec`

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `rssUrl` | `string` | **Yes** | — | Absolute HTTP or HTTPS URL of the RSS or Atom feed. |
| `name` | `string` | No | — | Optional stable feed identifier formatted as an RFC 1123 DNS label (lowercase alphanumeric and hyphens, max 63 characters). Must be unique within the `FeedGroup`. |
| `paused` | `bool` | No | `false` | Pauses polling and processing for this specific feed without dropping its status or watermarks. |
| `username` | `string` | No | — | Webhook username override specific to this feed. Overrides `spec.username`. Max 80 chars; cannot contain "clyde" or "discord". |
| `avatarURL` | `string` | No | — | Webhook avatar image URL override specific to this feed. Overrides `spec.avatarURL`. Must start with `http://` or `https://`. |
| `filter` | `*Filter` | No | — | Filtering rules determining which entries are published. |
| `format` | `string` | No | — | Overrides `spec.format` for this specific feed. |
| `embed` | `*EmbedSpec` | No | — | Overrides `spec.embed` for this specific feed. |
| `forumThreadName` | `string` | No | — | Template for creating a new forum thread per entry when targeting a Discord forum channel webhook. |
| `forumThreadID` | `string` | No | — | Discord snowflake ID of an existing forum thread to post into. Takes precedence over `forumThreadName`. |

---

### `Filter`

Filters control which entries are eligible for dispatch. Positive filters require matches; negative filters act as immediate vetoes.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `keywords` | `[]string` | No | — | Case-insensitive positive keywords matched against title or description (OR semantics). |
| `regex` | `string` | No | — | Regular expression matched against entry title or description. |
| `excludeKeywords` | `[]string` | No | — | Case-insensitive exclusion keywords. Any match rejects the entry immediately. |
| `excludeRegex` | `string` | No | — | Regular expression exclusion. Any match rejects the entry immediately. |

#### Filtering Evaluation Logic
1. **Exclusion Veto:** If `excludeRegex` is provided and matches, the entry is dropped.
2. **Exclusion Keywords:** If any keyword in `excludeKeywords` matches the entry's title or description, the entry is dropped.
3. **Positive Regex:** If `regex` is provided and does not match, the entry is dropped.
4. **Positive Keywords:** If `keywords` is provided and none match, the entry is dropped.
5. Entries passing all checks proceed to rendering and dispatch.

---

### `EmbedSpec`

Controls formatting entries as rich Discord embeds instead of plain text.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `enabled` | `bool` | No | `false` | Enables rich embed formatting. |
| `color` | `string` | No | — | Hex color code for the embed border (e.g., `#5865F2` or `5865F2`). |
| `descriptionFormat` | `string` | No | `{{.Description}}` | Go template rendering the embed description body. |
| `authorName` | `string` | No | — | Author display text on the embed header. Discord requires this for the author section to render. |
| `authorURL` | `string` | No | — | Hyperlink target for the author name. Must start with `http://` or `https://`. Only sent if `authorName` is provided. |
| `authorIconURL` | `string` | No | — | Small icon displayed next to the author name. Must start with `http://` or `https://`. Only sent if `authorName` is provided. |
| `footerText` | `string` | No | — | Footer text at the bottom of the embed. Discord requires this for the footer section to render. |
| `footerIconURL` | `string` | No | — | Small icon displayed next to the footer text. Must start with `http://` or `https://`. Only sent if `footerText` is provided. |
| `imageStyle` | `string` | No | `thumbnail` | Layout style for lead images: `thumbnail` (compact right side) or `image` (large banner below description). |

---

## Status (`.status`)

### `FeedGroupStatus`

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | `int64` | The most recent metadata generation observed by the controller. |
| `conditions` | `[]metav1.Condition` | Group-level conditions (`Ready`, `FeedReachable`). |
| `webhookSecretResourceVersion` | `string` | The resource version of the referenced webhook Secret at the time it was last read. Used to detect secret updates and clear send backoffs. |
| `feeds` | `[]FeedStatus` | Array of per-feed runtime statuses, maintained in `spec.feeds` order. |

### `FeedStatus`

| Field | Type | Description |
|---|---|---|
| `name` | `string` | Stable feed name matching `spec.feeds[i].name`. |
| `rssUrl` | `string` | Feed URL currently associated with this status entry. |
| `initialized` | `bool` | Whether the feed has completed at least one successful sync cycle. |
| `lastChecked` | `*metav1.Time` | RFC 3339 timestamp of the most recent fetch attempt. |
| `lastSeenEntry` | `string` | Delivery watermark representing the newest entry processed. |
| `lastSent` | `map[string]string` | De-duplication ledger mapping entry identity hashes to their RFC 3339 send timestamp. Bounded to prevent unbounded status growth. |
| `etag` | `string` | HTTP `ETag` validator stored from the most recent successful 200 response for conditional GETs. |
| `lastModified` | `string` | HTTP `Last-Modified` header string stored from the most recent 200 response for conditional GETs. |
| `retryCount` | `int32` | Number of consecutive failed attempts in the current failure phase. |
| `backoffUntil` | `*metav1.Time` | RFC 3339 timestamp until which the feed is in backoff and skipped by periodic reconciles. |
| `lastError` | `string` | Error message from the most recent failure, clamped to 2048 bytes. |
| `conditions` | `[]metav1.Condition` | Per-feed conditions (`Reachable`, `Delivered`). |

---

## Key Features in v1beta1

### 1. Stable Feed Identity (`name`)
In `v1alpha1`, feed statuses were keyed solely by `rssUrl`. Changing a feed's URL caused the controller to treat it as a new feed, discarding its watermark (`lastSeenEntry`), de-duplication ledger (`lastSent`), and conditional-GET headers (`etag`, `lastModified`).

In `v1beta1`, `name` provides a durable identifier:
- **Durable Watermarks:** When a feed with a `name` has its `rssUrl` changed in `spec.feeds`, the controller matches by `name` first, updates `rssUrl` in `.status.feeds`, and retains the full delivery history.
- **Reservation Safety:** Name matching reserves prior statuses before URL fallbacks run, guaranteeing that each prior status is claimed at most once.
- **Prometheus Metric Cleanup:** The previous RSS URL is reported as pruned, immediately dropping gauge series for the old URL and creating series for the new URL.
- **Validation:** Names must be RFC 1123 DNS labels (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, max 63 characters). CEL validation ensures that any configured feed names within a group are unique.

### 2. Group Suspension (`spec.suspend`)
Setting `spec.suspend: true` temporarily pauses all processing for the entire `FeedGroup`:
- Reconciles short-circuit immediately without reading Secrets or hitting external networks.
- Condition `Ready` is marked `Status=Unknown` with `Reason=Suspended`.
- Existing watermarks, ledgers, and feed conditions are preserved.
- Prometheus gauge metrics drop their series (identical to paused feeds) so suspended groups do not trigger false alerts.
- Periodic polling requeues are unscheduled until the group is resumed or its specification changes.

### 3. Sender Override Precedence
Messages sent to Discord resolve their sender identity using a three-tier fallback hierarchy:
$$\text{FeedSpec override} \longrightarrow \text{FeedGroupSpec default} \longrightarrow \text{Discord Webhook configuration}$$

- `feed.username` takes precedence over `group.username`, falling back to Discord.
- `feed.avatarURL` takes precedence over `group.avatarURL`, falling back to Discord.
- Username and avatar overrides can be mixed (e.g. custom username with group avatar).

### 4. Exclusion Vetoes (`filter.excludeKeywords`, `filter.excludeRegex`)
Negative filters allow rejecting unwanted entries (e.g., job postings, sponsored articles, recurring automated digests):
- Any match against `excludeKeywords` or `excludeRegex` immediately drops the entry, regardless of whether it matched positive criteria.
- `excludeKeywords` is case-insensitive.
- `excludeRegex` allows full regex syntax, including flags like `(?i)` for case insensitivity.

### 5. Extended Discord Embeds
Embed authors and footers now support links and icon avatars:
- `authorURL` and `authorIconURL` attach links and icons to the author banner.
- `footerIconURL` attaches an icon beside the footer text.
- To prevent Discord webhook rejection (`400 Bad Request`), Discord requires author names and footer texts when those objects exist. The operator only constructs author or footer objects when `authorName` or `footerText` is non-empty.
- All URLs are validated and restricted to `http://` or `https://` schemes.

### 6. Standardized Typed Timestamps
- `lastChecked` and `backoffUntil` are native `*metav1.Time` fields.
- Malformed timestamps cannot be applied through typed clients or the Kubernetes API server.
- `lastModified` remains a string because it holds HTTP-date formatted headers (RFC 7231 / RFC 1123, e.g. `Wed, 21 Oct 2026 07:28:00 GMT`), not RFC 3339 timestamps.

---

## Upgrading from v1alpha1 to v1beta1

`v1beta1` is a clean break promotion with no conversion webhooks or dual-version serving. To migrate existing resources:

1. Update your YAML manifests:
   - Change `apiVersion: rss2discord.maverickd650.dev/v1alpha1` to `apiVersion: rss2discord.maverickd650.dev/v1beta1`.
   - Optionally add `name: <label>` to feeds to lock in stable identity for future URL changes.
2. Re-apply your resources using `kubectl apply -f <manifest>.yaml`.
