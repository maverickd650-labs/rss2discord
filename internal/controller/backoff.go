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
	"fmt"
	"time"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maxPermanentBackoff is the ceiling for per-feed exponential backoff on
// permanent fetch failures (e.g. HTTP 404). Once the computed backoff would
// exceed this, the feed keeps being retried at this cadence indefinitely:
// "permanent" classifications (a 403 from a WAF blip, a 404 during a site
// migration) are often only long-lived, and a few requests a day is a cheap
// price for a feed that recovers on its own without a spec edit.
const maxPermanentBackoff = 6 * time.Hour

// retryState is the phase-independent view of a feed's retry count and
// permanent backoff. It is the one place that reads, writes or clears
// FeedStatus.RetryCount and FeedStatus.BackoffUntil and performs the retry-limit
// and backoff calculations. It is a view over the status, not a copy, so
// changes land directly on FeedStatus. Its operations never inspect Reachable.
type retryState struct {
	fs *v1beta1.FeedStatus
}

// retryStateOf returns the phase-independent retry state for fs.
func retryStateOf(fs *v1beta1.FeedStatus) retryState { return retryState{fs: fs} }

// A retry run is the consecutive failures of one phase of a feed's sync:
// fetching (Reachable False) or delivery -- renders and sends (Reachable True,
// Delivered False). A successful fetch ends a fetch-phase run (fetchSucceeded),
// resetting RetryCount and clearing the backoff, so the first render or send
// failure after a recovered outage is the first of its own run: it is not
// already past the retry limit (which would skip the entry without retries of
// its own and skip the exhaustion Event, fired only at ==) and a permanent send
// failure backs off from 1, not from the fetch failures' count. A successful
// fetch leaves a delivery-phase run alone: every retry of a failing entry is
// preceded by a successful fetch, so resetting there would stop the run ever
// exhausting. The reverse holds too: a fetch failure while Reachable is True
// (the run in progress is a delivery one) clears that run first and starts a
// fetch run at 1 (fail), so delivery failures don't exhaust a fetch run's
// retries or inflate its backoff. The backoff is driven by RetryCount, which
// the failure being recorded has already incremented.
//
// retryRun is a short-lived, phase-aware view for one transition. Its phase is
// captured from Reachable by runOf and is deliberately not refreshed if that
// condition changes. Construct it when a phase-sensitive transition begins;
// do not retain it across independent operations or reconciles. Use
// retryStateOf for operations that do not need the phase.
type retryRun struct {
	retryState
	phase retryPhase
}

// runOf returns the retry run of fs, capturing the phase represented by its
// current Reachable condition. The returned value must not outlive the single
// phase-sensitive transition for which it was created.
func runOf(fs *v1beta1.FeedStatus) retryRun {
	phase := phaseUnknown
	switch {
	case apimeta.IsStatusConditionFalse(fs.Conditions, v1beta1.FeedConditionTypeReachable):
		phase = phaseFetch
	case apimeta.IsStatusConditionTrue(fs.Conditions, v1beta1.FeedConditionTypeReachable):
		phase = phaseDelivery
	}
	return retryRun{retryState: retryStateOf(fs), phase: phase}
}

// retryPhase is the phase represented by an existing run. Unknown is possible
// only when the feed's Reachable condition has not yet been recorded.
type retryPhase uint8

const (
	phaseUnknown retryPhase = iota
	// phaseFetch is fetching and parsing the feed (the Reachable condition).
	phaseFetch
	// phaseDelivery is rendering and sending an entry (the Delivered condition).
	phaseDelivery
)

// failurePhase is the phase of a failure being recorded. Unlike retryPhase,
// it has no unknown value, so phaseUnknown cannot be passed to retryRun.fail,
// and its zero value is invalid rather than silently meaning delivery.
type failurePhase uint8

const (
	failureFetch failurePhase = iota + 1
	failureDelivery
)

// set reports whether a backoff is recorded at all, expired or not: an
// expired backoff still belongs to the run of failures that set it.
func (r retryState) set() bool { return r.fs.BackoffUntil != nil && !r.fs.BackoffUntil.IsZero() }

// inProgress reports whether a run has left any state: a counted failure or a
// recorded backoff.
func (r retryState) inProgress() bool { return r.fs.RetryCount > 0 || r.set() }

// outstanding reports whether failures of the current run are still counted.
// It is narrower than inProgress (a backoff alone does not count) and is what
// the scheduler's failedOrRetrying reads.
func (r retryState) outstanding() bool { return r.fs.RetryCount > 0 }

