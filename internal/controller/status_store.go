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
	"slices"
	"unicode/utf8"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	acv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1/applyconfiguration/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// max must be at least the ellipsis's three-byte UTF-8 length.
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

// ensureFeedStatuses rebuilds feedGroup.Status.Feeds to have exactly one
// entry per feed in feedGroup.Spec.Feeds, in spec order: existing entries are
// matched up by RSSUrl, new feeds get an entry with their RSSUrl, and entries
// for feeds no longer in the spec are dropped. That one pass both initializes
// status on a fresh FeedGroup and prunes the stale entries an edited or removed
// feed leaves behind (the same unbounded-growth problem maxLastSentPerFeed
// guards against one level down), so this is the only place that needs to know
// about every per-feed status field. Paused feeds still get an entry, since
// they're still part of the spec. Rebuilding in spec order also keeps
// Status.Feeds' order stable across reconciles whenever the spec doesn't
// change, which apiequality.Semantic.DeepEqual (used by settleGroup to skip
// no-op writes) needs to avoid seeing a reordered-but-otherwise-identical slice
// as a change. Retained entries have nil LastSent maps initialized, LastError
// bounded, and oversized LastSeenEntry values replaced by clampWatermark
// digests. It returns the RSSUrls it pruned so the caller can drop their
// metric series; it does not touch Prometheus.
func ensureFeedStatuses(feedGroup *v1alpha1.FeedGroup) (pruned []string) {
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

	// Whatever is left in existing was dropped from the spec.
	for url := range existing {
		pruned = append(pruned, url)
	}
	slices.Sort(pruned)
	return pruned
}

// feedStatusFor returns a pointer to the FeedStatus for url, or nil if absent
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
// Status conversion errors and errors from the API write are returned unchanged.
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
// JSON encoding or decoding errors return nil and a wrapped error.
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
