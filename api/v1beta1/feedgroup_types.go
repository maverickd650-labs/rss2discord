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

// +kubebuilder:object:generate=true
// +groupName=rss2discord.maverickd650.dev
package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FilterRules is one set of match criteria. Within Keywords and Categories any
// one item matching is enough (OR); when several criteria are set, how they
// combine depends on whether the rules are Filter.Include or Filter.Exclude.
// +kubebuilder:validation:MinProperties=1
type FilterRules struct {
	// keywords match, case-insensitively, as substrings of the entry's title
	// or description. Any one keyword matching is enough.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:MinLength=1
	Keywords []string `json:"keywords,omitempty"`

	// regex is an RE2 regular expression matched against the entry's title
	// and description (joined by a newline). It is case-sensitive; prefix it
	// with (?i) for a case-insensitive match.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:MinLength=1
	Regex string `json:"regex,omitempty"`

	// categories match the entry's RSS <category> / Atom <category term>
	// values: case-insensitively, as whole values (not substrings). Any one
	// category matching is enough.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:MinLength=1
	Categories []string `json:"categories,omitempty"`
}

// Filter selects which entries of a feed are delivered.
// +kubebuilder:validation:MinProperties=1
type Filter struct {
	// include, when set, delivers only entries that match every criterion it
	// sets (keywords AND regex AND categories).
	// +optional
	Include *FilterRules `json:"include,omitempty"`

	// exclude drops an entry that matches any criterion it sets (keywords OR
	// regex OR categories), even if it matched include.
	// +optional
	Exclude *FilterRules `json:"exclude,omitempty"`
}

// Embed image styles (EmbedSpec.ImageStyle).
const (
	// EmbedImageStyleThumbnail shows an entry's image as a small thumbnail
	// beside the description. It is the default.
	EmbedImageStyleThumbnail = "Thumbnail"
	// EmbedImageStyleImage shows an entry's image large, below the description.
	EmbedImageStyleImage = "Image"
	// EmbedImageStyleNone shows no entry image.
	EmbedImageStyleNone = "None"
)

// Embed timestamp sources (EmbedSpec.Timestamp).
const (
	// EmbedTimestampPublished shows the entry's published time on the embed
	// (when the entry has one). It is the default.
	EmbedTimestampPublished = "Published"
	// EmbedTimestampNone shows no timestamp.
	EmbedTimestampNone = "None"
)

// EmbedAuthor is an embed's author line.
type EmbedAuthor struct {
	// name is a template for the author line's text, with the same
	// placeholders as MessageSpec.Content (e.g. "{{.Author}}").
	// +required
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// url is the link opened by clicking the author name.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url,omitempty"`

	// iconURL is a small icon shown beside the author name.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	IconURL string `json:"iconURL,omitempty"`
}

// EmbedFooter is an embed's footer line.
type EmbedFooter struct {
	// text is a template for the footer's text, with the same placeholders
	// as MessageSpec.Content (e.g. "{{.FeedTitle}}").
	// +required
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	Text string `json:"text"`

	// iconURL is a small icon shown beside the footer text.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	IconURL string `json:"iconURL,omitempty"`
}

// EmbedSpec renders an entry as a Discord embed: the colored panel with a
// linked title, description, image, author and footer. Every text field is a
// template with the same placeholders as MessageSpec.Content.
type EmbedSpec struct {
	// title is a template for the embed's title, which links to the entry.
	// Defaults to "{{.Title}}".
	// +optional
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title,omitempty"`

	// description is a template for the embed's body. Defaults to
	// "{{.Description}}".
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:MinLength=1
	Description string `json:"description,omitempty"`

	// color is the embed's side-bar color, as a hex string (e.g. "#5865F2"
	// or "5865F2").
	// +optional
	// +kubebuilder:validation:Pattern=`^#?[0-9a-fA-F]{6}$`
	// +kubebuilder:validation:MaxLength=7
	// +kubebuilder:validation:MinLength=6
	Color string `json:"color,omitempty"`

	// author is the embed's author line.
	// +optional
	Author *EmbedAuthor `json:"author,omitempty"`

	// footer is the embed's footer line.
	// +optional
	Footer *EmbedFooter `json:"footer,omitempty"`

	// imageStyle chooses how an entry's image is shown: Thumbnail (small,
	// beside the description), Image (large, below it) or None.
	// +default="Thumbnail"
	// +kubebuilder:validation:Enum=Thumbnail;Image;None
	// +optional
	ImageStyle string `json:"imageStyle,omitempty"`

	// timestamp chooses the time shown on the embed: Published (the entry's
	// published time, when it has one) or None.
	// +default="Published"
	// +kubebuilder:validation:Enum=Published;None
	// +optional
	Timestamp string `json:"timestamp,omitempty"`
}

