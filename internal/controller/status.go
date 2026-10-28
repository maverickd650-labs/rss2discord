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
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	acv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1/applyconfiguration/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// markFeedCheckSuccess records that a feed was successfully checked this
// reconcile, whether or not it had anything new to send. It's the single
// place that advances feedLastSuccessTimestamp, so a quiet-but-healthy feed
// (304, or zero new entries) reads as fresh rather than as if the operator
// had stopped checking it.
func markFeedCheckSuccess(feedGroup *v1alpha1.FeedGroup) {
	feedLastSuccessTimestamp.WithLabelValues(feedGroup.Namespace, feedGroup.Name).
		Set(float64(time.Now().Unix()))
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
