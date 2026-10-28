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
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Filter defines filtering rules for RSS feed entries.
// +kubebuilder:validation:MinProperties=1
type Filter struct {
	// regex is a regular expression to match against entry title/description.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:MinLength=1
	Regex string `json:"regex,omitempty"`

	// keywords is a list of keywords to match (OR logic).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MaxLength=200
	// +kubebuilder:validation:items:MinLength=1
	Keywords []string `json:"keywords,omitempty"`
}

// EmbedImageStyleImage is the EmbedSpec.ImageStyle value that shows an
// entry's image large, below the description. Any other value (including
// unset) shows it as a small thumbnail.
const EmbedImageStyleImage = "image"

// EmbedSpec configures Discord's native embed rendering (the colored
// "bubble" with title, description, thumbnail, author and footer) instead
// of a plain text message.
// +kubebuilder:validation:MinProperties=1
type EmbedSpec struct {
	// enabled switches a feed's messages from plain text content to a
	// Discord embed.
	// +default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// color is the embed's side-bar color, as a hex string (e.g. "#5865F2"
	// or "5865F2").
	// +kubebuilder:validation:Pattern=`^#?[0-9a-fA-F]{6}$`
	// +kubebuilder:validation:MaxLength=7
	// +kubebuilder:validation:MinLength=1
	// +optional
	Color string `json:"color,omitempty"`

	// descriptionFormat is the template used to render the embed's
	// description. Supports the same placeholders as Format. Defaults to
	// "{{.Description}}".
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:MinLength=1
	DescriptionFormat string `json:"descriptionFormat,omitempty"`

	// imageStyle chooses how an entry's image is shown: "thumbnail" (a small
	// image beside the description, the default) or "image" (a large image
	// below the description).
	// +kubebuilder:validation:Enum=thumbnail;image
	// +optional
	ImageStyle string `json:"imageStyle,omitempty"`

	// authorName is shown on the embed's author line.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:MinLength=1
	AuthorName string `json:"authorName,omitempty"`

	// footerText is shown in the embed's footer.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	FooterText string `json:"footerText,omitempty"`
}

