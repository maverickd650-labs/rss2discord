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

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// groupSync owns a FeedGroup's status lifecycle across one Reconcile:
// normalizing Status.Feeds to Spec.Feeds, pruning dropped-feed metrics,
// resetting permanent backoffs on a spec change, snapshotting pre-reconcile
// status to skip no-op API writes, summarizing Ready/FeedReachable conditions,
// advancing ObservedGeneration when appropriate, and writing via SSA.
type groupSync struct {
	reconciler  *FeedGroupReconciler
	group       *v1beta1.FeedGroup
	original    *v1beta1.FeedGroupStatus
	cadence     cadence
	specChanged bool
}

// openGroupSync begins the FeedGroup's status lifecycle for one Reconcile:
// normalizing Status.Feeds to Spec.Feeds (ensureFeedStatuses), pruning metrics
// for dropped feeds, resetting permanent backoffs on a spec change so the new
// generation gets a fresh retry cycle, snapshotting the pre-reconcile status to
// detect no-op writes, and resolving the group's cadence.
func (r *FeedGroupReconciler) openGroupSync(feedGroup *v1beta1.FeedGroup) *groupSync {
	for _, url := range ensureFeedStatuses(feedGroup) {
		metricsFor(feedGroup).forgetFeed(url)
	}
	specChanged := feedGroup.Generation > feedGroup.Status.ObservedGeneration
	if specChanged {
		clearPermanentBackoffs(feedGroup)
	}
	return &groupSync{
		reconciler:  r,
		group:       feedGroup,
		original:    feedGroup.Status.DeepCopy(),
		cadence:     cadenceOf(&feedGroup.Spec),
		specChanged: specChanged,
	}
}

// settlePass settles a completed pass across the group: it calculates the
// requeue delay from the plan and verdict, advances ObservedGeneration,
// summarizes the Ready and FeedReachable conditions, syncs gauges, and writes
// the updated status via SSA when changed.
func (g *groupSync) settlePass(ctx context.Context, plan feedPlan, verdict attemptVerdict) (ctrl.Result, error) {
	decision := plan.requeue(g.cadence, verdict.retry, verdict.retryAfter)
	return g.settle(ctx, decision, nil, true)
}

// settleWebhookError settles a reconcile that failed before processing feeds
// because the webhook Secret could not be resolved. It requeues using the
// retry cadence, surfaces the error on the Ready condition, advances
// ObservedGeneration, syncs gauges, and writes the updated status via SSA when
// changed.
func (g *groupSync) settleWebhookError(ctx context.Context, err error) (ctrl.Result, error) {
	return g.settle(ctx, retryRequeue(g.cadence), err, true)
}

// settlePanic settles a reconcile interrupted by a feed panic. It requeues
// using the retry cadence and surfaces the panic on the Ready condition without
// advancing ObservedGeneration, so feeds after the panicking one remain due on
// the retry. It returns only an error so Reconcile can return
// errors.Join(panicked, g.settlePanic(ctx, panicked)).
func (g *groupSync) settlePanic(ctx context.Context, panicked *feedPanic) error {
	_, err := g.settle(ctx, retryRequeue(g.cadence), panicked, false)
	return err
}

// settleSuspend settles a reconcile for a suspended FeedGroup: it sets Ready=Unknown
// with reason Suspended, advances ObservedGeneration, refreshes feed metrics (aligning
// with paused-feed behavior), and writes the status via SSA when changed. It does not
// schedule a periodic requeue.
func (g *groupSync) settleSuspend(ctx context.Context) (ctrl.Result, error) {
	g.group.Status.ObservedGeneration = g.group.Generation
	setSemanticCondition(&g.group.Status.Conditions, metav1.Condition{
		Type:               v1beta1.ConditionTypeReady,
		Status:             metav1.ConditionUnknown,
		Reason:             reasonSuspended,
		Message:            "FeedGroup reconciliation is suspended",
		ObservedGeneration: g.group.Generation,
	})

	metricsFor(g.group).syncFeedState(g.group)

	if apiequality.Semantic.DeepEqual(g.original, &g.group.Status) {
		return ctrl.Result{}, nil
	}

	if err := g.reconciler.applyStatus(ctx, g.group); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// settle persists feedGroup's status and requeues after decision.wait() (the
// decision's delay, jittered and bounded; see requeueDecision). The status
// write (applyStatus) is skipped when the status hasn't actually changed from
// original, avoiding an API round trip for unchanged status. groupErr, if
// non-nil, means the reconcile did not get through the group: the webhook
// Secret couldn't be resolved, so no feed was processed, or a feed panicked
// (a *feedPanic), so the ones after it weren't. It takes over the Ready
// condition unconditionally, since per-feed status is stale or partial and
// would otherwise misreport as healthy.
//
// observed controls whether ObservedGeneration advances to match Generation:
// true for successful passes and webhook resolution errors, false for panics
// so the next reconcile still sees a changed spec and forces every feed due.
//
// An Interval/RetryInterval that time.ParseDuration rejects (the CRD Pattern
// admits overflowing values) must never abort this function: by now messages
// may already have been sent, and returning before the status write would
// discard LastSent/LastSeenEntry/LastChecked, so every error-backoff retry
// would re-send the same entries. The cadence has already substituted the
// default, so the requeue uses it, the problem is surfaced on Ready (see
// setReadyCondition) plus one Warning Event on the transition. Duration parse
// errors are not returned; status conversion or API write errors from
// applyStatus are returned with an empty result.
//
// decision.jitter controls whether requeueJitterMaxFactor is applied to the
// computed duration. It must be false when the fallback is itself an
// externally-dictated wait -- currently only Discord's rate-limit Retry-After --
// since that duration is already the exact minimum wait Discord demands, not a
// self-chosen periodicity that benefits from desynchronization.
func (g *groupSync) settle(ctx context.Context, decision requeueDecision, groupErr error, observed bool) (ctrl.Result, error) {
	// A parse error in the cadence already fell back to the default; it is
	// reported via g.cadence.err below, for both fields at once.
	duration := decision.wait()

	if observed {
		g.group.Status.ObservedGeneration = g.group.Generation
	}
	if warning, emit := summarizeGroup(g.group, g.cadence, groupErr); emit && g.reconciler.Recorder != nil {
		g.reconciler.Recorder.Eventf(g.group, nil, corev1.EventTypeWarning, reasonInvalidInterval, eventActionInvalidConfig, "%s", warning)
	}

	// Before the no-op short-circuit below so the per-feed gauges are refreshed
	// on every reconcile, not just ones that changed status.
	metricsFor(g.group).syncFeedState(g.group)

	if apiequality.Semantic.DeepEqual(g.original, &g.group.Status) {
		return ctrl.Result{RequeueAfter: duration}, nil
	}

	if err := g.reconciler.applyStatus(ctx, g.group); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: duration}, nil
}
