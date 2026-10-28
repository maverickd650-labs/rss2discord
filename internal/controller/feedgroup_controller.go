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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	neturl "net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	acv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
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

// maxPermanentBackoff is the ceiling for per-feed exponential backoff on
// permanent fetch failures (e.g. HTTP 404). Once the computed backoff would
// exceed this, the feed keeps being retried at this cadence indefinitely:
// "permanent" classifications (a 403 from a WAF blip, a 404 during a site
// migration) are often only long-lived, and a few requests a day is a cheap
// price for a feed that recovers on its own without a spec edit.
const maxPermanentBackoff = 6 * time.Hour

// maxLastSentPerFeed bounds how many sent-entry hashes are retained per feed
// in Status.LastSent. Without a cap this map grows by one key per sent
// message forever, bloating the FeedGroup status subresource indefinitely.
//
// The cap is kept deliberately modest because the whole status object lives in
// etcd, which has a ~1MB object soft limit: at Feeds.MaxItems (50) this cap
// bounds LastSent at 50*maxLastSentPerFeed {64-hex-hash: RFC3339} pairs, so the
// product is the real budget to watch, not this number alone. LastSent is only
// a dedup backstop -- LastSeenEntry is the primary watermark -- so it just needs
// to cover the entries currently in a feed's window (typically 10-50), not its
// whole history.
const maxLastSentPerFeed = 50

// defaultCatchUpLimit is used when FeedGroupSpec.CatchUpLimit is unset
// (zero), which happens both for an explicit 0 and for CRDs applied before
// the field existed (the kubebuilder default only takes effect through the
// API server's defaulting webhook/CRD schema).
const defaultCatchUpLimit = 5

// maxDiscordMessageLength is Discord's hard cap on webhook message content;
// the API rejects anything longer. Full article bodies in a feed's
// description can easily exceed this once stripped to plain text.
const maxDiscordMessageLength = 2000

// maxConcurrentFetches bounds how many feed fetches a single reconcile runs
// in parallel. Feeds.MaxItems caps the list at 50 (see FeedGroupSpec), but
// without this a FeedGroup at that cap would still open 50 simultaneous
// outbound connections every reconcile.
const maxConcurrentFetches = 10

// maxStatusTextBytes bounds free-text status fields (FeedStatus.LastError,
// LastSeenEntry and condition messages). FeedGroup.Status lives in a single
// etcd object (~1.5MB hard limit) and holds up to 50 feeds, so unbounded
// error strings or identifiers copied from feeds/servers could otherwise push
// it over and make every status write fail. Byte-based, so it also satisfies
// the CRD's character-based MaxLength.
const maxStatusTextBytes = 2048

// maxConditionMessageBytes is the tighter cap for per-feed condition
// messages, which duplicate LastError and are stored twice per feed.
const maxConditionMessageBytes = 1024

// clampText truncates s to at most max bytes without splitting a UTF-8
// sequence, marking the cut with an ellipsis.
func clampText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// errText is err's message bounded for storage in FeedStatus.LastError.
func errText(err error) string {
	return clampText(err.Error(), maxStatusTextBytes)
}

// clampWatermark returns id unchanged when it fits in FeedStatus.LastSeenEntry,
// otherwise a stable digest of it. Applied on both the store and compare
// sides, so a long identity still matches itself across reconciles.
func clampWatermark(id string) string {
	if len(id) <= maxStatusTextBytes {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FeedGroupReconciler reconciles a FeedGroup object
type FeedGroupReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	RSSClient            *rss.Client
	DiscordClientBuilder func(webhookURL string) *discord.Client
	Recorder             events.EventRecorder
	// MaxConcurrentReconciles bounds how many FeedGroups this controller
	// reconciles in parallel. Each reconcile only does outbound HTTP I/O
	// (RSS fetch, Discord send) against a FeedGroup-local status snapshot,
	// so multiple FeedGroups can safely reconcile concurrently; left at the
	// zero value, controller-runtime defaults this to 1, preserving the
	// historical single-worker behavior.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *FeedGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("feedgroup", req.NamespacedName)
	ctx = logf.IntoContext(ctx, log)

	reconcileStart := time.Now()

	var feedGroup v1alpha1.FeedGroup
	if err := r.Get(ctx, req.NamespacedName, &feedGroup); err != nil {
		if apierrors.IsNotFound(err) {
			// The FeedGroup is gone; drop its metric series so a deleted
			// group can't leave a stale feedLastSuccessTimestamp behind.
			deleteFeedGroupMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get FeedGroup")
		return ctrl.Result{}, err
	}

	// Timed from here, not from the top of Reconcile, so a deleted FeedGroup
	// never re-creates its just-dropped feedGroupReconcileDuration series via
	// a deferred Observe running after deleteFeedGroupMetrics above.
	defer func() {
		feedGroupReconcileDuration.WithLabelValues(req.Namespace, req.Name).Observe(time.Since(reconcileStart).Seconds())
	}()

	ensureFeedStatuses(&feedGroup)

	// A spec change (generation bump) lets a feed in permanent backoff retry
	// right away instead of waiting out its BackoffUntil. Clear BackoffUntil
	// and RetryCount for every feed so the new generation gets a fresh retry
	// cycle; without resetting RetryCount the first retry after clearing
	// BackoffUntil would compute a backoff of base*2^oldRetryCount, which
	// would immediately jump back to maxPermanentBackoff.
	specChanged := feedGroup.Generation > feedGroup.Status.ObservedGeneration
	if specChanged {
		clearPermanentBackoffs(&feedGroup)
	}

	// Snapshot status after ensureFeedStatuses above (which only normalizes
	// existing state) but before anything that reflects this reconcile's
	// outcome, so requeueWithStatus can skip the API write entirely when
	// nothing actually changed.
	statusSnapshot := feedGroup.Status.DeepCopy()

	webhookURL, err := r.resolveWebhookURL(ctx, &feedGroup)
	if err != nil {
		log.Error(err, "failed to resolve Discord webhook URL")
		return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, feedGroup.Spec.RetryInterval, 5*time.Minute, err, true)
	}

	if webhookURL == "" {
		err = fmt.Errorf("discord webhook URL is empty")
		log.Error(err, "invalid webhook secret")
		return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, feedGroup.Spec.RetryInterval, 5*time.Minute, err, true)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rssClient := r.RSSClient
	if rssClient == nil {
		rssClient = rss.NewClient(nil)
	}
	discordBuilder := r.DiscordClientBuilder
	if discordBuilder == nil {
		discordBuilder = discord.NewClient
	}
	discordClient := discordBuilder(webhookURL)

	reconcileNow := time.Now().UTC()
	// An unparsable Interval is reported by requeueWithStatus below; here it
	// only feeds the due check, so fall back rather than bail out early.
	pollInterval, _ := parseDurationWithDefault(feedGroup.Spec.Interval, 30*time.Minute)
	var nextDue time.Duration // soonest a feed skipped as not-yet-due becomes due
	activeFeeds := make([]v1alpha1.FeedSpec, 0, len(feedGroup.Spec.Feeds))
	for _, feed := range feedGroup.Spec.Feeds {
		if feed.RSSUrl == "" || feed.Paused {
			continue
		}
		// Skip feeds that are in permanent-failure backoff. The skip happens
		// here, before the fetch fan-out, so backed-off feeds emit no metrics
		// and incur no outbound HTTP calls during their backoff window.
		fs := feedStatusFor(&feedGroup, feed.RSSUrl)
		if fs != nil && feedInBackoff(fs.BackoffUntil, reconcileNow) {
			continue
		}
		// Reconciles are also triggered by things other than the interval
		// timer (operator restart, leader failover, retry cadence driven by a
		// sibling feed), so don't refetch a healthy feed that was checked
		// less than one interval ago.
		if fs != nil && !feedDue(fs, pollInterval, reconcileNow, specChanged) {
			// Not due yet: make sure the requeue below doesn't sleep past
			// the moment it will be (e.g. after a restart mid-interval).
			if last, err := time.Parse(time.RFC3339, fs.LastChecked); err == nil {
				if until := last.Add(pollInterval).Sub(reconcileNow); nextDue == 0 || until < nextDue {
					nextDue = until
				}
			}
			continue
		}
		activeFeeds = append(activeFeeds, feed)
	}

	// Feeds are independent network fetches, so fetch them concurrently
	// rather than paying each feed's fetch timeout sequentially. A semaphore
	// caps how many run at once, since a FeedGroup at the Feeds.MaxItems
	// limit would otherwise open that many simultaneous outbound connections
	// every reconcile. Sending and status-map updates below remain
	// single-threaded, so no locking is needed for the rest of the
	// reconcile. Reading (not writing) feedGroup.Status here is safe
	// concurrently with itself, since nothing mutates it until after
	// wg.Wait().
	fetched := make([]rss.FetchResult, len(activeFeeds))
	fetchErrs := make([]error, len(activeFeeds))
	sem := make(chan struct{}, maxConcurrentFetches)
	var wg sync.WaitGroup
	for i, feed := range activeFeeds {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			fs := feedStatusFor(&feedGroup, feed.RSSUrl)
			validators := rss.CacheValidators{
				ETag:         fs.ETag,
				LastModified: fs.LastModified,
			}
			start := time.Now()
			fetched[i], fetchErrs[i] = rssClient.FetchEntries(ctx, feed.RSSUrl, validators)
			feedRequestDuration.WithLabelValues(feedGroup.Namespace, feedGroup.Name, operationFetch).
				Observe(time.Since(start).Seconds())
		})
	}
	wg.Wait()

	wantRetry := false
	var discordRetryAfter time.Duration
	for i, feed := range activeFeeds {
		// All feeds in a group share one webhook, so once any feed hits
		// Discord's per-webhook rate limit, processing the rest would only
		// fire more sends that 429 too. Stop here and let the requeue (which
		// backs off for discordRetryAfter below) retry the remaining feeds.
		if discordRetryAfter > 0 {
			break
		}
		retry, rateLimitRetryAfter := r.processFeed(ctx, &feedGroup, feed, fetched[i], fetchErrs[i], discordClient, now)
		if retry {
			wantRetry = true
		}
		if rateLimitRetryAfter > discordRetryAfter {
			discordRetryAfter = rateLimitRetryAfter
		}
	}

	if discordRetryAfter > 0 {
		return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, "", discordRetryAfter, nil, false)
	}

	interval := feedGroup.Spec.Interval
	fallback := 30 * time.Minute
	if wantRetry {
		interval = feedGroup.Spec.RetryInterval
		fallback = 5 * time.Minute
	}

	result, err := r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, interval, fallback, nil, true)
	if err == nil && nextDue > 0 && nextDue < result.RequeueAfter {
		result.RequeueAfter = max(nextDue, minParsedInterval)
	}
	return result, err
}

