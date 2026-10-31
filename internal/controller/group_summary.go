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
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// summarizeGroup sets the group-level Ready and FeedReachable conditions from
// the group's spec and per-feed status, and decides whether the change warrants
// a Warning Event. It does no I/O, so settleGroup only has to emit the
// Event it reports.
//
// The Event is gated on Ready's reason/message actually changing (as
// feedAttempt.record does for a feed's config error), not on every reconcile
// that still sees the bad value; ObservedGeneration is deliberately not
// compared, so an unrelated spec edit doesn't repeat it. Only the
// InvalidInterval reason produces one: warning is that condition's message and
// emit is true exactly on the transition into it (or a change of its message).
// groupErr outranks the interval problem on Ready, so while it is set no
// Event is emitted here.
func summarizeGroup(feedGroup *v1alpha1.FeedGroup, cad cadence, groupErr error) (warning string, emit bool) {
	prevReason, prevMessage := "", ""
	if prev := apimeta.FindStatusCondition(feedGroup.Status.Conditions, v1alpha1.ConditionTypeReady); prev != nil {
		prevReason, prevMessage = prev.Reason, prev.Message
	}

	setReadyCondition(feedGroup, groupErr, cad.err)
	setFeedReachableCondition(feedGroup)

	ready := apimeta.FindStatusCondition(feedGroup.Status.Conditions, v1alpha1.ConditionTypeReady)
	if ready != nil && ready.Reason == reasonInvalidInterval &&
		(ready.Reason != prevReason || ready.Message != prevMessage) {
		return ready.Message, true
	}
	return "", false
}

// setReadyCondition summarizes the outcome of a reconciliation into a single
// "Ready" condition, leaving Status.Feeds (LastError, Conditions, etc.) as
// the detailed source of truth for *which* feed and *why*. groupErr, if
// set, is a group-level failure that preempts any per-feed status: the
// webhook Secret couldn't be resolved, so no feed was even attempted
// (WebhookUnresolved), or a feed panicked, so the pass stopped there
// (ReconcilePanicked, a *feedPanic; the message omits the stack).
//
// intervalErr, if set, means spec.interval or spec.retryInterval can't be
// parsed (see cadence.err) and the controller is polling on its
// default cadence instead. It ranks below groupErr -- with no feed
// processed, the webhook failure is the more fundamental problem and its
// retry cadence is the one that matters -- but above the per-feed summary:
// the spec is wrong no matter how the feeds fared, and until it's fixed the
// group is not running on the schedule the user asked for. The InvalidInterval
// condition clears on its own the reconcile after the spec is corrected.
func setReadyCondition(feedGroup *v1alpha1.FeedGroup, groupErr, intervalErr error) {
	if groupErr != nil {
		// A pass a feed panicked in did not get through the group, whatever
		// the feeds' own statuses say (one restored after the panic carries
		// no failure); a webhook that can't be resolved stops it before it
		// starts.
		reason, message := reasonWebhookUnresolved, groupErr.Error()
		if fp, ok := errors.AsType[*feedPanic](groupErr); ok {
			reason, message = reasonReconcilePanicked, fp.summary()
		}
		apimeta.SetStatusCondition(&feedGroup.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            clampText(message, maxConditionMessageBytes),
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
	reason := reasonReconciled
	message := "All feeds processed successfully"

	failing := countFailingFeeds(feedGroup.Status.Feeds)
	if failing > 0 {
		status = metav1.ConditionFalse
		reason = reasonFeedErrors
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

// countFailingFeeds reports how many feeds' last check failed (checkOf.failed).
func countFailingFeeds(feeds []v1alpha1.FeedStatus) int {
	count := 0
	for i := range feeds {
		if checkOf(&feeds[i]).failed() {
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
// Missing or Unknown Reachable conditions are not counted as failures.
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
			Reason:             reasonAllFeedsReachable,
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
// across reconciles of the same status, which settleGroup relies on to
// skip no-op status writes.
// reasons must be non-empty.
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
