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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
)

// attemptKind is what happened in one step of processing a feed. Every kind
// is applied by feedAttempt.record and nowhere else.
type attemptKind int

const (
	// attemptFetchSucceeded: the feed was fetched (or a 304 confirmed it).
	attemptFetchSucceeded attemptKind = iota + 1
	// attemptFetchFailed: the fetch failed; classifyFetchError says how.
	attemptFetchFailed
	// attemptRenderFailed: an entry's message could not be rendered.
	attemptRenderFailed
	// attemptSendFailed: Discord refused or never received a message;
	// classifySendError says how. A *discord.RateLimitError is recorded as
	// attemptRateLimited instead.
	attemptSendFailed
	// attemptRateLimited: Discord rate limited the webhook (a
	// *discord.RateLimitError), which spends no retry. Callers report such a
	// send as attemptSendFailed; record resolves it.
	attemptRateLimited
	// attemptSent: an entry was delivered.
	attemptSent
	// attemptEntrySkipped: an entry whose render retries are exhausted was
	// given up on so the feed behind it can proceed.
	attemptEntrySkipped
	// attemptBacklogSkipped: a first sync with catchUpLimit 0 skipped the
	// feed's existing entries.
	attemptBacklogSkipped
	// attemptConfigError: the feed's filter or templates don't compile.
	attemptConfigError

	// attemptKindEnd is one past the last kind, for tests that enumerate them.
	attemptKindEnd
)

// attemptOutcome is a kind with the detail that kind alone carries.
type attemptOutcome struct {
	kind attemptKind
	// entry is the title of the entry given up on (attemptEntrySkipped).
	entry string
	// count is how many existing entries were skipped (attemptBacklogSkipped).
	count int
}

// metricLabel is the outcome label the outcome increments
// feedOperationsTotal with, or "" for an outcome that isn't counted. At most
// one label per outcome; fetch_error and send_error are only ever recorded
// with their class's metricReason suffix.
func (o attemptOutcome) metricLabel(class failureClass) string {
	switch o.kind {
	case attemptFetchFailed:
		return fetchErrorOutcome(class)
	case attemptSendFailed:
		return sendErrorOutcome(class)
	case attemptRenderFailed:
		return outcomeRenderError
	case attemptRateLimited:
		return outcomeRateLimited
	case attemptSent:
		return outcomeSent
	case attemptEntrySkipped:
		return outcomeSkipped
	}
	return ""
}

// resolve settles what a reported failure is, from its kind and err alone, so
// callers only say what went wrong, never how it classifies. A fetch failure is
// classified by classifyFetchError; a send failure is a rate limit (which the
// webhook's Retry-After drives, not the retry count) when err holds a
// *discord.RateLimitError, and otherwise a send failure classified by
// classifySendError. A render failure carries a class of its own and never
// backs off; every other kind has none.
func (o attemptOutcome) resolve(err error) (attemptOutcome, failureClass) {
	switch o.kind {
	case attemptFetchFailed:
		return o, classifyFetchError(err)
	case attemptSendFailed:
		if _, ok := errors.AsType[*discord.RateLimitError](err); ok {
			return attemptOutcome{kind: attemptRateLimited}, classRateLimited
		}
		return o, classifySendError(err)
	case attemptRenderFailed:
		return o, failureClass{conditionReason: reasonRenderError}
	}
	return o, failureClass{}
}

// attemptVerdict is what recording an outcome means for the rest of the
// reconcile. The zero value is "nothing to do".
type attemptVerdict struct {
	// retry asks for the group to be requeued after its (faster) RetryInterval
	// instead of its Interval.
	retry bool
	// retryAfter is how long Discord asked the webhook to be left alone.
	retryAfter time.Duration
	// pending says an entry from this fetch is still unresolved, even if its
	// retries are exhausted or it is in permanent backoff, so the fetch is not
	// accounted for (see settlement).
	pending bool
}