// processFeed fetches/filters/sends entries for a single feed and updates
// its FeedStatus accordingly. It returns whether the reconcile should retry
// sooner than the normal interval, and, if Discord rate limited the
// webhook, how long to wait before the next attempt.
func (r *FeedGroupReconciler) processFeed(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	feed v1alpha1.FeedSpec,
	fetchResult rss.FetchResult,
	fetchErr error,
	discordClient *discord.Client,
	now string,
) (wantRetry bool, rateLimitRetryAfter time.Duration) {
	log := logf.FromContext(ctx)
	fs := feedStatusFor(feedGroup, feed.RSSUrl)

	if fetchErr != nil {
		// LastChecked deliberately doesn't advance here: it tracks the last
		// *successful* check, so a feed stuck failing shows an aging
		// LastChecked rather than looking freshly checked every retry.
		log.Error(fetchErr, "failed to fetch RSS feed", "url", feed.RSSUrl)
		class := classifyFetchError(fetchErr)
		fs.LastError = errText(fetchErr)
		fs.RetryCount++
		feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, fetchErrorOutcome(class)).Inc()
		setFeedCondition(fs, v1alpha1.FeedConditionTypeReachable, metav1.ConditionFalse,
			class.conditionReason, fetchErr.Error(), feedGroup.Generation)

		if class.permanent {
			// Permanent failures (404, 410, bad XML, etc.) won't self-heal on
			// the normal retry schedule, so use exponential backoff to reduce
			// polling instead of the flat RetryInterval. The group's normal
			// Interval drives other feeds; this feed handles its own timing via
			// BackoffUntil checked in the active-feeds filter. wantRetry is
			// false so a single permanent failure doesn't pull the whole group
			// onto the faster RetryInterval.
			r.applyPermanentBackoff(feedGroup, fs, feed.RSSUrl, "FetchFailed", fetchErr)
			return false, 0
		}

		maxRetries := maxRetryCount(feedGroup.Spec.Retries)
		// Fire the persistent-failure Event exactly once, the reconcile this
		// feed's retries are first exhausted -- not on every subsequent
		// reconcile of a feed stuck failing the same way. Without the == here
		// (rather than >=), RetryCount keeps climbing past maxRetries forever
		// and a single ongoing failure would otherwise generate a fresh Warning
		// Event every poll interval, indefinitely.
		if fs.RetryCount == maxRetries {
			r.recordPersistentFailure(feedGroup, feed.RSSUrl, "FetchFailed", fetchErr)
		}
		return fs.RetryCount < maxRetries, 0
	}

	// Persisting the new validators is deferred until the end of this
	// function (and skipped entirely if anything below needs a retry): if a
	// send fails, storing the new ETag now would make the next reconcile's
	// conditional GET return 304 before the unsent entry is ever retried,
	// silently dropping it.
	persistValidators := func() {
		if fetchResult.ETag != "" {
			fs.ETag = fetchResult.ETag
		}
		if fetchResult.LastModified != "" {
			fs.LastModified = fetchResult.LastModified
		}
	}

	markFetchSucceeded := func() {
		fs.LastChecked = now
		markFeedCheckSuccess(feedGroup)
		setFeedCondition(fs, v1alpha1.FeedConditionTypeReachable, metav1.ConditionTrue,
			"FetchSucceeded", "Feed fetched successfully", feedGroup.Generation)
	}

	if fetchResult.NotModified {
		// A 304 confirms the feed has no new entries, which is just as much a
		// successful check as one that found something to send -- it
		// shouldn't read as stale just because nothing changed. This does
		// give up the optimization of skipping the status write when nothing
		// else changed (requeueWithStatus diffs on the whole status), so a
		// quiet feed now costs one status write per poll interval instead of
		// zero.
		markFetchSucceeded()
		persistValidators()
		fs.LastError = ""
		fs.RetryCount = 0
		fs.BackoffUntil = ""
		return false, 0
	}

	markFetchSucceeded()

	entries := fetchResult.Entries
	// Stamp the feed's title on each entry so templates can use {{.FeedTitle}}
	// without threading it through sendNewEntries/buildDiscordMessage.
	for i := range entries {
		entries[i].FeedTitle = fetchResult.FeedTitle
	}
	if len(entries) == 0 {
		// Nothing to send, so it's always safe to persist.
		persistValidators()
		fs.LastError = ""
		fs.RetryCount = 0
		fs.BackoffUntil = ""
		return false, 0
	}

	filterRegex, err := compileFilterRegex(feed.Filter)
	if err != nil {
		log.Error(err, "invalid filter regex", "url", feed.RSSUrl)
		r.recordConfigError(feedGroup, fs, feed.RSSUrl, err)
		return false, 0
	}

	embedSpec := resolveEmbedSpec(feedGroup, &feed)

	var contentTmpl, descriptionTmpl, threadNameTmpl *template.Template
	if embedSpec != nil && embedSpec.Enabled {
		descriptionTmpl, err = compileTemplate("discordEmbedDescription", embedSpec.DescriptionFormat, defaultDescriptionFormat)
	} else {
		contentTmpl, err = compileMessageTemplate(feedGroup, &feed)
	}
	if err != nil {
		log.Error(err, "invalid Discord message template", "url", feed.RSSUrl)
		r.recordConfigError(feedGroup, fs, feed.RSSUrl, err)
		return false, 0
	}

	if strings.TrimSpace(feed.ForumThreadName) != "" {
		threadNameTmpl, err = compileTemplate("discordThreadName", feed.ForumThreadName, "")
		if err != nil {
			log.Error(err, "invalid Discord forum thread name template", "url", feed.RSSUrl)
			r.recordConfigError(feedGroup, fs, feed.RSSUrl, err)
			return false, 0
		}
	}

	// Sort oldest-to-newest so the catch-up window (limitCatchUp keeps the
	// tail) and the watermark (sendNewEntries advances LastSeenEntry to the
	// last entry it sends) both track the *newest* entries. Published is the
	// primary key, but many feeds omit per-entry dates, leaving every entry
	// with a zero timestamp; in that case fall back to document order, where a
	// lower Seq is newer (feeds list newest-first). Tie-break so a lower Seq
	// sorts later (toward the newest tail) -- without this, date-less feeds
	// would catch-up on their oldest entries and park the watermark there,
	// then never deliver anything newer.
	slices.SortStableFunc(entries, func(a, b rss.Entry) int {
		if c := a.Published.Compare(b.Published); c != 0 {
			return c
		}
		return cmp.Compare(b.Seq, a.Seq)
	})

	lastSeenID := fs.LastSeenEntry
	hasSeenID := lastSeenID != ""
	if hasSeenID && !entriesContainID(entries, lastSeenID) {
		// The stored ID has scrolled out of the feed's window (or the feed
		// changed how it identifies this entry between fetches), so the
		// forward-scan below would never match it and would silently treat
		// every entry as already sent, forever. Fall back to catch-up
		// instead of going permanently silent.
		hasSeenID = false
	}
	if !hasSeenID {
		entries = limitCatchUp(entries, int(feedGroup.Spec.CatchUpLimit))
	}

	var pending bool
	wantRetry, rateLimitRetryAfter, pending = r.sendNewEntries(ctx, feedGroup, fs, feed, entries, hasSeenID, lastSeenID,
		filterRegex, embedSpec, contentTmpl, descriptionTmpl, threadNameTmpl, discordClient, now)

	pruneLastSent(fs.LastSent, maxLastSentPerFeed)

	// Only persist now that every entry from this fetch was either sent or
	// filtered out. pending (not wantRetry) is the right gate here: wantRetry
	// turns false once an entry's retries are exhausted or it enters
	// permanent backoff, even though the entry itself is still unsent.
	// Persisting validators at that point would let the next fetch's
	// conditional GET come back 304 -- which skips re-parsing entries
	// entirely -- and silently strand the unresolved entry forever. Keeping
	// the old validators instead forces a full re-fetch (and another attempt
	// at this entry) on every reconcile until it's actually resolved.
	if !pending {
		persistValidators()
		// Every entry from this fetch was either sent (which already clears
		// these below in sendNewEntries) or skipped as already-sent/filtered.
		// A feed stuck on a prior failure that recovers but happens to have
		// nothing new and unfiltered to send would otherwise keep showing the
		// stale LastError/RetryCount forever, since neither branch above runs
		// for an all-skipped fetch -- the FeedGroup's Ready condition would
		// stay False on a feed that's actually healthy again.
		fs.LastError = ""
		fs.RetryCount = 0
		fs.BackoffUntil = ""
	}

	return wantRetry, rateLimitRetryAfter
}

