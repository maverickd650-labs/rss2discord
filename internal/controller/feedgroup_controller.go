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
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// defaultCatchUpLimit is the cap used when a watermark has scrolled out of a
// feed's window, and by limitCatchUp for any non-positive limit. On a true
// first sync an explicit catchUpLimit of 0 means "skip the backlog" instead
// (see skipBacklog); an omitted field is defaulted to 5 by the API server.
const defaultCatchUpLimit = 5

// maxConcurrentFetches bounds how many feed fetches a single reconcile runs
// in parallel. Feeds.MaxItems caps the list at 50 (see FeedGroupSpec), but
// without this a FeedGroup at that cap would still open 50 simultaneous
// outbound connections every reconcile.
const maxConcurrentFetches = 10

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
		return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, retryRequeue(&feedGroup.Spec), err)
	}

	if webhookURL == "" {
		err = fmt.Errorf("discord webhook URL is empty")
		log.Error(err, "invalid webhook secret")
		return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, retryRequeue(&feedGroup.Spec), err)
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

	// An unparsable Interval is reported by requeueWithStatus below; the plan
	// only uses it for the due check.
	plan := planFeeds(&feedGroup, time.Now().UTC(), specChanged)
	activeFeeds := plan.active

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

	decision := plan.requeue(&feedGroup.Spec, wantRetry, discordRetryAfter)
	return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, decision, nil)
}