// fold combines the verdicts of two steps (the entries of a feed, or the feeds
// of a group): any retry, the longest rate-limit wait, any pending.
func (v attemptVerdict) fold(other attemptVerdict) attemptVerdict {
	return attemptVerdict{
		retry:      v.retry || other.retry,
		retryAfter: max(v.retryAfter, other.retryAfter),
		pending:    v.pending || other.pending,
	}
}

// settlement is the syncVerdict for a feedSync whose send loop ended with v.
func (v attemptVerdict) settlement() syncVerdict {
	if v.pending {
		return syncPending
	}
	return syncAccountedFor
}

// feedAttempt records the outcomes of processing one feed in one reconcile. It
// is the single owner of what an outcome does to the world: the outcome metric,
// the Reachable or Delivered condition, LastError, RetryCount, permanent backoff,
// the persistent-failure Events and the retry decision. The policy -- a
// permanent failure backs off immediately, a transient one retries until its
// retries run out and is then announced once -- is written once, in record.
type feedAttempt struct {
	group *v1alpha1.FeedGroup
	fs    *v1alpha1.FeedStatus
	url   string
	// now is the reconcile's clock reading, for LastChecked.
	now time.Time
	// clock is the run's clock (feedRun.clock): the success gauge and the
	// backoff deadline read it when the outcome is recorded. nil is time.Now.
	clock func() time.Time
	// cadence is the group's retry limit and backoff base.
	cadence cadence
	// recorder receives the attempt's Warning Events; nil drops them.
	recorder events.EventRecorder
}

// recordedAt is when the attempt's outcome is being recorded: the run's clock,
// or the wall clock when it has none.
func (a feedAttempt) recordedAt() time.Time {
	if a.clock == nil {
		return time.Now()
	}
	return a.clock()
}

// newAttempt opens the attempt for the run's feed, whose status is fs.
func (run feedRun) newAttempt(fs *v1alpha1.FeedStatus) feedAttempt {
	return feedAttempt{
		group: run.group, fs: fs, url: run.feed.RSSUrl, now: run.now, clock: run.clock,
		cadence: run.cadence, recorder: run.recorder,
	}
}