// sendNewEntries scans entries in order, skipping forward past lastSeenID
// (if hasSeenID is set) before sending anything, then sends each not-yet-sent
// entry that passes the filter and updates fs (feed's FeedStatus) as it goes.
func (r *FeedGroupReconciler) sendNewEntries(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	fs *v1alpha1.FeedStatus,
	feed v1alpha1.FeedSpec,
	entries []rss.Entry,
	hasSeenID bool,
	lastSeenID string,
	filterRegex *regexp.Regexp,
	embedSpec *v1alpha1.EmbedSpec,
	contentTmpl, descriptionTmpl, threadNameTmpl *template.Template,
	discordClient *discord.Client,
	now string,
) (wantRetry bool, rateLimitRetryAfter time.Duration, pending bool) {
	log := logf.FromContext(ctx)
	foundLastSeen := !hasSeenID

	for _, entry := range entries {
		if hasSeenID && !foundLastSeen {
			if clampWatermark(entryIdentity(entry)) == lastSeenID {
				foundLastSeen = true
			}
			continue
		}

		identity := entryIdentity(entry)
		entryKey := hashIdentity(identity)
		if _, alreadySent := fs.LastSent[entryKey]; alreadySent {
			continue
		}

		if !matchesFilter(feed.Filter, entry, filterRegex) {
			continue
		}

		discordMessage, err := buildDiscordMessage(feedGroup, embedSpec, contentTmpl, descriptionTmpl, threadNameTmpl, &feed, entry)
		if err != nil {
			log.Error(err, "failed to render Discord message", "url", feed.RSSUrl)
			fs.LastError = errText(err)
			fs.RetryCount++
			feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, outcomeRenderError).Inc()
			setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse,
				reasonRenderError, err.Error(), feedGroup.Generation)

			// A render error is deterministic for a given entry+template, so
			// once retries are exhausted, retrying forever would just spin
			// at RetryInterval without ever making progress. Surface an
			// Event instead of looping indefinitely. The Event only fires
			// the reconcile RetryCount first reaches maxRetries (== rather
			// than >=), since this entry is reprocessed every reconcile
			// until something changes -- without the equality check, a
			// stuck template would emit a fresh Warning Event every poll
			// interval forever.
			maxRetries := maxRetryCount(feedGroup.Spec.Retries)
			if fs.RetryCount > maxRetries && otherEntryRenders(feedGroup, embedSpec, contentTmpl, descriptionTmpl, threadNameTmpl, &feed, entries, entry) {
				// Retries were exhausted a full reconcile ago and the entry
				// still can't render, yet a sibling entry renders fine with
				// the same templates: the problem is this entry's content, it
				// never will render, and holding the watermark here would
				// block every newer entry behind it forever. Drop it and carry
				// on. If no entry renders, the templates themselves are broken
				// and skipping would just drain the feed, so keep holding.
				r.skipEntry(ctx, feedGroup, fs, feed, entry, identity, entryKey, "RenderFailed", err, now)
				continue
			}
			if fs.RetryCount < maxRetries {
				wantRetry = true
			} else if fs.RetryCount == maxRetries {
				r.recordPersistentFailure(feedGroup, feed.RSSUrl, "RenderFailed", err)
			}

			// Stop processing this feed's remaining entries for this
			// reconcile rather than continuing to the next one: entries are
			// sent oldest-to-newest, and a later entry's success advances
			// LastSeenEntry (below) past everything up to it. If we kept
			// going past this failed entry, a subsequent success would push
			// the watermark past it too, and the next reconcile's forward-
			// scan would then treat it as "already handled" and never
			// attempt it again -- silently dropping it for good. Stopping
			// here, like the rate-limit branch below already does, keeps
			// the watermark from ever skipping an unresolved entry; it just
			// means later entries in the same feed wait behind this one
			// instead of jumping ahead.
			pending = true
			break
		}

		sendStart := time.Now()
		err = discordClient.SendMessage(ctx, discordMessage)
		feedRequestDuration.WithLabelValues(feedGroup.Namespace, feedGroup.Name, operationSend).
			Observe(time.Since(sendStart).Seconds())
		if err != nil {
			log.Error(err, "failed to send Discord message", "url", feed.RSSUrl)
			fs.LastError = errText(err)

			if rateLimitErr, ok := errors.AsType[*discord.RateLimitError](err); ok {
				wantRetry = true
				feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, outcomeRateLimited).Inc()
				setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse,
					reasonRateLimited, err.Error(), feedGroup.Generation)
				if rateLimitErr.RetryAfter > rateLimitRetryAfter {
					rateLimitRetryAfter = rateLimitErr.RetryAfter
				}
				// Discord rate limits are per-webhook, so once one entry is
				// limited every further POST this reconcile would 429 too.
				// Stop sending the rest of this feed's entries; the unsent ones
				// stay pending (validators aren't persisted below) and are
				// retried after the requeue backs off for RetryAfter.
				pending = true
				break
			} else {
				class := classifySendError(err)
				fs.RetryCount++
				feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, sendErrorOutcome(class)).Inc()
				setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse,
					class.conditionReason, err.Error(), feedGroup.Generation)

				// See the matching comment on the render-error branch above:
				// fire the persistent-failure Event only the reconcile
				// RetryCount first reaches maxRetries, not on every
				// subsequent reconcile of an entry stuck failing to send.
				maxRetries := maxRetryCount(feedGroup.Spec.Retries)
				if fs.RetryCount < maxRetries {
					wantRetry = true
				} else {
					if fs.RetryCount == maxRetries {
						r.recordPersistentFailure(feedGroup, feed.RSSUrl, "SendFailed", err)
					}
					// A permanent classification (e.g. a deleted webhook, or
					// a malformed message Discord will keep rejecting) won't
					// recover on its own, so back off exponentially the same
					// way a permanent fetch failure does instead of retrying
					// at full Interval cadence forever. A transient
					// classification (5xx, timeout) keeps retrying at that
					// cadence since it's expected to recover on its own.
					if class.permanent {
						r.applyPermanentBackoff(feedGroup, fs, feed.RSSUrl, "SendFailed", err)
					}
				}
			}
			// See the matching comment on the render-error branch above:
			// stop here rather than continuing to the next entry, so a later
			// entry's success can't advance LastSeenEntry past this unsent
			// one and silently skip it for good.
			pending = true
			break
		}

		fs.LastSent[entryKey] = now
		fs.LastSeenEntry = clampWatermark(identity)
		fs.LastError = ""
		fs.RetryCount = 0
		fs.BackoffUntil = ""
		setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue,
			"MessageSent", "Entry rendered and delivered to Discord", feedGroup.Generation)
		feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, outcomeSent).Inc()
	}

	return wantRetry, rateLimitRetryAfter, pending
}