// Mentions are the Discord roles and users a message pings. Only the IDs
// listed here can ping: mentions written in templates or in feed text never
// do, and @everyone/@here are always suppressed.
// +kubebuilder:validation:MinProperties=1
type Mentions struct {
	// roleIDs are the IDs of the roles to ping.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[0-9]{1,20}$`
	// +kubebuilder:validation:items:MaxLength=20
	RoleIDs []string `json:"roleIDs,omitempty"`

	// userIDs are the IDs of the users to ping.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[0-9]{1,20}$`
	// +kubebuilder:validation:items:MaxLength=20
	UserIDs []string `json:"userIDs,omitempty"`
}

// DefaultMessageContent is the content template used when neither the feed
// nor the group sets a message.
const DefaultMessageContent = "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"

// MessageSpec describes the Discord message sent for each entry: text
// content, an embed, or both. A feed's message replaces the group's as a
// whole; the two are never merged. With neither set, the content is
// DefaultMessageContent.
// +kubebuilder:validation:XValidation:rule="has(self.content) || has(self.embed)",message="a message needs content, embed, or both"
// +kubebuilder:validation:XValidation:rule="!has(self.embed) || !has(self.suppressLinkPreviews) || !self.suppressLinkPreviews",message="suppressLinkPreviews would also hide the embed; it can only be used without one"
type MessageSpec struct {
	// content is a template for the message text. Placeholders: {{.Title}},
	// {{.Description}}, {{.Content}}, {{.Link}}, {{.Image}}, {{.Published}},
	// {{.PublishedUnix}}, {{.Author}}, {{.Categories}} and {{.FeedTitle}};
	// helper functions: truncate, default, lower, upper, trim, replace and
	// discordTime. Sent alongside the embed when both are set.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:MinLength=1
	Content string `json:"content,omitempty"`

	// embed renders the entry as a Discord embed.
	// +optional
	Embed *EmbedSpec `json:"embed,omitempty"`

	// mentions are the roles and users each message pings. The mentions are
	// put at the start of the message text.
	// +optional
	Mentions *Mentions `json:"mentions,omitempty"`

	// silent posts messages without sending push or desktop notifications
	// (Discord's @silent).
	// +default=false
	// +optional
	Silent bool `json:"silent,omitempty"`

	// suppressLinkPreviews stops Discord expanding links in the message text
	// into previews. It cannot be combined with embed, which it would hide.
	// +default=false
	// +optional
	SuppressLinkPreviews bool `json:"suppressLinkPreviews,omitempty"`
}

// ThreadTarget posts into an existing thread: a thread in a text channel, or
// a post in a forum or media channel.
type ThreadTarget struct {
	// id is the thread's (or forum post's) Discord ID.
	// +required
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	ID string `json:"id"`
}

