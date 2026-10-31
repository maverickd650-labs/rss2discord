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
	"errors"
	"fmt"
	"runtime/debug"
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

	// defaultsOnce guards withDefaults.
	defaultsOnce sync.Once
}

// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rss2discord.maverickd650.dev,resources=feedgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state. For one
// FeedGroup it normalizes status (prepareStatus), resolves the webhook
// (webhook), builds the timed clients (clientsFor), runs the pure middle
// (reconcilePass: planFeeds, fetchAll, the processFeed fold), and writes status
// and the next requeue (settleGroup). The Kubernetes I/O stays here.
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
	prep := prepareStatus(&feedGroup)
	for _, url := range prep.pruned {
		metricsFor(&feedGroup).forgetFeed(url)
	}
	statusSnapshot := prep.snapshot

	cad := cadenceOf(&feedGroup.Spec)

	webhookURL, err := r.webhook(ctx, &feedGroup)
	if err != nil {
		return r.settleGroup(ctx, &feedGroup, statusSnapshot, cad, retryRequeue(cad), err)
	}

	rssClient, discordClient := r.clientsFor(&feedGroup, webhookURL)

	// An unparsable Interval is reported by settleGroup below; the pass only
	// uses the cadence's default for the due check.
	verdict, plan, panicked := containedPass(ctx, &feedGroup, rssClient, discordClient, r.Recorder, cad, time.Now().UTC(), prep.specChanged)
	if panicked != nil {
		// Write what the pass got done, then fail the reconcile as the panic
		// would have: controller-runtime logs it and requeues with backoff.
		// The pass stopped at the panicking feed, so the feeds after it are
		// not observed at this generation: ObservedGeneration stays put, and
		// a spec change still forces them due on the retry.
		if _, err := r.settle(ctx, &feedGroup, statusSnapshot, cad, retryRequeue(cad), panicked, false); err != nil {
			return ctrl.Result{}, errors.Join(panicked, err)
		}
		return ctrl.Result{}, panicked
	}

	decision := plan.requeue(cad, verdict.retry, verdict.retryAfter)
	return r.settleGroup(ctx, &feedGroup, statusSnapshot, cad, decision, nil)
}

// statusPrep is the group's status as the reconcile starts from it.
type statusPrep struct {
	// snapshot is the status after normalization but before anything that
	// reflects this reconcile's outcome, so settleGroup can skip the API write
	// entirely when nothing actually changed.
	snapshot *v1alpha1.FeedGroupStatus
	// specChanged is whether the spec changed since the last reconcile
	// (generation bump), which forces every feed due.
	specChanged bool
	// pruned are the RSSUrls of feeds dropped from the spec, whose metric
	// series the caller should drop.
	pruned []string
}

// prepareStatus normalizes feedGroup.Status to its spec (ensureFeedStatuses),
// and on a spec change lets a feed in permanent backoff retry right away
// instead of waiting out its BackoffUntil: it clears BackoffUntil and
// RetryCount for every feed so the new generation gets a fresh retry cycle;
// without resetting RetryCount the first retry after clearing BackoffUntil
// would compute a backoff of base*2^oldRetryCount, which would immediately jump
// back to maxPermanentBackoff. The snapshot is taken after both (they only
// normalize existing state) and before the webhook lookup or any feed work.
func prepareStatus(feedGroup *v1alpha1.FeedGroup) statusPrep {
	prep := statusPrep{pruned: ensureFeedStatuses(feedGroup)}
	prep.specChanged = feedGroup.Generation > feedGroup.Status.ObservedGeneration
	if prep.specChanged {
		clearPermanentBackoffs(feedGroup)
	}
	prep.snapshot = feedGroup.Status.DeepCopy()
	return prep
}