// otherEntryRenders reports whether any entry other than skip renders
// successfully with the feed's templates, which distinguishes "this entry is
// the problem" from "the template is broken for everything".
func otherEntryRenders(
	feedGroup *v1alpha1.FeedGroup,
	embedSpec *v1alpha1.EmbedSpec,
	contentTmpl, descriptionTmpl, threadNameTmpl *template.Template,
	feed *v1alpha1.FeedSpec,
	entries []rss.Entry,
	skip rss.Entry,
) bool {
	for _, other := range entries {
		if entryIdentity(other) == entryIdentity(skip) {
			continue
		}
		if _, err := buildDiscordMessage(feedGroup, embedSpec, contentTmpl, descriptionTmpl, threadNameTmpl, feed, other); err == nil {
			return true
		}
	}
	return false
}

// skipEntry gives up on a single entry whose retries are exhausted, so the
// rest of the feed can proceed: it is recorded as handled (dedup key and
// watermark) and the retry state is reset for the entry after it. The skip is
// surfaced via a Warning Event, a metric outcome, and the Delivered condition
// rather than LastError, which would otherwise mark the feed failing forever.
func (r *FeedGroupReconciler) skipEntry(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	fs *v1alpha1.FeedStatus,
	feed v1alpha1.FeedSpec,
	entry rss.Entry,
	identity, entryKey, reason string,
	cause error,
	now string,
) {
	logf.FromContext(ctx).Error(cause, "skipping entry after exhausting retries", "url", feed.RSSUrl, "entry", identity)

	fs.LastSent[entryKey] = now
	fs.LastSeenEntry = clampWatermark(identity)
	fs.LastError = ""
	fs.RetryCount = 0
	fs.BackoffUntil = ""
	feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, outcomeSkipped).Inc()
	setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse,
		reasonEntrySkipped, fmt.Sprintf("skipped entry %q after %s: %v", clampText(entry.Title, 100), reason, cause), feedGroup.Generation)
	if r.Recorder != nil {
		r.Recorder.Eventf(feedGroup, nil, corev1.EventTypeWarning, reasonEntrySkipped, "SkippedEntry",
			"feed %s: skipped entry %q after exhausting retries (%s): %v", feed.RSSUrl, clampText(entry.Title, 100), reason, cause)
	}
}

// markFeedCheckSuccess records that a feed was successfully checked this
// reconcile, whether or not it had anything new to send. It's the single
// place that advances feedLastSuccessTimestamp, so a quiet-but-healthy feed
// (304, or zero new entries) reads as fresh rather than as if the operator
// had stopped checking it.
func markFeedCheckSuccess(feedGroup *v1alpha1.FeedGroup) {
	feedLastSuccessTimestamp.WithLabelValues(feedGroup.Namespace, feedGroup.Name).
		Set(float64(time.Now().Unix()))
}

// applyPermanentBackoff sets fs.BackoffUntil using exponential backoff
// (permanentBackoffDuration, based on fs.RetryCount, which the caller has
// already incremented for this failure), capped at maxPermanentBackoff. Shared
// by the fetch- and send-permanent-failure paths so a feed/entry that can't
// recover on its own backs off the same way instead of retrying at full
// Interval cadence forever. reason/err identify the Warning Event fired the
// first time the cap is reached -- not on every capped retry after, so a feed
// that keeps failing at the cap doesn't emit an Event every few hours.
func (r *FeedGroupReconciler) applyPermanentBackoff(feedGroup *v1alpha1.FeedGroup, fs *v1alpha1.FeedStatus, rssURL, reason string, err error) {
	base, _ := parseDurationWithDefault(feedGroup.Spec.RetryInterval, 5*time.Minute)
	backoff := permanentBackoffDuration(int(fs.RetryCount), base)
	if backoff >= maxPermanentBackoff {
		firstCapped := fs.RetryCount <= 1 ||
			permanentBackoffDuration(int(fs.RetryCount)-1, base) < maxPermanentBackoff
		if firstCapped {
			r.recordPersistentFailure(feedGroup, rssURL, reason, err)
		}
		backoff = maxPermanentBackoff
	}
	fs.BackoffUntil = nextBackoffUntil(time.Now().UTC(), backoff)
}

// recordPersistentFailure emits a Warning Event on feedGroup once a feed's
// fetch or send retries are exhausted, so the failure is visible via
// `kubectl describe feedgroup` instead of requiring a controller log dive.
// It's a no-op if Recorder is unset (e.g. in unit tests that construct the
// reconciler directly rather than through SetupWithManager).
func (r *FeedGroupReconciler) recordPersistentFailure(feedGroup *v1alpha1.FeedGroup, url, reason string, err error) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(feedGroup, nil, corev1.EventTypeWarning, reason, "RetriesExhausted",
		"feed %s: giving up after exhausting retries: %v", url, err)
}