// fetchSucceeded ends the run if the phase captured by runOf was fetching.
// A delivery-phase or unknown run is left alone.
func (r retryRun) fetchSucceeded() {
	if r.phase == phaseFetch && r.inProgress() {
		r.clear()
	}
}

// fail counts one failure of phase in the run, first ending an in-progress run
// when the captured phase is the other one. A fetch failure ends a delivery
// run; a delivery failure never ends a fetch run. Apart from ending the other
// phase's run (which clears that run's backoff), it does not set BackoffUntil;
// permanent failures follow it with extend.
func (r retryRun) fail(failure failurePhase) {
	switch failure {
	case failureFetch:
		if r.phase == phaseDelivery && r.inProgress() {
			r.clear()
		}
	case failureDelivery:
	default:
		panic(fmt.Sprintf("retryRun: unknown failure phase %d", failure))
	}
	r.fs.RetryCount++
}

// belowLimit reports whether the run still has retries left: RetryCount is
// under max.
func (r retryState) belowLimit(max int32) bool { return r.fs.RetryCount < max }

// exhaustedNow reports whether the failure just counted is the one that
// reaches max (== rather than >=, so an ongoing failure announces it once).
func (r retryState) exhaustedNow(max int32) bool { return r.fs.RetryCount == max }

// pastLimit reports whether RetryCount went past max, not just up to it: the
// retries were already exhausted a full reconcile ago.
func (r retryState) pastLimit(max int32) bool { return r.fs.RetryCount > max }

// active reports whether the backoff is still in the future as of now. A
// nil or zero BackoffUntil is treated as "not in backoff" so an unset value
// can never permanently wedge a feed without a spec change.
func (r retryState) active(now time.Time) bool {
	if r.fs.BackoffUntil == nil || r.fs.BackoffUntil.IsZero() {
		return false
	}
	return now.Before(r.fs.BackoffUntil.Time)
}

// extend sets BackoffUntil to now plus the exponential backoff for the current
// RetryCount (base * 2^RetryCount, capped at maxPermanentBackoff; see
// permanentBackoffDuration). It reports firstCapped: whether this is the first
// failure whose backoff reaches the cap (the previous failure's backoff, at
// RetryCount-1, was still below it). That is true once per run of failures, so
// the caller can fire its Event exactly once rather than on every capped
// retry. RetryCount returns to 0 when the run is cleared, which re-arms it.
func (r retryState) extend(base time.Duration, now time.Time) (firstCapped bool) {
	count := int(r.fs.RetryCount)
	backoff := permanentBackoffDuration(count, base)
	if backoff >= maxPermanentBackoff {
		firstCapped = count <= 1 || permanentBackoffDuration(count-1, base) < maxPermanentBackoff
		backoff = maxPermanentBackoff
	}
	r.fs.BackoffUntil = nextBackoffUntil(now, backoff)
	return firstCapped
}

// clear ends the run: it ends the backoff and resets RetryCount with it.
// Leaving RetryCount at its old value would make the next extend compute
// base*2^oldRetryCount, which jumps straight back to maxPermanentBackoff on the
// first retry. LastError is not touched (see clearFailure).
func (r retryState) clear() {
	r.fs.BackoffUntil = nil
	r.fs.RetryCount = 0
}

// sendCaused reports whether the backoff represented by this retryRun came
// from a delivery failure. It uses the same phase snapshot as fail and
// fetchSucceeded, so a later Reachable update cannot reclassify the backoff.
func (r retryRun) sendCaused() bool {
	return r.set() && r.phase != phaseFetch
}

// nextBackoffUntil returns the *metav1.Time for a feed's next permanent
// backoff expiry, backoff duration past now.
func nextBackoffUntil(now time.Time, backoff time.Duration) *metav1.Time {
	t := metav1.NewTime(now.Add(backoff))
	return &t
}

// permanentBackoffDuration returns the exponential backoff for a permanent
// fetch failure at retryCount (1-based, already incremented before call).
// The formula is base * 2^retryCount, capped at maxPermanentBackoff, with
// retryCount floored at 1. A non-positive base returns maxPermanentBackoff.
func permanentBackoffDuration(retryCount int, base time.Duration) time.Duration {
	retryCount = max(retryCount, 1)
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

// clearPermanentBackoffs clears the backoff (and RetryCount, see
// retryState.clear) of every feed in the group. Called when the FeedGroup spec
// generation advances, so an edit gives feeds in backoff an immediate fresh
// attempt.
func clearPermanentBackoffs(feedGroup *v1beta1.FeedGroup) {
	for i := range feedGroup.Status.Feeds {
		retryStateOf(&feedGroup.Status.Feeds[i]).clear()
	}
}