// ForumPostSpec creates a new post for each entry, for a webhook that belongs
// to a forum or media channel.
type ForumPostSpec struct {
	// name is a template for the post's title, with the same placeholders as
	// MessageSpec.Content (e.g. "{{.Title}}").
	// +required
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// appliedTagIDs are the IDs of the channel's forum tags to put on each
	// post.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=5
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[0-9]{1,20}$`
	// +kubebuilder:validation:items:MaxLength=20
	AppliedTagIDs []string `json:"appliedTagIDs,omitempty"`
}

// FeedSpec defines the configuration for a single feed.
// +kubebuilder:validation:XValidation:rule="!has(self.thread) || !has(self.forumPost)",message="thread and forumPost are mutually exclusive"
type FeedSpec struct {
	// name identifies this feed within the group: status, delivery history
	// and metrics are keyed by it, so a feed's url can change without losing
	// its history. A DNS-1123 label.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// url is the RSS or Atom feed to fetch. Only http:// and https:// are
	// supported.
	// +required
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// suspend stops processing this feed. Its status and delivery history
	// are kept.
	// +default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// filter selects which of this feed's entries are delivered.
	// +optional
	Filter *Filter `json:"filter,omitempty"`

	// message replaces the group's message for this feed. It is not merged
	// with the group's: fields it leaves unset are not inherited.
	// +optional
	Message *MessageSpec `json:"message,omitempty"`

	// username overrides the group's username for this feed.
	// +optional
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.lowerAscii().contains('clyde') && !self.lowerAscii().contains('discord') && !self.contains('@') && !self.contains('#') && !self.contains(':') && !self.contains('```') && !(self.lowerAscii() in ['everyone', 'here'])",message="Discord rejects usernames containing \"clyde\", \"discord\", \"@\", \"#\", \":\" or \"```\", and the names \"everyone\" and \"here\""
	Username string `json:"username,omitempty"`

	// avatarURL overrides the group's avatarURL for this feed.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	AvatarURL string `json:"avatarURL,omitempty"`

	// thread posts this feed's messages into an existing thread or forum
	// post. Mutually exclusive with forumPost.
	// +optional
	Thread *ThreadTarget `json:"thread,omitempty"`

	// forumPost creates a new forum post per entry, for a forum or media
	// channel's webhook. Mutually exclusive with thread.
	// +optional
	ForumPost *ForumPostSpec `json:"forumPost,omitempty"`
}