// recordConfigError updates fs to reflect a deterministic FeedSpec
// misconfiguration (an invalid filter regex or message/thread-name
// template): these won't be fixed by retrying, so unlike a fetch/send
// failure they carry no RetryCount escalation to gate a one-time Event on.
// setFeedCondition's changed return serves the same purpose instead: it's
// true only the reconcile the Delivered condition first flips to
// ConfigError (or its message changes, e.g. a different typo), not on every
// subsequent reconcile of the same unresolved error.
func (r *FeedGroupReconciler) recordConfigError(feedGroup *v1alpha1.FeedGroup, fs *v1alpha1.FeedStatus, url string, err error) {
	fs.LastError = errText(err)
	changed := setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse,
		reasonConfigError, err.Error(), feedGroup.Generation)
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(feedGroup, nil, corev1.EventTypeWarning, reasonConfigError, "InvalidConfig",
			"feed %s: %v", url, err)
	}
}

func (r *FeedGroupReconciler) resolveWebhookURL(ctx context.Context, feedGroup *v1alpha1.FeedGroup) (string, error) {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      feedGroup.Spec.DiscordWebhookSecretRef.Name,
		Namespace: feedGroup.Namespace,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		return "", err
	}

	value, ok := secret.Data[feedGroup.Spec.DiscordWebhookSecretRef.Key]
	if !ok {
		return "", fmt.Errorf("secret %s missing key %q", secretKey.Name, feedGroup.Spec.DiscordWebhookSecretRef.Key)
	}

	return strings.TrimSpace(string(value)), nil
}

// ensureFeedStatuses rebuilds feedGroup.Status.Feeds to have exactly one
// entry per feed in feedGroup.Spec.Feeds, in spec order: existing entries are
// matched up by RSSUrl and carried over untouched, new feeds get a
// zero-valued entry, and entries for feeds no longer in the spec are
// dropped. This both initializes status on a fresh FeedGroup and prunes
// stale entries left behind by an edited/removed feed (the same
// unbounded-growth problem maxLastSentPerFeed guards against one level
// down) in a single pass -- there's only one place left that needs to know
// about every per-feed status field, instead of the seven parallel
// map[string]... fields this used to require initializing and pruning in
// lockstep. Paused feeds still get an entry, since they're still part of the
// spec; rebuilding in spec order also means Status.Feeds' order is stable
// across reconciles whenever the spec doesn't change, which
// apiequality.Semantic.DeepEqual (used by requeueWithStatus to skip no-op
// writes) needs to avoid seeing a reordered-but-otherwise-identical slice as
// a change. It also drops the rss_url-labeled metric series of any feed it
// prunes (see deleteFeedMetrics).
func ensureFeedStatuses(feedGroup *v1alpha1.FeedGroup) {
	existing := make(map[string]v1alpha1.FeedStatus, len(feedGroup.Status.Feeds))
	for _, fs := range feedGroup.Status.Feeds {
		existing[fs.RSSUrl] = fs
	}

	rebuilt := make([]v1alpha1.FeedStatus, 0, len(feedGroup.Spec.Feeds))
	for _, feed := range feedGroup.Spec.Feeds {
		fs, ok := existing[feed.RSSUrl]
		if !ok {
			fs = v1alpha1.FeedStatus{RSSUrl: feed.RSSUrl}
		}
		if fs.LastSent == nil {
			fs.LastSent = map[string]string{}
		}
		// Normalize values stored before these fields were bounded; an
		// oversized one would be rejected by the CRD schema on every write.
		fs.LastError = clampText(fs.LastError, maxStatusTextBytes)
		fs.LastSeenEntry = clampWatermark(fs.LastSeenEntry)
		rebuilt = append(rebuilt, fs)
		delete(existing, feed.RSSUrl)
	}
	feedGroup.Status.Feeds = rebuilt

	// Whatever is left in existing was dropped from the spec. Drop its
	// rss_url-labeled series too, so editing a group's feed list doesn't
	// leave stale series behind until the whole FeedGroup is deleted.
	for url := range existing {
		deleteFeedMetrics(feedGroup.Namespace, feedGroup.Name, url)
	}
}

// feedStatusFor returns the FeedStatus for url, which must already exist
// (ensureFeedStatuses runs once at the start of every Reconcile and creates
// an entry for every feed in Spec.Feeds, so any url drawn from Spec.Feeds is
// guaranteed to be found).
func feedStatusFor(feedGroup *v1alpha1.FeedGroup, url string) *v1alpha1.FeedStatus {
	for i := range feedGroup.Status.Feeds {
		if feedGroup.Status.Feeds[i].RSSUrl == url {
			return &feedGroup.Status.Feeds[i]
		}
	}
	return nil
}

// setFeedCondition is apimeta.SetStatusCondition scoped to a single feed's
// Conditions, so call sites read as "what happened" rather than repeating
// the metav1.Condition literal at every one of the half-dozen places a
// fetch/render/send outcome needs to update Reachable or Delivered. Returns
// whether the condition was newly added or changed (status/reason/message),
// so a caller with no RetryCount to gate on (e.g. a config error, which
// isn't retried) can still fire a one-time Event on the transition into that
// state instead of every reconcile it recurs.
func setFeedCondition(fs *v1alpha1.FeedStatus, condType string, status metav1.ConditionStatus, reason, message string, generation int64) bool {
	return apimeta.SetStatusCondition(&fs.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            clampText(message, maxConditionMessageBytes),
		ObservedGeneration: generation,
	})
}

// requeueJitterMaxFactor bounds the jitter added to a periodic (interval or
// retry-interval driven) requeue: up to 10% longer than the base duration,
// per wait.Jitter's contract. Without this, FeedGroups created together with
// the same interval requeue in lockstep forever -- every one of their
// fetches (and, for FeedGroups sharing a webhook, every Discord send) lands
// in the same instant, indefinitely.
const requeueJitterMaxFactor = 0.1