// record applies one outcome to the feed's status and says what the reconcile
// should do next.
//
// err is the failure itself, or the cause of a skip or config error; record
// classifies it (see resolve), so a send failure is reported as
// attemptSendFailed and becomes attemptRateLimited when Discord limited the
// webhook. The effects, per kind:
//
//   - attemptFetchSucceeded: LastChecked, the success gauge, Reachable True;
//     and if the feed was in a fetch-phase retry run, that run ends (see below).
//   - attemptFetchFailed / attemptSendFailed: LastError, RetryCount++, the
//     outcome metric, Reachable / Delivered False with the class's reason, then
//     the retry policy below. Not pending for a fetch; pending for a send.
//   - attemptRenderFailed: the same with outcome render_error and a transient
//     policy; pending.
//   - attemptRateLimited: LastError, outcome rate_limited, Delivered False
//     RateLimited; retry, with the wait Discord asked for; pending. No retry is
//     spent.
//   - attemptSent: failure cleared, outcome sent, Delivered True.
//   - attemptEntrySkipped: failure cleared, outcome skipped, Delivered False
//     EntrySkipped and a Warning Event.
//   - attemptBacklogSkipped: Delivered True BacklogSkipped.
//   - attemptConfigError: LastError, Delivered False ConfigError and one Warning
//     Event per distinct error. No metric and no retry: it won't fix itself.
//
// Retry runs (RetryCount, BackoffUntil, and which phase a run belongs to) are
// owned by retryRun (backoff.go); record only calls it, in an order its doc
// comment explains: before the condition that would hide the phase is set.
// LastError is kept when a fetch-phase run ends: it is cleared only where a
// fetch is settled as accounted for or an entry is sent or skipped
// (clearFailure), and until then it keeps a feed that recovered but has not yet
// delivered counted as failing (countFailingFeeds); the next delivery failure
// overwrites it, and settlement clears it.
//
// The retry policy: a permanent failure backs off exponentially right away
// (BackoffUntil; one Warning Event, the first time the backoff reaches its cap)
// and never asks for a retry, so it doesn't pull the group onto the faster
// RetryInterval. A transient one asks for a retry while RetryCount is below the
// limit; at the reconcile RetryCount first equals it (== rather than >=, so an
// ongoing failure doesn't re-fire) one Warning Event announces the exhaustion.
func (a feedAttempt) record(o attemptOutcome, err error) attemptVerdict {
	o, class := o.resolve(err)
	if label := o.metricLabel(class); label != "" {
		metricsFor(a.group).operation(a.url, label)
	}

	switch o.kind {
	case attemptFetchSucceeded:
		runOf(a.fs).fetchSucceeded() // before Reachable flips: it reads the phase from it
		a.fs.LastChecked = a.now.Format(time.RFC3339)
		metricsFor(a.group).checkSucceeded(a.recordedAt())
		a.setCondition(v1alpha1.FeedConditionTypeReachable, metav1.ConditionTrue,
			reasonFetchSucceeded, "Feed fetched successfully")
		return attemptVerdict{}

	case attemptFetchFailed:
		return a.failed(phaseFetch, class, eventReasonFetchFailed, err)

	case attemptRenderFailed:
		verdict := a.failed(phaseDelivery, class, eventReasonRenderFailed, err)
		verdict.pending = true
		return verdict

	case attemptSendFailed:
		verdict := a.failed(phaseDelivery, class, eventReasonSendFailed, err)
		verdict.pending = true
		return verdict

	case attemptRateLimited:
		a.fs.LastError = errText(err)
		a.setCondition(v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse, reasonRateLimited, err.Error())
		verdict := attemptVerdict{retry: true, pending: true}
		if rateLimitErr, ok := errors.AsType[*discord.RateLimitError](err); ok {
			verdict.retryAfter = rateLimitErr.RetryAfter
		}
		return verdict

	case attemptSent:
		clearFailure(a.fs)
		a.setCondition(v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue,
			reasonMessageSent, "Entry rendered and delivered to Discord")
		return attemptVerdict{}

	case attemptEntrySkipped:
		// Surfaced via a Warning Event, a metric outcome and the Delivered
		// condition rather than LastError, which would otherwise mark the feed
		// failing forever.
		const reason = eventReasonRenderFailed
		clearFailure(a.fs)
		a.setCondition(v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse, reasonEntrySkipped,
			fmt.Sprintf("skipped entry %q after %s: %v", clampText(o.entry, 100), reason, err))
		a.warn(reasonEntrySkipped, eventActionSkippedEntry, "feed %s: skipped entry %q after exhausting retries (%s): %v",
			a.url, clampText(o.entry, 100), reason, err)
		return attemptVerdict{}

	case attemptBacklogSkipped:
		a.setCondition(v1alpha1.FeedConditionTypeDelivered, metav1.ConditionTrue, reasonBacklogSkipped,
			fmt.Sprintf("catchUpLimit is 0: skipped %d existing entries", o.count))
		return attemptVerdict{}

	case attemptConfigError:
		// A deterministic FeedSpec misconfiguration carries no RetryCount to
		// gate a one-time Event on. setCondition's changed return serves the
		// same purpose instead: it is true only the reconcile the Delivered
		// condition first flips to ConfigError (or its message changes, e.g. a
		// different typo), not on every reconcile of the same unresolved error.
		a.fs.LastError = errText(err)
		if a.setCondition(v1alpha1.FeedConditionTypeDelivered, metav1.ConditionFalse, reasonConfigError, err.Error()) {
			a.warn(reasonConfigError, eventActionInvalidConfig, "feed %s: %v", a.url, err)
		}
		return attemptVerdict{}
	}
	panic(fmt.Sprintf("feedAttempt: unknown outcome kind %d", o.kind))
}

