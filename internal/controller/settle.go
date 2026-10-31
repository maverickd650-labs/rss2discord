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

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
)

// settleGroup persists feedGroup's status and requeues after
// decision.wait() (the decision's delay, jittered and bounded; see
// requeueDecision). The
// status write (applyStatus) is skipped when the status hasn't actually
// changed from original, avoiding an API round trip for unchanged status.
// webhookErr, if non-nil, means the reconcile never got far enough to
// process any feed (the webhook Secret couldn't be resolved); it takes over
// the Ready condition unconditionally, since per-feed status is stale/empty
// and would otherwise misreport as healthy.
//
// An Interval/RetryInterval that time.ParseDuration rejects (the CRD Pattern
// admits overflowing values) must never abort this function: by now messages
// may already have been sent, and returning before the status write would
// discard LastSent/LastSeenEntry/LastChecked, so every error-backoff retry
// would re-send the same entries. The cadence has already substituted the default, so the requeue uses it, the
// problem is surfaced on Ready (see setReadyCondition) plus one Warning Event
// on the transition. Duration parse errors are not returned; status conversion
// or API write errors from applyStatus are returned with an empty result.
//
// decision.jitter controls whether requeueJitterMaxFactor is applied to the
// computed duration. It must be false when the fallback is itself an
// externally-dictated wait -- currently only Discord's rate-limit Retry-After -- since that
// duration is already the exact minimum wait Discord demands, not a
// self-chosen periodicity that benefits from desynchronization.
func (r *FeedGroupReconciler) settleGroup(ctx context.Context, feedGroup *v1alpha1.FeedGroup, original *v1alpha1.FeedGroupStatus, cad cadence, decision requeueDecision, webhookErr error) (ctrl.Result, error) {
	return r.settle(ctx, feedGroup, original, cad, decision, webhookErr, true)
}

// settle is settleGroup, with observed false for a pass that did not get
// through the group (a feed panicked): ObservedGeneration is then left as it
// was, so the next reconcile still sees a changed spec and forces every feed
// due.
func (r *FeedGroupReconciler) settle(ctx context.Context, feedGroup *v1alpha1.FeedGroup, original *v1alpha1.FeedGroupStatus, cad cadence, decision requeueDecision, webhookErr error, observed bool) (ctrl.Result, error) {
	// A parse error in the cadence already fell back to the default; it is
	// reported via cad.err below, for both fields at once.
	duration := decision.wait()

	if observed {
		feedGroup.Status.ObservedGeneration = feedGroup.Generation
	}
	if warning, emit := summarizeGroup(feedGroup, cad, webhookErr); emit && r.Recorder != nil {
		r.Recorder.Eventf(feedGroup, nil, corev1.EventTypeWarning, reasonInvalidInterval, eventActionInvalidConfig, "%s", warning)
	}

	// Before the no-op short-circuit below so the per-feed gauges are refreshed
	// on every reconcile, not just ones that changed status.
	metricsFor(feedGroup).syncFeedState(feedGroup)

	if apiequality.Semantic.DeepEqual(original, &feedGroup.Status) {
		return ctrl.Result{RequeueAfter: duration}, nil
	}

	if err := r.applyStatus(ctx, feedGroup); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: duration}, nil
}