// requeueWithStatus persists feedGroup's status and requeues after the given
// interval (falling back to fallback if interval is unset or invalid). The
// status write (applyStatus) is skipped when the status hasn't actually
// changed from original, since a conditional-GET 304 makes the common
// reconcile a no-op that would otherwise still pay for a status write every
// interval -- a no-op server-side apply still costs an API round trip.
// webhookErr, if non-nil, means the reconcile never got far enough to
// process any feed (the webhook Secret couldn't be resolved); it takes over
// the Ready condition unconditionally, since per-feed status is stale/empty
// and would otherwise misreport as healthy.
//
// An Interval/RetryInterval that time.ParseDuration rejects (the CRD Pattern
// admits overflowing values) must never abort this function: by now messages
// may already have been sent, and returning before the status write would
// discard LastSent/LastSeenEntry/LastChecked, so every error-backoff retry
// would re-send the same entries. The requeue uses fallback instead, the
// problem is surfaced on Ready (see setReadyCondition) plus one Warning Event
// on the transition, and the result carries no error, which would only
// hot-loop the same failure through controller-runtime's backoff.
//
// jitter controls whether requeueJitterMaxFactor is applied to the computed
// duration. It must be false when fallback is itself an externally-dictated
// wait -- currently only Discord's rate-limit Retry-After -- since that
// duration is already the exact minimum wait Discord demands, not a
// self-chosen periodicity that benefits from desynchronization.
func (r *FeedGroupReconciler) requeueWithStatus(ctx context.Context, feedGroup *v1alpha1.FeedGroup, original *v1alpha1.FeedGroupStatus, interval string, fallback time.Duration, webhookErr error, jitter bool) (ctrl.Result, error) {
	// parseDurationWithDefault returns fallback alongside its error; the error
	// itself is reported via intervalErr below, for both fields at once.
	duration, _ := parseDurationWithDefault(interval, fallback)
	duration = jitterDuration(duration, jitter)

	feedGroup.Status.ObservedGeneration = feedGroup.Generation
	prevReady := apimeta.FindStatusCondition(feedGroup.Status.Conditions, v1alpha1.ConditionTypeReady)
	prevReason, prevMessage := "", ""
	if prevReady != nil {
		prevReason, prevMessage = prevReady.Reason, prevReady.Message
	}
	setReadyCondition(feedGroup, webhookErr, invalidIntervalError(&feedGroup.Spec))
	setFeedReachableCondition(feedGroup)

	// Gate the Event on the condition's reason/message actually changing (as
	// recordConfigError does for feeds), not on every reconcile that still
	// sees the bad value. ObservedGeneration is deliberately not compared, so
	// an unrelated spec edit doesn't repeat the Event.
	if ready := apimeta.FindStatusCondition(feedGroup.Status.Conditions, v1alpha1.ConditionTypeReady); r.Recorder != nil &&
		ready != nil && ready.Reason == reasonInvalidInterval &&
		(ready.Reason != prevReason || ready.Message != prevMessage) {
		r.Recorder.Eventf(feedGroup, nil, corev1.EventTypeWarning, reasonInvalidInterval, "InvalidConfig", "%s", ready.Message)
	}

	// Before the no-op short-circuit below so the per-feed gauges are refreshed
	// on every reconcile, not just ones that changed status.
	recordFeedStateMetrics(feedGroup)

	if apiequality.Semantic.DeepEqual(original, &feedGroup.Status) {
		return ctrl.Result{RequeueAfter: duration}, nil
	}

	if err := r.applyStatus(ctx, feedGroup); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: duration}, nil
}

// statusFieldOwner names this controller as the field manager for
// server-side apply status writes. It matches the event recorder's source
// name in SetupWithManager, since both identify the same actor.
const statusFieldOwner = "feedgroup-controller"

// applyStatus persists feedGroup's status via server-side apply rather than
// a whole-object Status().Update. FeedGroupStatus.Feeds is listType=map,
// listMapKey=rssUrl, so entries merge (and prune, for feeds no longer sent
// in the apply) by rssUrl instead of by slice position -- and, more broadly,
// SSA lets this controller declare itself owner of exactly the status
// fields it manages instead of unconditionally overwriting the whole
// subresource, which is what removes the conflict-retry churn Update()
// incurs once reconciles run with concurrency > 1.
//
// This controller is the sole writer of FeedGroup.Status, so ForceOwnership
// is safe here: there's no other field manager whose fields could be
// clobbered.
//
// The request body is a generated ApplyConfiguration (api/v1alpha1/
// applyconfiguration, from `mise run generate`) carrying only name,
// namespace, apiVersion/kind and status. Unlike patching with the typed
// object, it never sends managedFields or a resourceVersion, so there's
// nothing to scrub first and no optimistic-concurrency precondition to
// trip: field-ownership merging, not "did anything else change first", is
// what decides whether this write succeeds.
func (r *FeedGroupReconciler) applyStatus(ctx context.Context, feedGroup *v1alpha1.FeedGroup) error {
	status, err := statusApplyConfiguration(&feedGroup.Status)
	if err != nil {
		return err
	}
	return r.Status().Apply(ctx,
		acv1alpha1.FeedGroup(feedGroup.Name, feedGroup.Namespace).WithStatus(status),
		client.FieldOwner(statusFieldOwner), client.ForceOwnership)
}

// statusApplyConfiguration converts status into its generated
// ApplyConfiguration by round-tripping it through JSON. The two types share
// field names and json tags by construction (both come from the same
// FeedGroupStatus definition), so this carries every field across --
// including ones added later -- without a hand-maintained With*() builder
// chain that would silently drop a new status field until someone
// remembered to wire it in. The round trip also preserves omitempty exactly:
// a field the typed status omits is absent from the apply, so this manager
// releases it and the API server removes it, just as before.
func statusApplyConfiguration(status *v1alpha1.FeedGroupStatus) (*acv1alpha1.FeedGroupStatusApplyConfiguration, error) {
	raw, err := json.Marshal(status)
	if err != nil {
		return nil, fmt.Errorf("marshaling FeedGroup status: %w", err)
	}
	ac := &acv1alpha1.FeedGroupStatusApplyConfiguration{}
	if err := json.Unmarshal(raw, ac); err != nil {
		return nil, fmt.Errorf("converting FeedGroup status to an apply configuration: %w", err)
	}
	return ac, nil
}

// jitterDuration adds up to requeueJitterMaxFactor extra to duration when
// enabled is true, and returns duration unchanged otherwise. Split out from
// requeueWithStatus so it can be unit-tested without a real client.Client
// (requeueWithStatus's applyStatus call needs one).
func jitterDuration(duration time.Duration, enabled bool) time.Duration {
	if !enabled {
		return duration
	}
	return wait.Jitter(duration, requeueJitterMaxFactor)
}

// setReadyCondition summarizes the outcome of a reconciliation into a single
// "Ready" condition, leaving Status.Feeds (LastError, Conditions, etc.) as
// the detailed source of truth for *which* feed and *why*. webhookErr, if
// set, means the webhook Secret itself couldn't be resolved -- a
// group-level failure that preempts any per-feed status, since no feed was
// even attempted.
//
// intervalErr, if set, means spec.interval or spec.retryInterval can't be
// parsed (see invalidIntervalError) and the controller is polling on its
// default cadence instead. It ranks below webhookErr -- with no feed
// processed, the webhook failure is the more fundamental problem and its
// retry cadence is the one that matters -- but above the per-feed summary:
// the spec is wrong no matter how the feeds fared, and until it's fixed the
// group is not running on the schedule the user asked for. The InvalidInterval
// condition clears on its own the reconcile after the spec is corrected.
func setReadyCondition(feedGroup *v1alpha1.FeedGroup, webhookErr, intervalErr error) {
	if webhookErr != nil {
		apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "WebhookUnresolved",
			Message:            webhookErr.Error(),
			ObservedGeneration: feedGroup.Generation,
		})
		return
	}

	if intervalErr != nil {
		apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             reasonInvalidInterval,
			Message:            intervalErr.Error(),
			ObservedGeneration: feedGroup.Generation,
		})
		return
	}

	status := metav1.ConditionTrue
	reason := "Reconciled"
	message := "All feeds processed successfully"

	failing := countFailingFeeds(feedGroup.Status.Feeds)
	if failing > 0 {
		status = metav1.ConditionFalse
		reason = "FeedErrors"
		message = fmt.Sprintf("%d feed(s) reporting errors", failing)
	}

	apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: feedGroup.Generation,
	})
}

// reasonInvalidInterval is the Ready condition reason (and Warning Event
// reason) for a spec.interval/spec.retryInterval that time.ParseDuration
// rejects.
const reasonInvalidInterval = "InvalidInterval"