// FeedSpec defines the configuration for a single RSS feed.
type FeedSpec struct {
	// rssUrl is the URL of the RSS feed to fetch. Only http:// and https:// are supported.
	// +required
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	RSSUrl string `json:"rssUrl"`

	// filter defines how to filter entries from this feed.
	// +optional
	Filter *Filter `json:"filter,omitempty"`

	// format is the template for Discord messages for this feed.
	// Overrides the group-level format if set.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:MinLength=1
	Format string `json:"format,omitempty"`

	// embed configures Discord embed rendering for this feed. Overrides the
	// group-level Embed config if set.
	// +optional
	Embed *EmbedSpec `json:"embed,omitempty"`

	// forumThreadName creates a new forum post for each message, named from
	// this template, when DiscordWebhookSecretRef points at a forum
	// channel's webhook. Supports the same placeholders as Format. Leave
	// unset for regular text channels.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:MinLength=1
	ForumThreadName string `json:"forumThreadName,omitempty"`

	// forumThreadID posts messages into an existing forum thread/post
	// instead of creating a new one. Takes precedence over ForumThreadName.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	ForumThreadID string `json:"forumThreadID,omitempty"`

	// paused stops processing this feed if set to true.
	// +default=false
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// FeedGroupSpec defines the desired state of a FeedGroup.
type FeedGroupSpec struct {
	// discordWebhookSecretRef points to the Secret containing the Discord webhook URL.
	// The secret must contain the webhook URL under the specified key.
	// +required
	DiscordWebhookSecretRef corev1.SecretKeySelector `json:"discordWebhookSecretRef"`

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

	// format is the default template for Discord messages.
	// Supports placeholders: {{.Title}}, {{.Description}}, {{.Link}}, {{.Published}}, {{.Author}}, {{.Categories}}.
	// +default="**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:MinLength=1
	Format string `json:"format,omitempty"`

	// embed configures Discord embed rendering, used as the default for all
	// feeds in this group unless a feed sets its own Embed.
	// +optional
	Embed *EmbedSpec `json:"embed,omitempty"`

	// username overrides the webhook's default display name for messages
	// sent from this group. Discord rejects names containing "clyde" or
	// "discord" (case-insensitive) or over 80 characters, so both
	// constraints are enforced here at admission rather than surfacing only
	// as a runtime send failure.
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="!self.lowerAscii().contains('clyde') && !self.lowerAscii().contains('discord')",message="username must not contain \"clyde\" or \"discord\" (Discord rejects these)"
	// +optional
	Username string `json:"username,omitempty"`

	// avatarURL overrides the webhook's default avatar for messages sent
	// from this group.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https?://`
	AvatarURL string `json:"avatarURL,omitempty"`

	// retries is the number of times to retry failed operations (fetch/send).
	// +default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	Retries int32 `json:"retries,omitempty"`

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

	// feeds is the list of RSS feed configurations in this group. Capped at
	// maxConcurrentFetches (see feedgroup_controller.go) so a single
	// reconcile can't fan out an unbounded number of simultaneous outbound
	// fetches. RSS URLs must be unique within a group, since all per-feed
	// status (LastSeenEntry, LastSent, ETag, ...) is keyed by URL: two feeds
	// sharing a URL would silently clobber each other's status.
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self.all(f, self.exists_one(g, g.rssUrl == f.rssUrl))",message="each feed's rssUrl must be unique within a FeedGroup"
	Feeds []FeedSpec `json:"feeds"`

	// catchUpLimit caps how many backlog entries are sent to Discord the
	// first time a feed is reconciled (i.e. before it has a recorded
	// LastSeenEntry). Without this, adding a feed with a long history
	// floods the webhook with every existing entry at once. Entries beyond
	// the limit are treated as already seen and are not sent.
	// +default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	CatchUpLimit int32 `json:"catchUpLimit,omitempty"`
}

// FeedStatus is the observed state of a single feed within a FeedGroup,
// keyed by RSSUrl. Consolidating what used to be seven parallel
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

	// rssUrl identifies which feed (in FeedGroupSpec.Feeds) this status
	// corresponds to. Acts as the list-map merge key.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	RSSUrl string `json:"rssUrl"`

	// lastChecked is the last time this feed was successfully checked
	// (RFC3339 timestamp). A 304 or a fetch with no new entries still
	// counts as a successful check; a failed fetch does not advance it.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:MinLength=1
	LastChecked string `json:"lastChecked,omitempty"`

	// lastSeenEntry is the last seen entry identifier (GUID or Link) for
	// this feed. Used to fetch only new entries after restarts. Identifiers
	// longer than the field limit are stored as "sha256:<hex digest>".
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:MinLength=1
	LastSeenEntry string `json:"lastSeenEntry,omitempty"`

	// lastSent is a map of entry hash to timestamp (RFC3339), tracking
	// which entries have been sent to Discord to avoid duplicates.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	LastSent map[string]string `json:"lastSent,omitempty"`

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

	// backoffUntil is an RFC3339 timestamp before which this feed should not
	// be fetched again. Set on permanent fetch failures (e.g. HTTP 404) using
	// exponential backoff starting from Spec.RetryInterval, capped at 6h
	// (the feed keeps being retried at that cadence); cleared on the next
	// successful fetch or when the FeedGroup spec changes. An empty
	// string means the feed is not in backoff.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:MinLength=1
	BackoffUntil string `json:"backoffUntil,omitempty"`
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
	// RSSUrl.
	// +optional
	// +patchMergeKey=rssUrl
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=rssUrl
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:MinItems=1
	Feeds []FeedStatus `json:"feeds,omitempty" patchStrategy:"merge" patchMergeKey:"rssUrl"`

	// observedGeneration is the most recent generation observed for this FeedGroup
	// by the controller.
	// +optional
	//nolint:kubeapilinter // plain int64 matches the universal k8s status convention (Deployment, StatefulSet, ...); not worth deviating from for one field.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ConditionTypeReady indicates whether the FeedGroup's most recent
// reconciliation completed without errors across all of its feeds.
const ConditionTypeReady = "Ready"

// ConditionTypeFeedReachable indicates whether every feed in the FeedGroup
// was reachable on its last fetch attempt. Unlike Ready, this is scoped
// specifically to fetch failures (not Discord send/render failures), so a
// feed returning a persistent 404 is distinguishable from, say, the webhook
// being misconfigured. Its Reason summarizes the most common
// FeedConditionTypeReachable Reason across Status.Feeds.
const ConditionTypeFeedReachable = "FeedReachable"

// FeedConditionTypeReachable indicates whether a single feed (FeedStatus)
// was reachable on its last fetch attempt. Reason is one of the
// classification reasons set in internal/controller/classify.go (e.g.
// "HTTP404", "Timeout", "DNSFailure").
const FeedConditionTypeReachable = "Reachable"

// FeedConditionTypeDelivered indicates whether a single feed's (FeedStatus)
// last attempted entry was successfully rendered and sent to Discord.
// Unset (no condition of this type present) means the feed hasn't attempted
// a delivery yet -- e.g. it has never had a new entry. Reason is one of the
// classification reasons set in internal/controller/classify.go, or
// "RenderError" for a template failure.
const FeedConditionTypeDelivered = "Delivered"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=feedgroups,scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reachable",type="string",JSONPath=".status.conditions[?(@.type=='FeedReachable')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='FeedReachable')].reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// FeedGroup is the Schema for the feedgroups API.
type FeedGroup struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of the FeedGroup.
	// +optional
	Spec FeedGroupSpec `json:"spec,omitempty"`

	// status is the observed state of the FeedGroup.
	// +optional
	//nolint:kubeapilinter // controller-authored field; status starts as a zero-value FeedGroupStatus{} before the first reconcile populates it, so a MinProperties constraint risks rejecting the controller's own status writes.
	Status FeedGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FeedGroupList contains a list of FeedGroup.
type FeedGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FeedGroup `json:"items"`
}
