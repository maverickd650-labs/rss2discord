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
	"time"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
)

// maxPermanentBackoff is the ceiling for per-feed exponential backoff on
// permanent fetch failures (e.g. HTTP 404). Once the computed backoff would
// exceed this, the feed keeps being retried at this cadence indefinitely:
// "permanent" classifications (a 403 from a WAF blip, a 404 during a site
// migration) are often only long-lived, and a few requests a day is a cheap
// price for a feed that recovers on its own without a spec edit.
const maxPermanentBackoff = 6 * time.Hour

// retryRun is the retry run of one feed: the consecutive failures of one phase
// of its sync, counted by FeedStatus.RetryCount and, for permanent failures,
// paced by FeedStatus.BackoffUntil. It is the one place that reads, writes or
// clears those two fields, does the backoff math, and encodes which phase a run
// belongs to. It is a view over the status, not a copy, so changes land on the
// FeedStatus. feedAttempt.record is its only production caller on the failure
// path; it keeps the metric, condition, Event and verdict.
//
// A run belongs to one phase: fetching (Reachable False) or delivery -- renders
// and sends (Reachable True, Delivered False). A successful fetch ends a
// fetch-phase run (fetchSucceeded), resetting RetryCount and clearing the
// backoff, so the first render or send failure after a recovered outage is the
// first of its own run: it is not already past the retry limit (which would
// skip the entry without retries of its own and skip the exhaustion Event,
// fired only at ==) and a permanent send failure backs off from 1, not from
// the fetch failures' count. A successful fetch leaves a delivery-phase run
// alone: every retry of a failing entry is preceded by a successful fetch, so
// resetting there would stop the run ever exhausting. The reverse holds too: a
// fetch failure while Reachable is True (the run in progress is a delivery one)
// clears that run first and starts a fetch run at 1 (fail), so delivery
// failures don't exhaust a fetch run's retries or inflate its backoff.
//
// The phase is not stored: it is read from the Reachable condition, which the
// status already persists. That makes the order of calls matter. fetchSucceeded
// and fail(phaseFetch) must run before the caller sets Reachable (True and
// False respectively), because the condition they read is the old run's; record
// calls them first, ahead of its setCondition. Every other reader of the phase
// (sendCaused) lives here too, so the rule has one home. The backoff is driven
// by RetryCount, which the failure being recorded has already incremented.
type retryRun struct {
	fs *v1alpha1.FeedStatus
}

// runOf returns the retry run of fs.
func runOf(fs *v1alpha1.FeedStatus) retryRun { return retryRun{fs: fs} }

// retryPhase is the part of a feed's sync a failure belongs to.
type retryPhase int

const (
	// phaseFetch is fetching and parsing the feed (the Reachable condition).
	phaseFetch retryPhase = iota
	// phaseDelivery is rendering and sending an entry (the Delivered condition).
	phaseDelivery
)

// set reports whether a backoff is recorded at all, expired or not: an
// expired backoff still belongs to the run of failures that set it.
func (r retryRun) set() bool { return r.fs.BackoffUntil != "" }

// inProgress reports whether a run has left any state: a counted failure or a
// recorded backoff.
func (r retryRun) inProgress() bool { return r.fs.RetryCount > 0 || r.set() }

// outstanding reports whether failures of the current run are still counted.
// It is narrower than inProgress (a backoff alone does not count) and is what
// the scheduler's failedOrRetrying reads.
func (r retryRun) outstanding() bool { return r.fs.RetryCount > 0 }

// fetchSucceeded ends the run if it is a fetch-phase one (Reachable False with
// state left over). Call it before setting Reachable True, which would erase
// the evidence of the phase. A delivery-phase run is left alone.
func (r retryRun) fetchSucceeded() {
	if apimeta.IsStatusConditionFalse(r.fs.Conditions, v1alpha1.FeedConditionTypeReachable) && r.inProgress() {
		r.clear()
	}
}