// invalidIntervalError reports the first of spec.interval and
// spec.retryInterval that parseDurationWithDefault rejects, or nil. It checks
// both fields regardless of which one this reconcile happens to requeue on, so
// the Ready condition doesn't flap between the normal and retry paths (and
// re-fire its Event each time) while only one of them is bad. The CRD Pattern
// admits values like "9999999999999h" that overflow time.ParseDuration, and
// objects admitted under an older schema may hold others.
func invalidIntervalError(spec *v1alpha1.FeedGroupSpec) error {
	for _, field := range []struct{ name, value string }{
		{"interval", spec.Interval},
		{"retryInterval", spec.RetryInterval},
	} {
		if _, err := parseDurationWithDefault(field.value, 0); err != nil {
			return fmt.Errorf("spec.%s: %w; using the default instead", field.name, err)
		}
	}
	return nil
}

// countFailingFeeds reports how many feeds currently have a non-empty
// LastError.
func countFailingFeeds(feeds []v1alpha1.FeedStatus) int {
	count := 0
	for _, fs := range feeds {
		if fs.LastError != "" {
			count++
		}
	}
	return count
}

// setFeedReachableCondition reports whether every feed in feedGroup was
// reachable on its last fetch attempt, with Reason set to the classification
// (see classify.go) of the most common fetch failure across feeds -- e.g.
// "HTTP404" for a group with several feeds stuck on a persistent 404. This
// is narrower than Ready: it only reflects each feed's Reachable condition
// (fetch failures), not Discord send/render failures (Delivered), so a
// feed-side outage is distinguishable from a webhook misconfiguration.
func setFeedReachableCondition(feedGroup *v1alpha1.FeedGroup) {
	unreachable := map[string]string{}
	for _, fs := range feedGroup.Status.Feeds {
		cond := apimeta.FindStatusCondition(fs.Conditions, v1alpha1.FeedConditionTypeReachable)
		if cond != nil && cond.Status == metav1.ConditionFalse {
			unreachable[fs.RSSUrl] = cond.Reason
		}
	}

	if len(unreachable) == 0 {
		apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionTypeFeedReachable,
			Status:             metav1.ConditionTrue,
			Reason:             "AllFeedsReachable",
			Message:            "All feeds were reachable on their last check",
			ObservedGeneration: feedGroup.Generation,
		})
		return
	}

	reason := dominantErrorReason(unreachable)
	apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
		Type:   v1alpha1.ConditionTypeFeedReachable,
		Status: metav1.ConditionFalse,
		Reason: reason,
		Message: fmt.Sprintf("%d feed(s) unreachable; most common reason: %s",
			len(unreachable), reason),
		ObservedGeneration: feedGroup.Generation,
	})
}

// dominantErrorReason returns the most frequent value in reasons (a map of
// feed URL to classification Reason), so a FeedGroup with several feeds
// failing differently still gets one representative Reason on its aggregate
// condition rather than an arbitrary one. Ties are broken by sort order
// (rather than Go's randomized map iteration) so the result -- and thus
// whether the condition's Message actually changed -- is deterministic
// across reconciles of the same status, which requeueWithStatus relies on to
// skip no-op status writes.
func dominantErrorReason(reasons map[string]string) string {
	counts := make(map[string]int, len(reasons))
	for _, reason := range reasons {
		counts[reason]++
	}

	keys := slices.Sorted(maps.Keys(counts))

	best := keys[0]
	for _, key := range keys[1:] {
		if counts[key] > counts[best] {
			best = key
		}
	}
	return best
}

// trackingQueryParams lists query-string parameters added by analytics or
// sharing tools that commonly get appended, rotated, or dropped between
// fetches of an otherwise-unchanged article (FeedBurner/WordPress are
// common sources). They're stripped in normalizeIdentity so churn limited
// to these parameters doesn't make an unchanged article look new.
var trackingQueryParams = map[string]bool{
	"fbclid":  true,
	"gclid":   true,
	"mc_eid":  true,
	"mc_cid":  true,
	"igshid":  true,
	"ref_src": true,
	"ref":     true,
	"mkt_tok": true,
	"_hsenc":  true,
	"_hsmi":   true,
	"spm":     true,
}

// schemeHTTP/schemeHTTPS are the only URL schemes normalizeIdentity and
// httpURLOrEmpty treat as a URL rather than opaque text.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// normalizeIdentity normalizes id when it's an http(s) URL (an entry's ID
// is frequently just its permalink, since the RSS/Atom parser falls back to
// Link, then Title, when a feed has no GUID): lowercases the scheme/host,
// drops the fragment, and strips known tracking query parameters, so that
// churn limited to those doesn't change the result. Anything that isn't a
// URL (an opaque GUID, or a Title used as a last-resort identity) is
// returned unchanged, since there's no generically safe way to normalize
// arbitrary text.
func normalizeIdentity(id string) string {
	// Opaque GUIDs and title fallbacks are common and can't be URLs we
	// normalize, so skip the (allocating) parse for anything that doesn't
	// start with an http(s) scheme.
	if len(id) < 7 || !strings.EqualFold(id[:4], schemeHTTP) {
		return id
	}
	parsed, err := neturl.Parse(id)
	if err != nil || (parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS) {
		return id
	}

	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""

	if query := parsed.Query(); len(query) > 0 {
		for param := range query {
			if trackingQueryParams[strings.ToLower(param)] || strings.HasPrefix(strings.ToLower(param), "utm_") {
				query.Del(param)
			}
		}
		parsed.RawQuery = query.Encode()
	}

	return parsed.String()
}

// entryIdentity resolves entry's stable identity, used for both the
// catch-up watermark (LastSeenEntry) and per-entry dedup (LastSent):
// entry.ID itself (already resolved via GUID -> Link -> Title precedence by
// the RSS/Atom parser), normalized when it's a URL.
func entryIdentity(entry rss.Entry) string {
	return normalizeIdentity(entry.ID)
}

// computeEntryKey hashes an entry's identity into a fixed-length key for
// Status.LastSent. It intentionally does not also fold in Link or Title the
// way an earlier version did: those commonly churn (a rotated tracking
// parameter, an edited headline) on an otherwise-unchanged article, and
// folding them into the key made every such edit look like a brand new
// entry and get re-sent as a duplicate -- defeating the point of having a
// stable GUID-based identity in the first place.
func computeEntryKey(entry rss.Entry) string {
	return hashIdentity(entryIdentity(entry))
}

// hashIdentity is computeEntryKey for an identity the caller already
// resolved, so a loop over entries doesn't normalize each URL twice.
func hashIdentity(identity string) string {
	hash := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(hash[:])
}

// compileFilterRegex compiles a feed's filter regex once per feed per
// reconcile, instead of recompiling it for every entry in the feed.
func compileFilterRegex(filter *v1alpha1.Filter) (*regexp.Regexp, error) {
	if filter == nil || strings.TrimSpace(filter.Regex) == "" {
		return nil, nil
	}

	re, err := regexp.Compile(filter.Regex)
	if err != nil {
		return nil, fmt.Errorf("invalid filter regex %q: %w", filter.Regex, err)
	}
	return re, nil
}

func matchesFilter(filter *v1alpha1.Filter, entry rss.Entry, filterRegex *regexp.Regexp) bool {
	if filter == nil {
		return true
	}

	if filterRegex != nil && !filterRegex.MatchString(entry.Title+"\n"+entry.Description) {
		return false
	}

	if len(filter.Keywords) == 0 {
		return true
	}

	content := strings.ToLower(strings.TrimSpace(entry.Title + "\n" + entry.Description))
	for _, keyword := range filter.Keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		if strings.Contains(content, strings.ToLower(keyword)) {
			return true
		}
	}

	return false
}

// compileMessageTemplate parses a feed's Discord message template once per
// feed per reconcile, instead of reparsing it for every entry sent.
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
// to fallback when text is blank.
func compileTemplate(name, text, fallback string) (*template.Template, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = fallback
	}
	return template.New(name).Parse(text)
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

