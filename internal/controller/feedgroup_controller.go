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
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

// maxConcurrentFetches bounds how many feed fetches a single reconcile runs
// in parallel. Feeds.MaxItems caps the list at 50 (see FeedGroupSpec), but
// without this a FeedGroup at that cap would still open 50 simultaneous
// outbound connections every reconcile.
const maxConcurrentFetches = 10

// FeedGroupReconciler reconciles a FeedGroup object
type FeedGroupReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	RSSClient            FeedFetcher
	DiscordClientBuilder func(webhookURL string) MessageSender
	Recorder             events.EventRecorder
	// MaxConcurrentReconciles bounds how many FeedGroups this controller
	// reconciles in parallel. Each reconcile only does outbound HTTP I/O
	// (RSS fetch, Discord send) against a FeedGroup-local status snapshot,
	// so multiple FeedGroups can safely reconcile concurrently; left at the
	// zero value, controller-runtime defaults this to 1, preserving the
	// historical single-worker behavior.
	MaxConcurrentReconciles int
	// WatchWebhookSecrets wakes a FeedGroup as soon as its webhook Secret
	// changes (see noteWebhookSecretVersion), via a metadata-only Secret
	// watch. It needs list/watch on Secrets, so it is off unless that grant is
	// wanted: the Helm chart turns it on with a namespace-scoped install
	// (rbac.watchNamespaces), where the grant is scoped to those namespaces
	// too. Without it the change is still noticed, on the group's next
	// scheduled reconcile, with only get on Secrets.
	WatchWebhookSecrets bool
}

// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state. For one
// FeedGroup it resolves the webhook (webhook), builds the timed clients
// (clientsFor), plans which feeds are due (planFeeds), fetches them
// concurrently (fetchAll), folds each feed's outcome through processFeed, and
// writes status and the next requeue (requeueWithStatus).
func (r *FeedGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("feedgroup", req.NamespacedName)
	ctx = logf.IntoContext(ctx, log)

	reconcileStart := time.Now()

	var feedGroup v1alpha1.FeedGroup
	if err := r.Get(ctx, req.NamespacedName, &feedGroup); err != nil {
		if apierrors.IsNotFound(err) {
			// The FeedGroup is gone; drop its metric series so a deleted
			// group can't leave a stale feedLastSuccessTimestamp behind.
			groupMetrics{namespace: req.Namespace, name: req.Name}.forget()
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get FeedGroup")
		return ctrl.Result{}, err
	}

	// Timed from here, not from the top of Reconcile, so a deleted FeedGroup
	// never re-creates its just-dropped feedGroupReconcileDuration series via
	// a deferred Observe running after groupMetrics.forget above.
	defer func() {
		groupMetrics{namespace: req.Namespace, name: req.Name}.reconcileDuration(time.Since(reconcileStart))
	}()

	// Prune the metric series of feeds dropped from the spec, so editing a
	// group's feed list doesn't leave stale series behind until the whole
	// FeedGroup is deleted.
	for _, url := range ensureFeedStatuses(&feedGroup) {
		metricsFor(&feedGroup).forgetFeed(url)
	}

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

	webhookURL, err := r.webhook(ctx, &feedGroup)
	if err != nil {
		return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, retryRequeue(&feedGroup.Spec), err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rssClient, discordClient := r.clientsFor(&feedGroup, webhookURL)

	// An unparsable Interval is reported by requeueWithStatus below; the plan
	// only uses it for the due check.
	plan := planFeeds(&feedGroup, time.Now().UTC(), specChanged)
	activeFeeds := plan.active

	fetched := fetchAll(ctx, rssClient, &feedGroup, activeFeeds)

	var verdict attemptVerdict
	for i, feed := range activeFeeds {
		// All feeds in a group share one webhook, so once any feed hits
		// Discord's per-webhook rate limit, processing the rest would only
		// fire more sends that 429 too. Stop here and let the requeue (which
		// backs off for verdict.retryAfter below) retry the remaining feeds.
		if verdict.retryAfter > 0 {
			break
		}
		verdict = verdict.fold(r.processFeed(ctx, &feedGroup, feed, fetched[i].result, fetched[i].err, discordClient, now))
	}

	decision := plan.requeue(&feedGroup.Spec, verdict.retry, verdict.retryAfter)
	return r.requeueWithStatus(ctx, &feedGroup, statusSnapshot, decision, nil)
}

// processFeed handles a completed fetch for a single feed: it records the
// fetch outcome, sends new entries that pass the filter, and updates the feed's
// FeedStatus. now is an RFC3339 status timestamp. Fetch, configuration,
// rendering, and delivery errors are recorded in status by the feed's
// feedAttempt, which also decides whether the failure is worth retrying.
// Applying the fetch to the FeedStatus -- the order of the steps and what is
// settled -- belongs to the feedSync opened below, which is closed with a
// verdict on every return. The returned verdict says whether the reconcile
// should retry sooner than the normal interval and, if Discord rate limited the
// webhook, how long to wait before the next attempt.
func (r *FeedGroupReconciler) processFeed(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	feed v1alpha1.FeedSpec,
	fetchResult rss.FetchResult,
	fetchErr error,
	discordClient MessageSender,
	now string,
) attemptVerdict {
	log := logf.FromContext(ctx)
	fs := feedStatusFor(feedGroup, feed.RSSUrl)
	attempt := r.newFeedAttempt(feedGroup, fs, feed.RSSUrl, now)

	// Every return below sets settlement first; the deferred close is what
	// settles the FeedStatus, and rejects a return that forgot to. A panic
	// before settlement is set passes through unchanged (see closeOnReturn).
	syncer := openFeedSync(fs, fetchResult, feedSyncSpecFor(feedGroup, now))
	var settlement syncVerdict
	defer syncer.closeOnReturn(&settlement)

	if fetchErr != nil {
		settlement = syncFetchFailed
		// LastChecked deliberately doesn't advance here: it tracks the last
		// *successful* check, so a feed stuck failing shows an aging
		// LastChecked rather than looking freshly checked every retry.
		log.Error(fetchErr, "failed to fetch RSS feed", "url", feed.RSSUrl)
		return attempt.record(attemptOutcome{kind: attemptFetchFailed}, classifyFetchError(fetchErr), fetchErr)
	}

	// A 304 is just as much a successful check as one that found something to
	// send -- it shouldn't read as stale just because nothing changed. This does
	// give up the optimization of skipping the status write when nothing
	// else changed (requeueWithStatus diffs on the whole status), so a
	// quiet feed now costs one status write per poll interval instead of
	// zero.
	attempt.record(attemptOutcome{kind: attemptFetchSucceeded}, noClass, nil)

	if v, done := syncer.withoutEntries(); done {
		settlement = v
		return attemptVerdict{}
	}

	// Every config check happens in one step, before the sync begins, so a
	// config error on a non-empty first fetch can't turn the next attempt into
	// a "feed was empty" delivery of its whole backlog.
	renderer, err := newFeedRenderer(feedGroup, &feed)
	if err != nil {
		settlement = syncConfigError
		log.Error(err, "invalid feed render configuration", "url", feed.RSSUrl)
		return attempt.record(attemptOutcome{kind: attemptConfigError}, noClass, err)
	}

	syncer.begin()
	if syncer.cappedAmbiguousLegacy {
		log.Info("Capped catch-up for a FeedStatus written before stateVersion 1",
			"url", feed.RSSUrl, "limit", defaultCatchUpLimit)
	}
	if syncer.skippedBacklog > 0 {
		attempt.record(attemptOutcome{kind: attemptBacklogSkipped, count: syncer.skippedBacklog}, noClass, nil)
	}

	verdict := entryDelivery{
		renderer: renderer,
		syncer:   syncer,
		attempt:  attempt,
		sender:   discordClient,
		metrics:  metricsFor(feedGroup),
	}.run(ctx)
	settlement = verdict.settlement()
	return verdict
}

// webhook returns the group's Discord webhook URL, read uncached from its
// Secret. It owns every outcome of that lookup: a failed Get or a missing key
// is an error; the Secret's resourceVersion is then recorded with
// noteWebhookSecretVersion (so a replaced Secret is noticed even when its new
// value is unusable); and a blank value is an error. Errors are logged here and
// returned for Reconcile to surface through requeueWithStatus.
func (r *FeedGroupReconciler) webhook(ctx context.Context, feedGroup *v1alpha1.FeedGroup) (string, error) {
	log := logf.FromContext(ctx)
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      feedGroup.Spec.DiscordWebhookSecretRef.Name,
		Namespace: feedGroup.Namespace,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		log.Error(err, "failed to resolve Discord webhook URL")
		return "", err
	}

	value, ok := secret.Data[feedGroup.Spec.DiscordWebhookSecretRef.Key]
	if !ok {
		err := fmt.Errorf("secret %s missing key %q", secretKey.Name, feedGroup.Spec.DiscordWebhookSecretRef.Key)
		log.Error(err, "failed to resolve Discord webhook URL")
		return "", err
	}
	noteWebhookSecretVersion(feedGroup, secret.ResourceVersion)

	webhookURL := strings.TrimSpace(string(value))
	if webhookURL == "" {
		err := fmt.Errorf("discord webhook URL is empty")
		log.Error(err, "invalid webhook secret")
		return "", err
	}
	return webhookURL, nil
}

// clientsFor returns the feed fetcher and message sender for one reconcile,
// defaulting r.RSSClient / r.DiscordClientBuilder to the real clients. Both are
// wrapped in the timing adapters, which are the only place feedRequestDuration
// is observed -- not the fetch goroutines or entryDelivery.
func (r *FeedGroupReconciler) clientsFor(feedGroup *v1alpha1.FeedGroup, webhookURL string) (FeedFetcher, MessageSender) {
	fetcher := r.RSSClient
	if fetcher == nil {
		fetcher = rss.NewClient(nil)
	}
	var sender MessageSender
	if r.DiscordClientBuilder != nil {
		sender = r.DiscordClientBuilder(webhookURL)
	} else {
		sender = discord.NewClient(webhookURL)
	}
	return newTimedFetcher(fetcher, feedGroup.Namespace, feedGroup.Name),
		newTimedSender(sender, feedGroup.Namespace, feedGroup.Name)
}

// fetchOutcome is one feed's fetch: the result and the error FetchEntries
// returned for it.
type fetchOutcome struct {
	result rss.FetchResult
	err    error
}

// fetchAll fetches every feed concurrently and returns the outcomes in the
// same order as feeds. Feeds are independent network fetches, so this avoids
// paying each fetch timeout sequentially; a semaphore caps how many run at
// once (maxConcurrentFetches), since a FeedGroup at the Feeds.MaxItems limit
// would otherwise open that many simultaneous outbound connections.
//
// Invariant: feedGroup.Status is only read here (to look up each feed's cache
// validators), concurrently with itself, and is never written until fetchAll
// returns -- wg.Wait() is the barrier. Everything after it (sending, status
// updates) is single-threaded, so no locking is needed. Do not mutate the
// FeedGroup from the fetch goroutines.
func fetchAll(ctx context.Context, f FeedFetcher, feedGroup *v1alpha1.FeedGroup, feeds []v1alpha1.FeedSpec) []fetchOutcome {
	outcomes := make([]fetchOutcome, len(feeds))
	sem := make(chan struct{}, maxConcurrentFetches)
	var wg sync.WaitGroup
	for i, feed := range feeds {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			fs := feedStatusFor(feedGroup, feed.RSSUrl)
			validators := rss.CacheValidators{
				ETag:         fs.ETag,
				LastModified: fs.LastModified,
			}
			outcomes[i].result, outcomes[i].err = f.FetchEntries(ctx, feed.RSSUrl, validators)
		})
	}
	wg.Wait()
	return outcomes
}

// SetupWithManager sets up the controller with the Manager.
func (r *FeedGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("feedgroup-controller")
	}
	b := ctrl.NewControllerManagedBy(mgr).
		// Status is written by this controller, so reacting to those writes
		// would re-enqueue every reconcile that changed anything (and, for a
		// failing feed whose RetryCount ticks up each pass, loop at
		// API-round-trip speed instead of RetryInterval). Only spec changes
		// (generation bumps), creates and deletes matter; periodic work is
		// driven by RequeueAfter.
		For(&v1alpha1.FeedGroup{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if r.WatchWebhookSecrets {
		// A replaced webhook Secret lets a feed backed off after a permanent
		// send failure retry right away (see noteWebhookSecretVersion).
		// Metadata only: the informer never holds Secret data, and the
		// webhook URL itself is still read uncached in webhook.
		b = b.Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.feedGroupsForSecret),
			builder.OnlyMetadata, builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}))
	}
	return b.
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