// fail counts one failure of phase in the run, first ending the run in
// progress when it belongs to the other phase: a fetch failure while Reachable
// is True means the run is a delivery one. Call it before setting the
// condition for this failure. Apart from ending the other phase's run (which
// clears that run's backoff), it does not set BackoffUntil; permanent failures
// follow it with extend.
func (r retryRun) fail(phase retryPhase) {
	if phase == phaseFetch &&
		apimeta.IsStatusConditionTrue(r.fs.Conditions, v1alpha1.FeedConditionTypeReachable) &&
		r.inProgress() {
		r.clear()
	}
	r.fs.RetryCount++
}

// belowLimit reports whether the run still has retries left: RetryCount is
// under max.
func (r retryRun) belowLimit(max int32) bool { return r.fs.RetryCount < max }

// exhaustedNow reports whether the failure just counted is the one that
// reaches max (== rather than >=, so an ongoing failure announces it once).
func (r retryRun) exhaustedNow(max int32) bool { return r.fs.RetryCount == max }

// pastLimit reports whether RetryCount went past max, not just up to it: the
// retries were already exhausted a full reconcile ago.
func (r retryRun) pastLimit(max int32) bool { return r.fs.RetryCount > max }

// active reports whether the backoff is still in the future as of now. A
// malformed or empty BackoffUntil is treated as "not in backoff" so a bad value
// can never permanently wedge a feed without a spec change.
func (r retryRun) active(now time.Time) bool {
	if r.fs.BackoffUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, r.fs.BackoffUntil)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// extend sets BackoffUntil to now plus the exponential backoff for the current
// RetryCount (base * 2^RetryCount, capped at maxPermanentBackoff; see
// permanentBackoffDuration). It reports firstCapped: whether this is the first
// failure whose backoff reaches the cap (the previous failure's backoff, at
// RetryCount-1, was still below it). That is true once per run of failures, so
// the caller can fire its Event exactly once rather than on every capped
// retry. RetryCount returns to 0 when the run is cleared, which re-arms it.
func (r retryRun) extend(base time.Duration, now time.Time) (firstCapped bool) {
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
func (r retryRun) clear() {
	r.fs.BackoffUntil = ""
	r.fs.RetryCount = 0
}

// sendCaused reports whether the feed is backed off because delivery to its
// webhook failed, as opposed to its own URL failing to fetch. It is inferred
// from the Reachable condition: a fetch failure sets Reachable False, whereas a
// send or render failure leaves it True (the fetch succeeded), and a later
// successful fetch sets it True again (ending a fetch-phase run and its
// backoff, see fetchSucceeded). So a backoff with Reachable not False is a
// send backoff, which replacing the webhook may fix. This must stay in step
// with where extend is called (feedAttempt.failed): the fetch path sets
// Reachable, the send path sets Delivered.
func (r retryRun) sendCaused() bool {
	return r.set() &&
		!apimeta.IsStatusConditionFalse(r.fs.Conditions, v1alpha1.FeedConditionTypeReachable)
}

// nextBackoffUntil formats the RFC3339 timestamp for a feed's next permanent
// backoff expiry, backoff duration past now.
func nextBackoffUntil(now time.Time, backoff time.Duration) string {
	return now.Add(backoff).Format(time.RFC3339)
}

// permanentBackoffDuration returns the exponential backoff for a permanent
// fetch failure at retryCount (1-based, already incremented before call).
// The formula is base * 2^retryCount, capped at maxPermanentBackoff, with
// retryCount floored at 1. A non-positive base returns maxPermanentBackoff.
func permanentBackoffDuration(retryCount int, base time.Duration) time.Duration {
	if retryCount < 1 {
		retryCount = 1
	}
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
// retryRun.clear) of every feed in the group. Called when the FeedGroup spec
// generation advances, so an edit gives feeds in backoff an immediate fresh
// attempt.
func clearPermanentBackoffs(feedGroup *v1alpha1.FeedGroup) {
	for i := range feedGroup.Status.Feeds {
		runOf(&feedGroup.Status.Feeds[i]).clear()
	}
}