// FeedGroupSpec defines the desired state of a FeedGroup.
type FeedGroupSpec struct {
	// discordWebhookSecretRef points to the Secret containing the Discord webhook URL.
	// The secret must contain the webhook URL under the specified key.
	// +required
	DiscordWebhookSecretRef corev1.SecretKeySelector `json:"discordWebhookSecretRef"`

	// suspend stops processing every feed in the group. Status and delivery
	// history are kept.
	// +default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// interval is the duration between feed checks (e.g., "30m", "1h"),
	// parsed by Go's time.ParseDuration. Must be at least 1m: a shorter
	// interval would requeue the FeedGroup in a tight reconcile loop,
	// hammering every feed host on each pass.
	// +default="30m"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m')",message="interval must be at least 1m"
	// +optional
	Interval string `json:"interval,omitempty"`

	// message is the Discord message sent for each entry, for every feed
	// that doesn't set its own. Defaults to content DefaultMessageContent.
	// +optional
	Message *MessageSpec `json:"message,omitempty"`

	// username overrides the webhook's default display name. A feed's
	// username overrides this one.
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.lowerAscii().contains('clyde') && !self.lowerAscii().contains('discord') && !self.contains('@') && !self.contains('#') && !self.contains(':') && !self.contains('```') && !(self.lowerAscii() in ['everyone', 'here'])",message="Discord rejects usernames containing \"clyde\", \"discord\", \"@\", \"#\", \":\" or \"```\", and the names \"everyone\" and \"here\""
	// +optional
	Username string `json:"username,omitempty"`

	// avatarURL overrides the webhook's default avatar. A feed's avatarURL
	// overrides this one.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	AvatarURL string `json:"avatarURL,omitempty"`

	// retries is how many consecutive transient failures of a feed's fetch or
	// send are counted, the first included, before they are reported as
	// persistent (a RetriesExhausted Event): each one below the limit is
	// retried after retryInterval, and from the limit on the feed is retried
	// only at the normal interval. So 3 means two quick retries, and 0 and 1
	// both mean none. A pointer, like catchUpLimit, so an explicit 0 survives
	// typed clients' encoding instead of being replaced by the default.
	// +default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Retries *int32 `json:"retries,omitempty"`

	// retryInterval is the duration between retries (e.g., "5m"), parsed by
	// Go's time.ParseDuration. Must be at least 10s: a shorter interval
	// would requeue the FeedGroup in a tight reconcile loop, hammering every
	// feed host on each pass.
	// +default="5m"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s')",message="retryInterval must be at least 10s"
	// +optional
	RetryInterval string `json:"retryInterval,omitempty"`

	// feeds is the list of feeds in this group, keyed by name. At most 50
	// feeds per group, so a single FeedGroup (and its status object) stays
	// bounded; a reconcile fetches at most 10 of them at once. URLs must also
	// be unique within a group, so one feed is never fetched and delivered
	// twice.
	// +required
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self.all(f, self.exists_one(g, g.url == f.url))",message="each feed's url must be unique within a FeedGroup"
	Feeds []FeedSpec `json:"feeds"`

	// catchUpLimit caps how many entries are sent to Discord when a feed has
	// no usable watermark: the first time it is synced (no recorded
	// LastSeenEntry), or when its recorded LastSeenEntry has scrolled out of
	// the feed's window. A normal fetch that finds the watermark is never
	// capped. Without this, adding a feed with a long history floods the
	// webhook with every existing entry at once. Entries beyond the limit are
	// treated as already seen and are not sent.
	//
	// Set to 0 to send no backlog: the first non-empty sync records the feed's
	// newest entry and only entries published afterwards are delivered. A feed
	// that was empty on its first sync has no watermark yet, so with 0
	// everything that appears later is delivered uncapped; if a recorded
	// watermark later scrolls out of the window, 0 falls back to the default
	// cap of 5. Omitting the field uses the default of 5.
	// +default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	CatchUpLimit *int32 `json:"catchUpLimit,omitempty"`
}