// parseHexColor parses a "#RRGGBB" or "RRGGBB" string into Discord's 24-bit
// embed color integer, defaulting to 0 (black/unset) for blank or malformed
// input rather than failing the whole message.
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

// httpURLOrEmpty returns rawURL unchanged if it's a well-formed http(s) URL,
// or "" otherwise. Entry links/images come straight from feed XML, which is
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
func renderTemplate(tmpl *template.Template, entry rss.Entry, max int) (string, int, error) {
	data := templateData{
		Title:         stripHTML(entry.Title),
		Description:   stripHTML(entry.Description),
		Link:          entry.Link,
		Image:         httpURLOrEmpty(entry.Image),
		Published:     formatPublished(entry.Published),
		PublishedUnix: publishedUnix(entry.Published),
		Author:        entry.Author,
		Categories:    strings.Join(entry.Categories, ", "),
		FeedTitle:     stripHTML(entry.FeedTitle),
		content:       entry.Content,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", 0, err
	}

	content, overflow := truncateMessage(buf.String(), max)
	return content, overflow, nil
}

// buildDiscordMessage renders entry into the message that should actually
// be sent: an embed (colored bubble with title/description/thumbnail) when
// embedSpec is enabled, or plain text content otherwise. It also resolves
// forum-channel thread targeting (ForumThreadID takes precedence over a
// rendered ForumThreadName) and group-level webhook branding.
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
			Title:        title,
			Description:  description,
			URL:          httpURLOrEmpty(entry.Link),
			Color:        parseHexColor(embedSpec.Color),
			Timestamp:    formatPublished(entry.Published),
			ThumbnailURL: httpURLOrEmpty(entry.Image),
			AuthorName:   embedSpec.AuthorName,
			FooterText:   embedSpec.FooterText,
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

// pruneLastSent caps a feed's sent-entry dedup map at max entries, dropping
// the oldest (by recorded RFC3339 send timestamp, which sorts lexically;
// ties broken by key) first. Without this, LastSent grows by one key per sent message forever.
func pruneLastSent(sent map[string]string, max int) {
	if len(sent) <= max {
		return
	}

	keys := slices.SortedFunc(maps.Keys(sent), func(a, b string) int {
		// Timestamps have one-second resolution, so ties are common; break
		// them on the key so the dropped subset is deterministic.
		return cmp.Or(strings.Compare(sent[a], sent[b]), strings.Compare(a, b))
	})

	for _, key := range keys[:len(keys)-max] {
		delete(sent, key)
	}
}

// entriesContainID reports whether id matches any entry's identity (see
// entryIdentity).
func entriesContainID(entries []rss.Entry, id string) bool {
	for _, entry := range entries {
		if clampWatermark(entryIdentity(entry)) == id {
			return true
		}
	}
	return false
}

// limitCatchUp trims entries to at most limit, keeping the most recent ones
// (entries is assumed sorted ascending by Published). A non-positive limit
// falls back to defaultCatchUpLimit rather than disabling the cap, so a feed
// can never dump its entire backlog on first reconcile.
func limitCatchUp(entries []rss.Entry, limit int) []rss.Entry {
	if limit <= 0 {
		limit = defaultCatchUpLimit
	}
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
}

// minParsedInterval floors any interval/retryInterval this parses, whether
// from FeedGroup spec or already-persisted status. The CRD's Pattern rejects
// a negative literal, but a tiny positive one ("1ns") still validates and,
// fed straight into ctrl.Result{RequeueAfter: ...}, would requeue in a tight
// loop that hammers every feed host on each pass; this floor is the backstop
// for that, and also for objects admitted under an older, less strict schema.
const minParsedInterval = 10 * time.Second

func parseDurationWithDefault(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback, fmt.Errorf("invalid duration %q: %w", value, err)
	}

	return max(duration, minParsedInterval), nil
}

func maxRetryCount(specRetries int32) int32 {
	if specRetries < 1 {
		return 1
	}
	return specRetries
}

// feedDue reports whether fs should be fetched this reconcile. A feed is due
// when its spec just changed (force), it has never been checked, it has an
// outstanding failure (those follow the RetryInterval cadence the group's
// requeue already encodes), or a full interval has elapsed since its last
// successful check. An unparsable LastChecked counts as due.
func feedDue(fs *v1alpha1.FeedStatus, interval time.Duration, now time.Time, force bool) bool {
	if force || fs.LastChecked == "" || fs.LastError != "" || fs.RetryCount > 0 {
		return true
	}
	last, err := time.Parse(time.RFC3339, fs.LastChecked)
	if err != nil {
		return true
	}
	return !now.Before(last.Add(interval))
}

// feedInBackoff reports whether backoffUntil (a FeedStatus.BackoffUntil
// RFC3339 timestamp) is still in the future relative to now. A malformed or
// empty backoffUntil is treated as "not in backoff" so a bad value can never
// permanently wedge a feed without a spec change.
func feedInBackoff(backoffUntil string, now time.Time) bool {
	if backoffUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, backoffUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// nextBackoffUntil formats the RFC3339 timestamp for a feed's next permanent
// backoff expiry, backoff duration past now.
func nextBackoffUntil(now time.Time, backoff time.Duration) string {
	return now.Add(backoff).Format(time.RFC3339)
}

// permanentBackoffDuration returns the exponential backoff for a permanent
// fetch failure at retryCount (1-based, already incremented before call).
// The formula is base * 2^retryCount, capped at maxPermanentBackoff. Callers
// check whether the result >= maxPermanentBackoff to decide whether to set the
// sentinel vs. a concrete timestamp.
func permanentBackoffDuration(retryCount int, base time.Duration) time.Duration {
	if retryCount < 1 {
		retryCount = 1
	}
	// Avoid overflow: time.Duration is int64, so 1<<63 wraps negative.
	// Guard the shift itself first, then check whether base*shift exceeds
	// the cap before multiplying -- otherwise large retryCount values
	// silently produce zero or negative durations.
	if retryCount >= 63 {
		return maxPermanentBackoff
	}
	shift := time.Duration(1 << uint(retryCount))
	if base <= 0 || shift > maxPermanentBackoff/base {
		return maxPermanentBackoff
	}
	return base * shift
}

// clearPermanentBackoffs resets BackoffUntil and RetryCount on every feed in
// the group. Called when the FeedGroup spec generation advances, so an edit
// gives feeds in backoff an immediate fresh attempt. RetryCount is reset
// alongside BackoffUntil because leaving it at its old value would cause the
// next permanentBackoffDuration call to compute a backoff that immediately
// jumps to maxPermanentBackoff on the very first retry after the edit.
func clearPermanentBackoffs(feedGroup *v1alpha1.FeedGroup) {
	for i := range feedGroup.Status.Feeds {
		feedGroup.Status.Feeds[i].BackoffUntil = ""
		feedGroup.Status.Feeds[i].RetryCount = 0
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FeedGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("feedgroup-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Status is written by this controller, so reacting to those writes
		// would re-enqueue every reconcile that changed anything (and, for a
		// failing feed whose RetryCount ticks up each pass, loop at
		// API-round-trip speed instead of RetryInterval). Only spec changes
		// (generation bumps), creates and deletes matter; periodic work is
		// driven by RequeueAfter.
		For(&v1alpha1.FeedGroup{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("feedgroup").
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
			// controller-runtime v0.24 already defaults UsePriorityQueue to
			// true, but the queue is dominated by periodic RequeueAfter
			// items (every FeedGroup's interval/retry-interval), so pinning
			// this explicitly documents that fresh spec-change events are
			// meant to jump ahead of that backlog rather than wait behind
			// hundreds of routine retries -- and keeps that guarantee if the
			// upstream default ever changes.
			UsePriorityQueue: new(true),
		}).
		Complete(r)
}