// reconcilePass is the pure middle of a reconcile, once the webhook is
// resolved: plan which feeds are due (planFeeds), fetch them concurrently
// (fetchAll), and fold each through processFeed. It does no Kubernetes I/O, so
// tests drive it with fakes of the ports. force (a spec change) makes every
// non-backed-off feed due. The returned plan carries the wake time of feeds
// skipped as not yet due, and the verdict is the fold of the feeds' verdicts.
func reconcilePass(
	ctx context.Context,
	feedGroup *v1alpha1.FeedGroup,
	fetcher FeedFetcher,
	sender MessageSender,
	recorder events.EventRecorder,
	cad cadence,
	now time.Time,
	force bool,
) (attemptVerdict, feedPlan) {
	plan := planFeeds(feedGroup, cad, now, force)
	activeFeeds := plan.active

	fetched := fetchAll(ctx, fetcher, feedGroup, activeFeeds)

	run := feedRun{
		group: feedGroup, sender: sender, cadence: cad, recorder: recorder,
		now: now,
	}
	var verdict attemptVerdict
	for i, feed := range activeFeeds {
		// All feeds in a group share one webhook, so once any feed hits
		// Discord's per-webhook rate limit, processing the rest would only
		// fire more sends that 429 too. Stop here and let the requeue (which
		// backs off for verdict.retryAfter) retry the remaining feeds.
		if verdict.retryAfter > 0 {
			break
		}
		run.feed, run.fetched = feed, fetched[i]
		verdict = verdict.fold(processFeed(ctx, run))
	}
	return verdict, plan
}

// feedRun is everything processFeed needs to handle one feed's completed fetch
// in one reconcile, so a caller (Reconcile, or a test) builds one value instead
// of passing a reconciler and a string of positional arguments.
type feedRun struct {
	group *v1alpha1.FeedGroup
	feed  v1alpha1.FeedSpec
	// fetched is the feed's fetch: the result and the error FetchEntries
	// returned for it.
	fetched fetchOutcome
	// sender delivers the feed's messages to the group's webhook.
	sender MessageSender
	// cadence is the group's resolved timing (cadenceOf), computed once per
	// reconcile.
	cadence cadence
	// recorder receives the Warning Events; nil drops them.
	recorder events.EventRecorder
	// now is the reconcile's clock reading; status timestamps are formatted
	// from it where they are written.
	now time.Time
	// clock reads the time a failure or success is recorded at, for the
	// permanent-failure backoff deadline and the success gauge: those must
	// count from when the attempt happened, not from the reconcile's start, or
	// a slow fetch or a rate-limit wait would eat into the backoff. nil is
	// time.Now; tests pin it (withNow) so their assertions are exact.
	clock func() time.Time
}

// processFeed handles a completed fetch for a single feed: it records the
// fetch outcome, sends new entries that pass the filter, and updates the feed's
// FeedStatus. Fetch, configuration, rendering, and delivery errors are recorded
// in status by the feed's feedAttempt, which also decides whether the failure is
// worth retrying. The whole sequence -- its order, what is settled and the
// syncVerdict it ends with -- belongs to the feedSync opened below and run by
// feedSync.apply. The returned verdict says whether the reconcile should retry
// sooner than the normal interval and, if Discord rate limited the webhook, how
// long to wait before the next attempt.
//
// A panic while the sync is applied is not settled normally, but it is not
// left to discard the feed's progress either: entries already posted to
// Discord would be posted again on the next reconcile, and again on every
// retry if the panic is deterministic. processFeed secures the feed's status
// (feedSync.secureAfterPanic: restored to how it was when nothing was handled
// yet, settled as pending once something was) and re-raises the panic as a
// *feedPanic, which Reconcile recovers to write the group's status before
// failing the reconcile.
func processFeed(ctx context.Context, run feedRun) attemptVerdict {
	fs := feedStatusFor(run.group, run.feed.RSSUrl)
	before := fs.DeepCopy()
	syncer := openFeedSync(fs, run.fetched.result, feedSyncSpecFor(run.group, run.now))
	attempt := run.newAttempt(fs)
	defer func() {
		if p := recover(); p != nil {
			syncer.secureAfterPanic(before, attempt, fmt.Errorf("%w: %v", errDeliveryPanicked, p))
			panic(&feedPanic{url: run.feed.RSSUrl, value: p, stack: debug.Stack()})
		}
	}()
	return syncer.apply(ctx, run, attempt)
}