// FeedStatus is the observed state of a single feed within a FeedGroup,
// keyed by Name. Consolidating what used to be seven parallel
// map[string]... fields on FeedGroupStatus (all keyed by the same URL) into
// one struct per feed means a feed's full health -- timestamps, retry
// count, and *why* it's failing -- reads as one coherent block in `kubectl
// get feedgroup -o yaml` instead of requiring cross-referencing seven maps
// by hand.
type FeedStatus struct {
	// conditions report this feed's health along the two independent
	// stages a delivery passes through: FeedConditionTypeReachable (was the
	// feed itself fetchable) and FeedConditionTypeDelivered (did rendering
	// and sending to Discord succeed). Each carries a machine-readable
	// Reason (e.g. "HTTP404", "Timeout", "WebhookInvalid") set by
	// internal/controller/classify.go, so a feed stuck on a permanent 404
	// is distinguishable at a glance from a misconfigured webhook, without
	// parsing LastError's free text.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// name is the feed's FeedSpec.Name. Acts as the list-map key.
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// url is the feed URL this status was last synced from. When a feed's
	// url changes, its delivery history is kept but its etag and
	// lastModified are cleared, since they belong to the old URL.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url,omitempty"`

	// lastChecked is the last time this feed was successfully checked.
	// A 304 or a fetch with no new entries still counts as a successful check;
	// a failed fetch does not advance it.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// initialized records that this feed has been fetched successfully with a
	// usable configuration, even if it had no entries, so entries that appear
	// later are new rather than an initial backlog. It is left false when a
	// filter or template error stops processing, and reads as false on
	// statuses written before this field existed.
	// +optional
	Initialized bool `json:"initialized,omitempty"`

	// lastSeenEntry is the last seen entry identifier (GUID or Link) for
	// this feed. Used to fetch only new entries after restarts. Identifiers
	// longer than the field limit are stored as "sha256:<hex digest>".
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	LastSeenEntry string `json:"lastSeenEntry,omitempty"`

	// lastSent maps the identity of each entry recently sent to Discord to
	// when it was sent, so an entry is never sent twice. It is replaced as a
	// whole on every status write, so keys the controller prunes disappear.
	// +optional
	// +mapType=atomic
	// +kubebuilder:validation:MinProperties=1
	LastSent map[string]metav1.Time `json:"lastSent,omitempty"`

	// lastError is the last error message encountered for this feed, from
	// whichever of fetch/render/send most recently failed. See Conditions
	// for a structured, machine-readable breakdown of which stage failed
	// and why. Truncated to fit the field limit.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	LastError string `json:"lastError,omitempty"`

	// etag is the ETag header from this feed's last fetch. Sent back as
	// If-None-Match on the next fetch so an unchanged feed costs a 304
	// response instead of a full re-download and re-parse.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:MinLength=1
	ETag string `json:"etag,omitempty"`

	// lastModified is the Last-Modified header from this feed's last fetch.
	// Sent back as If-Modified-Since on the next fetch, alongside ETag.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:MinLength=1
	LastModified string `json:"lastModified,omitempty"`

	// retryCount is the number of consecutive retries since this feed last
	// succeeded.
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`

	// backoffUntil is a timestamp before which this feed should not be fetched
	// again. Set on permanent fetch failures using exponential backoff starting
	// from Spec.RetryInterval, capped at 6h.
	// +optional
	BackoffUntil *metav1.Time `json:"backoffUntil,omitempty"`
}

// FeedGroupStatus defines the observed state of a FeedGroup.
type FeedGroupStatus struct {
	// conditions represent the latest available observations of the FeedGroup's
	// overall state, following the standard Kubernetes condition conventions.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// feeds is the observed status of every feed in Spec.Feeds, keyed by
	// name.
	// +optional
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	Feeds []FeedStatus `json:"feeds,omitempty" patchStrategy:"merge" patchMergeKey:"name"`

	// observedGeneration is the most recent generation observed for this FeedGroup
	// by the controller.
	// +optional
	//nolint:kubeapilinter // plain int64 matches the universal k8s status convention (Deployment, StatefulSet, ...); not worth deviating from for one field.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// webhookSecretResourceVersion is the resourceVersion of the webhook
	// Secret the controller last read. When the Secret changes (e.g. a deleted
	// webhook is replaced), feeds backed off after a permanent send failure
	// get an immediate fresh attempt instead of waiting out their backoff.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	WebhookSecretResourceVersion string `json:"webhookSecretResourceVersion,omitempty"`
}

// ConditionTypeReady indicates whether the FeedGroup's most recent
// reconciliation completed without errors across all of its feeds.
const ConditionTypeReady = "Ready"

// ConditionTypeFeedReachable indicates whether every feed in the FeedGroup
// was reachable on its last fetch attempt.
const ConditionTypeFeedReachable = "FeedReachable"

// FeedConditionTypeReachable indicates whether a single feed (FeedStatus)
// was reachable on its last fetch attempt.
const FeedConditionTypeReachable = "Reachable"

// FeedConditionTypeDelivered indicates whether a single feed's (FeedStatus)
// last attempted entry was successfully rendered and sent to Discord.
const FeedConditionTypeDelivered = "Delivered"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=feedgroups,scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reachable",type="string",JSONPath=".status.conditions[?(@.type=='FeedReachable')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='FeedReachable')].reason"
// +kubebuilder:printcolumn:name="Suspended",type="boolean",JSONPath=".spec.suspend"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// FeedGroup is the Schema for the feedgroups API.
type FeedGroup struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the FeedGroup.
	// +required
	Spec FeedGroupSpec `json:"spec"`

	// status is the observed state of the FeedGroup.
	// +optional
	//nolint:kubeapilinter // controller-authored field; status starts as a zero-value FeedGroupStatus{} before the first reconcile populates it.
	Status FeedGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FeedGroupList contains a list of FeedGroup.
type FeedGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FeedGroup `json:"items"`
}