// failed records one counted failure -- LastError (errText, which is clamped),
// the failure counted in the retry run (retryRun.fail, which starts a new run
// when the one in progress is the other phase's) and the phase's condition
// (Reachable for a fetch, Delivered otherwise; carrying the full err.Error())
// -- and applies the retry policy to it. eventReason names the failure in the
// Events.
func (a feedAttempt) failed(phase retryPhase, class failureClass, eventReason string, err error) attemptVerdict {
	a.fs.LastError = errText(err)
	runOf(a.fs).fail(phase) // before the condition: it reads the old Reachable
	condType := v1alpha1.FeedConditionTypeDelivered
	if phase == phaseFetch {
		condType = v1alpha1.FeedConditionTypeReachable
	}
	a.setCondition(condType, metav1.ConditionFalse, class.conditionReason, err.Error())

	if class.permanent {
		// Permanent failures (404, 410, bad XML, a deleted webhook, a malformed
		// message Discord keeps rejecting) won't self-heal on the normal retry
		// schedule, so back off exponentially to reduce polling instead of
		// using the flat RetryInterval. The group's normal Interval drives other
		// feeds; this feed handles its own timing via BackoffUntil, checked in
		// the active-feeds filter. No retry is asked for, so a single permanent
		// failure doesn't pull the whole group onto the faster RetryInterval.
		a.backOff(eventReason, err)
		return attemptVerdict{}
	}

	run, maxRetries := runOf(a.fs), a.cadence.maxRetries
	if run.belowLimit(maxRetries) {
		return attemptVerdict{retry: true}
	}
	// Fire the persistent-failure Event exactly once, the reconcile this
	// feed's retries are first exhausted -- not on every subsequent reconcile
	// of a feed stuck failing the same way. Without the == here (rather than
	// >=), RetryCount keeps climbing past maxRetries forever and a single
	// ongoing failure would generate a fresh Warning Event every poll interval,
	// indefinitely.
	if run.exhaustedNow(maxRetries) {
		a.warn(eventReason, eventActionRetriesExhausted, "feed %s: giving up after exhausting retries: %v", a.url, err)
	}
	return attemptVerdict{}
}

// pastRetryLimit reports whether the failure just recorded came after the
// retries were already exhausted a full reconcile ago: RetryCount went past
// the limit, not just up to it. The count is the current phase's run (see
// retryRun), so fetch failures never count against an entry's retries.
func (a feedAttempt) pastRetryLimit() bool {
	return runOf(a.fs).pastLimit(a.cadence.maxRetries)
}

// backOff extends fs's permanent backoff (retryRun.extend, based on
// the run's count, which failed has already incremented for this failure). reason and err
// identify the Warning Event fired the first time the cap is reached -- not on
// every capped retry after, so a feed that keeps failing at the cap doesn't
// emit an Event every few hours. Unlike the exhaustion Event it doesn't speak
// of exhausted retries: a permanent failure never spends them, and the feed
// keeps being retried, waiting at most the cap between attempts (or longer if
// the group's Interval is longer).
func (a feedAttempt) backOff(reason string, err error) {
	if runOf(a.fs).extend(a.cadence.retry, a.recordedAt().UTC()) {
		a.warn(reason, eventActionBackoffCapped, "feed %s: still failing permanently, backoff now capped at %s: %v",
			a.url, maxPermanentBackoff, err)
	}
}

// setCondition is setFeedCondition for the attempt's feed and generation. It
// returns whether the condition was added or changed.
func (a feedAttempt) setCondition(condType string, status metav1.ConditionStatus, reason, message string) bool {
	return setFeedCondition(a.fs, condType, status, reason, message, a.group.Generation)
}

// warn emits a Warning Event on the FeedGroup, so a failure is visible via
// `kubectl describe feedgroup` instead of requiring a controller log dive. It
// is a no-op if Recorder is unset (e.g. in unit tests that construct the
// reconciler directly rather than through SetupWithManager).
func (a feedAttempt) warn(reason, action, format string, args ...any) {
	if a.recorder == nil {
		return
	}
	a.recorder.Eventf(a.group, nil, corev1.EventTypeWarning, reason, action, format, args...)
}

// clearFailure resets fs's retry state: its last error, its retry count and
// any permanent backoff. A sent or skipped entry clears it through record; a
// feedSync does when the fetch it applied is accounted for.
func clearFailure(fs *v1alpha1.FeedStatus) {
	fs.LastError = ""
	runOf(fs).clear()
}