// errDeliveryPanicked is the failure recorded for a feed whose delivery a
// panic interrupted after it had handled an entry (see
// feedSync.secureAfterPanic), so the feed reads as failing and is retried on
// the reconcile's error backoff rather than its next poll.
var errDeliveryPanicked = errors.New("delivery interrupted by a panic")

// feedPanic is a panic raised while processFeed applied one feed's fetch,
// re-raised once that feed's status was secured (see processFeed). It is the
// only panic Reconcile recovers: any other one may have left the status
// half-written, so it propagates unchanged and nothing is written.
type feedPanic struct {
	url   string
	value any
	stack []byte
}

func (p *feedPanic) Error() string {
	return p.summary() + "\n" + string(p.stack)
}

// summary is the panic without its stack, for the Ready condition.
func (p *feedPanic) summary() string {
	return fmt.Sprintf("panic while processing feed %s: %v", p.url, p.value)
}

// containedPass is reconcilePass with a *feedPanic recovered and returned
// rather than propagated, so Reconcile can still write the group's status
// (the feeds processed before it, and the panicking feed as secured).
func containedPass(ctx context.Context, feedGroup *v1alpha1.FeedGroup, fetcher FeedFetcher, sender MessageSender,
	recorder events.EventRecorder, cad cadence, now time.Time, force bool,
) (verdict attemptVerdict, plan feedPlan, panicked *feedPanic) {
	defer func() {
		if p := recover(); p != nil {
			fp, ok := p.(*feedPanic)
			if !ok {
				panic(p)
			}
			panicked = fp
		}
	}()
	verdict, plan = reconcilePass(ctx, feedGroup, fetcher, sender, recorder, cad, now, force)
	return verdict, plan, nil
}

// webhook returns the group's Discord webhook URL, read uncached from its
// Secret. It owns every outcome of that lookup: a failed Get or a missing key
// is an error; the Secret's resourceVersion is then recorded with
// noteWebhookSecretVersion (so a replaced Secret is noticed even when its new
// value is unusable); and a blank value is an error. Errors are logged here and
// returned for Reconcile to surface through settleGroup.
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

// withDefaults fills a nil RSSClient and DiscordClientBuilder with the real
// clients, once per reconciler (SetupWithManager calls it; a reconciler built
// as a literal gets it on its first reconcile). The defaulted builder shares one
// HTTP client and one discord.RateLimiter across every webhook it builds a
// sender for, so a 429 seen by one FeedGroup holds the others on that webhook,
// as cmd/main.go's explicit builder does.
func (r *FeedGroupReconciler) withDefaults() {
	r.defaultsOnce.Do(func() {
		if r.RSSClient == nil {
			r.RSSClient = rss.NewClient(nil)
		}
		if r.DiscordClientBuilder == nil {
			httpClient := discord.NewHTTPClient()
			limiter := discord.NewRateLimiter()
			r.DiscordClientBuilder = func(webhookURL string) MessageSender {
				return discord.NewClient(webhookURL, discord.WithHTTPClient(httpClient), discord.WithRateLimiter(limiter))
			}
		}
	})
}

// clientsFor returns the feed fetcher and message sender for one reconcile,
// wrapped in the timing adapters, which are the only place feedRequestDuration
// is observed -- not the fetch goroutines or entryDelivery.
func (r *FeedGroupReconciler) clientsFor(feedGroup *v1alpha1.FeedGroup, webhookURL string) (FeedFetcher, MessageSender) {
	r.withDefaults()
	return newTimedFetcher(r.RSSClient, feedGroup.Namespace, feedGroup.Name),
		newTimedSender(r.DiscordClientBuilder(webhookURL), feedGroup.Namespace, feedGroup.Name)
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
	r.withDefaults()
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