// processFeed handles a completed fetch for a single feed: it records the
// fetch outcome, applies the catch-up policy when no usable watermark exists,
// sends new entries that pass the filter, and updates the feed's FeedStatus. now is an
// RFC3339 status timestamp. Fetch, configuration, rendering, and delivery
// errors are recorded in status. Legacy identities are migrated before the
// watermark lookup. For non-empty fetches, Initialized is set only once the
// feed's configuration is known to be usable, so a config error on a first fetch
// can't turn the next attempt into a "feed was empty" delivery of its whole
// backlog. An ambiguous legacy status additionally waits until the fetch is
// accounted for (nothing pending) before it is initialized and settled. It
// returns whether the reconcile should retry sooner than the normal interval
// and, if Discord rate limited the webhook, how long to wait before the next
// attempt.
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

	// Settles what a status written before stateVersion 1 still needs from
	// the legacy reading below; see legacyState.
	legacy := readLegacyState(fs)
	defer legacy.finish(fs)

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

	// Initialized is set only once the feed's configuration is known to be
	// usable (below, or on the paths that never reach it): marking it earlier
	// would let a config error on a non-empty first fetch leave Initialized
	// true with no watermark, so a fixed config would deliver the whole
	// backlog despite catchUpLimit 0.
	wasInitialized := fs.Initialized

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
		// A 304 repeats a fetch an ambiguous legacy status can't account
		// for, so it settles nothing.
		if !legacy.ambiguous {
			fs.Initialized = true
		}
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
		// Nothing to send, so it's always safe to persist. An empty feed
		// also settles an ambiguous legacy status: anything after it is new.
		fs.Initialized = true
		legacy.settled = true
		persistValidators()
		fs.LastError = ""
		fs.RetryCount = 0
		fs.BackoffUntil = ""
		return false, 0
	}

	// Every config check happens in one step, before Initialized is set below,
	// so a config error on a non-empty first fetch can't turn the next
	// attempt into a "feed was empty" delivery of its whole backlog.
	renderer, err := newFeedRenderer(feedGroup, &feed)
	if err != nil {
		log.Error(err, "invalid feed render configuration", "url", feed.RSSUrl)
		r.recordConfigError(feedGroup, fs, feed.RSSUrl, err)
		return false, 0
	}

	// An ambiguous legacy status is capped in memory only (see below), so it
	// is settled once the fetch is accounted for, not here: settling it now
	// would let a failed first send leave a current, watermark-less status
	// that no longer gets the cap, and the next attempt would deliver the
	// whole backlog.
	if !legacy.ambiguous {
		fs.Initialized = true
		legacy.settled = true
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

	catchUpLimit := defaultCatchUpLimit
	if feedGroup.Spec.CatchUpLimit != nil {
		catchUpLimit = int(*feedGroup.Spec.CatchUpLimit)
	}
	legacy.migrate(fs, entries, catchUpLimit)
	migratePreviousIdentities(fs, entries)

	// Built over the whole fetch, before any catch-up trim below, so an
	// entry's stripped identity is only unique if it is across the feed.
	ledger := newDeliveryLedger(fs, entries)
	lastSeenID := fs.LastSeenEntry
	hasSeenID := lastSeenID != ""
	resumeAt := 0
	if hasSeenID {
		index, found := ledger.resume(entries)
		hasSeenID = found
		resumeAt = index + 1
	}
	if lastSeenID != "" && !hasSeenID {
		// The stored ID has scrolled out of the feed's window (or the feed
		// changed how it identifies this entry between fetches), so the
		// forward-scan below would never match it and would silently treat
		// every entry as already sent, forever. Fall back to catch-up
		// instead of going permanently silent.
		hasSeenID = false
	}
	if !hasSeenID {
		if catchUpLimit == 0 && lastSeenID == "" {
			// catchUpLimit: 0 with no watermark at all. On a true first sync
			// (not yet initialized) that means "don't post the backlog":
			// start from the newest entry. If the feed was already fetched
			// while empty, everything in it was published after that first
			// sync, so all of it is delivered rather than capped as a backlog.
			// A watermark that merely scrolled out of the feed's window is
			// not a first sync, so it falls through to limitCatchUp's default
			// cap rather than silently dropping new entries. An ambiguous
			// legacy status is capped the same way (see legacyState).
			switch {
			case legacy.ambiguous:
				log.Info("Capped catch-up for a FeedStatus written before stateVersion 1",
					"url", feed.RSSUrl, "limit", defaultCatchUpLimit)
				entries = limitCatchUp(entries, defaultCatchUpLimit)
			case !wasInitialized:
				skipBacklog(feedGroup, ledger, entries)
				entries = nil
			}
		} else {
			entries = limitCatchUp(entries, catchUpLimit)
		}
	}

	var pending bool
	wantRetry, rateLimitRetryAfter, pending = r.sendNewEntries(ctx, feedGroup, fs, renderer, entries, ledger, resumeAt,
		discordClient, now)

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
		fs.Initialized = true
		legacy.settled = true
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

// sendNewEntries scans entries in order, starting at resumeAt (the index just
// past the watermark, from ledger.resume; 0 when there is none), sending each
// not-yet-sent entry that passes the filter and updates fs (feed's FeedStatus)
// as it goes.
// entries must be sorted oldest-to-newest; now is an RFC3339 send timestamp.
// ledger must have been built over the entire fetch before any catch-up trimming.
// Rendering and delivery failures update status and stop the scan unless a
// render failure qualifies for skipping. The results indicate whether to retry
// sooner, the rate-limit wait, and whether an unresolved entry remains pending
// even if its retries are exhausted.
func (r *FeedGroupReconciler) sendNewEntries(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	fs *v1alpha1.FeedStatus,
	renderer *feedRenderer,
	entries []rss.Entry,
	ledger *deliveryLedger,
	resumeAt int,
	discordClient *discord.Client,
	now string,
) (wantRetry bool, rateLimitRetryAfter time.Duration, pending bool) {
	log := logf.FromContext(ctx)
	feed := renderer.feed

	for _, entry := range entries[resumeAt:] {
		if ledger.sent(entry) {
			continue
		}

		if !renderer.matches(entry) {
			continue
		}

		discordMessage, overflow, err := renderer.build(entry)
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
			if fs.RetryCount > maxRetries && renderer.otherEntryRenders(entries, entry) {
				// Retries were exhausted a full reconcile ago and the entry
				// still can't render, yet a sibling entry renders fine with
				// the same templates: the problem is this entry's content, it
				// never will render, and holding the watermark here would
				// block every newer entry behind it forever. Drop it and carry
				// on. If no entry renders, the templates themselves are broken
				// and skipping would just drain the feed, so keep holding.
				r.skipEntry(ctx, feedGroup, fs, *feed, entry, ledger, "RenderFailed", err, now)
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

		// Recorded per send attempt, before the outcome is known, so a retried
		// entry counts again; renders that are never sent (see
		// otherEntryRenders) are not observed.
		if overflow > 0 {
			messageOverflowChars.WithLabelValues(feedGroup.Namespace, feedGroup.Name).Observe(float64(overflow))
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

		ledger.markHandled(entry, now)
		fs.LastError = ""
		fs.RetryCount = 0
		fs.BackoffUntil = ""
		setFeedCondition(fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue,
			"MessageSent", "Entry rendered and delivered to Discord", feedGroup.Generation)
		feedOperationsTotal.WithLabelValues(feedGroup.Namespace, feedGroup.Name, feed.RSSUrl, outcomeSent).Inc()
	}

	return wantRetry, rateLimitRetryAfter, pending
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
	ledger *deliveryLedger,
	reason string,
	cause error,
	now string,
) {
	identity := entryIdentity(entry)
	logf.FromContext(ctx).Error(cause, "skipping entry after exhausting retries", "url", feed.RSSUrl, "entry", identity)

	ledger.markHandled(entry, now)
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

// applyPermanentBackoff sets fs.BackoffUntil using exponential backoff
// (permanentBackoffDuration, based on fs.RetryCount, which the caller has
// already incremented for this failure), capped at maxPermanentBackoff. Shared
// by the fetch- and send-permanent-failure paths so a feed/entry that can't
// recover on its own backs off the same way instead of retrying at full
// Interval cadence forever. reason/err identify the Warning Event fired the
// first time the cap is reached -- not on every capped retry after, so a feed
// that keeps failing at the cap doesn't emit an Event every few hours.
func (r *FeedGroupReconciler) applyPermanentBackoff(feedGroup *v1alpha1.FeedGroup, fs *v1alpha1.FeedStatus, rssURL, reason string, err error) {
	base, _ := parseDurationWithDefault(feedGroup.Spec.RetryInterval, fallbackRetryInterval)
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

// skipBacklog records the newest of entries (sorted oldest-to-newest) as the
// feed's watermark without sending anything and sets Delivered to BacklogSkipped.
// entries must be non-empty; later scans resume after this entry rather than
// comparing publication times. The identity is the same one sendNewEntries would
// have stored after sending that entry, so the next forward-scan resumes
// right behind it.
func skipBacklog(feedGroup *v1alpha1.FeedGroup, ledger *deliveryLedger, entries []rss.Entry) {
	ledger.markNewest(entries)
	setFeedCondition(ledger.fs, v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue,
		reasonBacklogSkipped, fmt.Sprintf("catchUpLimit is 0: skipped %d existing entries", len(entries)), feedGroup.Generation)
}

// limitCatchUp trims entries to at most limit, keeping the most recent ones
// (entries is assumed sorted ascending by Published). A non-positive limit
// falls back to defaultCatchUpLimit rather than disabling the cap, so a feed
// can never dump its entire backlog on first reconcile. (An explicit
// catchUpLimit of 0 is handled earlier, by skipBacklog, on a true first
// sync only.)
func limitCatchUp(entries []rss.Entry, limit int) []rss.Entry {
	if limit <= 0 {
		limit = defaultCatchUpLimit
	}
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
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
